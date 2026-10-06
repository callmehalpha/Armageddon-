package lifecycle

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Uninstaller runs `server uninstall [--purge]` (contract §9.3).
type Uninstaller struct {
	Layout Layout
	// Systemctl runs systemctl (tests substitute a recorder).
	Systemctl func(args ...string) error
	// DeleteUser removes an OS user (default: userdel).
	DeleteUser func(name string) error
	// Passwd is the user database to find workspace users in.
	Passwd string
	Log    io.Writer
}

func (u *Uninstaller) logf(format string, args ...any) {
	if u.Log != nil {
		fmt.Fprintf(u.Log, format+"\n", args...)
	}
}

// PurgePhrase is what the operator must type to confirm --purge.
func PurgePhrase(dataDir string) string { return "delete " + dataDir }

// ErrNotConfirmed is returned when --purge was not confirmed.
var ErrNotConfirmed = errors.New("purge not confirmed; nothing was removed")

// Confirm reads one line from in and checks it is the purge phrase.
func Confirm(in io.Reader, out io.Writer, dataDir string) bool {
	fmt.Fprintf(out, "This permanently deletes every workspace, checkpoint and key in %s and the workspace users.\n"+
		"Make a backup first (`armageddon server backup --to <dir>`).\nType %q to continue: ", dataDir, PurgePhrase(dataDir))
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line) == PurgePhrase(dataDir)
}

var wsUser = regexp.MustCompile(`^ws-[a-z0-9]{8,24}$`)

// Run removes services and binaries. With purge (already confirmed by the
// caller) it also removes the data directory, the configuration and the
// armageddon and ws-* users. Missing pieces are skipped, so it can be
// re-run.
func (u *Uninstaller) Run(purge, confirmed bool) error {
	if purge && !confirmed {
		return ErrNotConfirmed
	}
	l := u.Layout
	var units []string
	for _, n := range []string{ServerUnit, HelperUnit} {
		if _, err := os.Stat(filepath.Join(l.UnitDir(), n)); err == nil {
			units = append(units, n)
		}
	}
	if len(units) > 0 && u.Systemctl != nil {
		if err := u.Systemctl(append([]string{"disable", "--now"}, units...)...); err != nil {
			u.logf("WARNING: %v", err)
		}
	}
	for _, n := range units {
		if err := os.Remove(filepath.Join(l.UnitDir(), n)); err != nil {
			return err
		}
		u.logf("removed %s", filepath.Join(l.UnitDir(), n))
	}
	if len(units) > 0 && u.Systemctl != nil {
		u.Systemctl("daemon-reload")
	}
	// The command symlink, only if it points into our layout.
	if t, err := os.Readlink(l.BinLink()); err == nil && (strings.HasPrefix(t, "/opt/armageddon/") || strings.HasPrefix(t, l.Opt())) {
		os.Remove(l.BinLink())
		u.logf("removed %s", l.BinLink())
	}
	if err := os.RemoveAll(l.Opt()); err != nil {
		return err
	}
	u.logf("removed %s", l.Opt())
	if !purge {
		u.logf("kept the data in %s and the configuration in %s (use --purge to delete them)", l.DataDir, l.Etc())
		return nil
	}
	if l.DataDir == "" || l.DataDir == "/" {
		return fmt.Errorf("refusing to purge data directory %q", l.DataDir)
	}
	if err := os.RemoveAll(l.DataDir); err != nil {
		return err
	}
	u.logf("removed %s", l.DataDir)
	os.RemoveAll(l.Etc())
	u.logf("removed %s", l.Etc())
	if u.DeleteUser != nil {
		users := []string{}
		if b, err := os.ReadFile(u.Passwd); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if name, _, ok := strings.Cut(line, ":"); ok && wsUser.MatchString(name) {
					users = append(users, name)
				}
			}
		}
		users = append(users, "armageddon")
		for _, n := range users {
			if err := u.DeleteUser(n); err == nil {
				u.logf("removed user %s", n)
			}
		}
	}
	return nil
}
