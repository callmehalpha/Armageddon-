// Package sysuser is the MVP stand-in for the privileged helper (contract
// §2.5). It exposes one narrow capability, "run this program as the
// workspace's OS user", plus user creation and ownership. When the server is
// not root, isolation is unavailable and everything runs as the current user
// (development mode; the server logs a warning).
//
// The split into a separate root helper with an unprivileged server is plan
// item M3.1. Keeping every privileged action behind this package keeps that
// split mechanical.
package sysuser

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Account struct {
	Name     string
	UID, GID uint32
	Home     string
	isolated bool
}

// Isolated reports whether workspace processes run as separate OS users.
func Isolated() bool { return os.Geteuid() == 0 }

var validName = regexp.MustCompile(`^ws-[a-z0-9]{8,24}$`)

// NameFor derives the OS user name for a workspace ID.
func NameFor(workspaceID string) string {
	id := strings.ToLower(workspaceID)
	if len(id) > 14 {
		id = id[len(id)-14:]
	}
	return "ws-" + id
}

// Ensure returns the account for a workspace, creating the OS user if needed.
func Ensure(name, home string) (*Account, error) {
	if !Isolated() {
		u, err := user.Current()
		if err != nil {
			return nil, err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		return &Account{Name: u.Username, UID: uint32(uid), GID: uint32(gid), Home: home}, nil
	}
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("refusing invalid workspace user name %q", name)
	}
	if _, err := user.Lookup(name); err != nil {
		cmd := exec.Command("useradd", "--system", "--user-group", "--no-create-home",
			"--home-dir", home, "--shell", "/bin/bash", name)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("useradd %s: %v: %s", name, err, out)
		}
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &Account{Name: name, UID: uint32(uid), GID: uint32(gid), Home: home, isolated: true}, nil
}

// Delete removes a workspace OS user (isolated mode only).
func Delete(name string) error {
	if !Isolated() || !validName.MatchString(name) {
		return nil
	}
	return exec.Command("userdel", name).Run()
}

// Prepare configures cmd to run as the account, with a clean environment
// (no inherited GIT_* or server secrets) plus extra variables.
func (a *Account) Prepare(cmd *exec.Cmd, extraEnv ...string) {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + a.Home,
		"USER=" + a.Name,
		"LOGNAME=" + a.Name,
		"LANG=C.UTF-8",
	}
	cmd.Env = append(env, extraEnv...)
	if a.isolated {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: []uint32{}},
			Setsid:     cmd.SysProcAttr != nil && cmd.SysProcAttr.Setsid,
		}
	}
}

// Command builds a command that runs as the account.
func (a *Account) Command(dir string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	a.Prepare(cmd)
	return cmd
}

// Chown gives the account ownership of each path, recursively.
func (a *Account) Chown(paths ...string) error {
	if !a.isolated {
		return nil
	}
	for _, p := range paths {
		err := filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(path, int(a.UID), int(a.GID))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// KillAll stops every process running as the workspace user: SIGTERM, then
// SIGKILL for whatever is left after grace. It implements decision Q1
// ("development processes stop when a device takes the workspace"). Docker
// services are not the workspace user's processes and keep running.
//
// In development mode (not root) the account is the server's own user, so
// nothing is signalled and 0 is returned.
//
// MOVES TO THE HELPER: with the privilege split (Phase 2) this becomes the
// root helper's SignalWorkspace operation; the server must not keep a
// kill capability of its own.
func (a *Account) KillAll(grace time.Duration) int {
	if !a.isolated || a.UID == 0 {
		return 0
	}
	pids := a.processes()
	for _, p := range pids {
		syscall.Kill(p, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && len(a.processes()) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range a.processes() {
		syscall.Kill(p, syscall.SIGKILL)
	}
	return len(pids)
}

// processes lists the PIDs whose effective owner is the account (Linux
// /proc; the server is Linux-only).
func (a *Account) processes() []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		fi, err := os.Stat(filepath.Join("/proc", e.Name()))
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == a.UID {
			out = append(out, pid)
		}
	}
	return out
}

// MkdirOwned creates a directory owned by the account with the given mode.
func (a *Account) MkdirOwned(p string, mode os.FileMode) error {
	if err := os.MkdirAll(p, mode); err != nil {
		return err
	}
	if err := os.Chmod(p, mode); err != nil {
		return err
	}
	if a.isolated {
		return os.Lchown(p, int(a.UID), int(a.GID))
	}
	return nil
}
