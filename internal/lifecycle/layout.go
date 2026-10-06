// Package lifecycle implements the server lifecycle commands of contract
// §9.3 that act on the installed layout: update, rollback and uninstall.
//
// The layout (contract §9.2):
//
//	/opt/armageddon/versions/<v>/armageddon   immutable per version
//	/opt/armageddon/current → versions/<v>
//	/usr/local/bin/armageddon → /opt/armageddon/current/armageddon
//	/etc/armageddon/
//	/etc/systemd/system/armageddon{,-helper}.service
//
// Every path can be put under a prefix, so tests (and `install.sh
// --prefix`) work on a temporary directory.
package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Layout is where an installation lives.
type Layout struct {
	Prefix  string // "" on a real system
	DataDir string // not prefixed: always the server's real data directory
}

func (l Layout) p(path string) string { return filepath.Join(l.Prefix, path) }

func (l Layout) Opt() string      { return l.p("/opt/armageddon") }
func (l Layout) Versions() string { return filepath.Join(l.Opt(), "versions") }
func (l Layout) CurrentLink() string {
	return filepath.Join(l.Opt(), "current")
}
func (l Layout) BinLink() string   { return l.p("/usr/local/bin/armageddon") }
func (l Layout) Etc() string       { return l.p("/etc/armageddon") }
func (l Layout) UnitDir() string   { return l.p("/etc/systemd/system") }
func (l Layout) StateFile() string { return filepath.Join(l.Opt(), "update-state.json") }

// VersionDir is the immutable directory of one version.
func (l Layout) VersionDir(v string) string { return filepath.Join(l.Versions(), v) }

// Binary is the armageddon binary of one version.
func (l Layout) Binary(v string) string { return filepath.Join(l.VersionDir(v), "armageddon") }

// Current returns the version `current` points to.
func (l Layout) Current() (string, error) {
	t, err := os.Readlink(l.CurrentLink())
	if err != nil {
		return "", fmt.Errorf("no current version (%s): is Armageddon installed with install.sh? %w", l.CurrentLink(), err)
	}
	return filepath.Base(t), nil
}

// Switch atomically points `current` at version v (symlink + rename).
func (l Layout) Switch(v string) error {
	if _, err := os.Stat(l.Binary(v)); err != nil {
		return fmt.Errorf("version %s is not installed: %w", v, err)
	}
	tmp := l.CurrentLink() + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("versions", v), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, l.CurrentLink())
}

// Installed lists installed versions.
func (l Layout) Installed() []string {
	ents, _ := os.ReadDir(l.Versions())
	var out []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// ---- update history (for rollback) ----

// Step is one completed update.
type Step struct {
	From     string `json:"from"`
	To       string `json:"to"`
	DBBackup string `json:"db_backup,omitempty"`
	At       string `json:"at"`
}

type state struct {
	History []Step `json:"history"`
}

func (l Layout) loadState() (*state, error) {
	b, err := os.ReadFile(l.StateFile())
	if errors.Is(err, os.ErrNotExist) {
		return &state{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s state
	return &s, json.Unmarshal(b, &s)
}

func (l Layout) saveState(s *state) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := l.StateFile() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.StateFile())
}

// ---- services ----

// Services starts and stops the installed server. Implementations: Systemd
// on a real install, fakes in tests.
type Services interface {
	Stop() error
	Start() error
}

const (
	ServerUnit = "armageddon.service"
	HelperUnit = "armageddon-helper.service"
)

// Systemd manages the units installed in a layout's unit directory: the
// helper (if installed) starts first and stops last.
type Systemd struct {
	Layout Layout
	Run    func(args ...string) error // default: systemctl
}

func (s *Systemd) units() []string {
	var u []string
	for _, n := range []string{HelperUnit, ServerUnit} {
		if _, err := os.Stat(filepath.Join(s.Layout.UnitDir(), n)); err == nil {
			u = append(u, n)
		}
	}
	return u
}

func (s *Systemd) run(args ...string) error {
	if s.Run != nil {
		return s.Run(args...)
	}
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Systemd) Start() error {
	u := s.units()
	if len(u) == 0 {
		return fmt.Errorf("no Armageddon systemd units in %s (Docker installs update by pulling a new image)", s.Layout.UnitDir())
	}
	return s.run(append([]string{"start"}, u...)...)
}

func (s *Systemd) Stop() error {
	u := s.units()
	if len(u) == 0 {
		return fmt.Errorf("no Armageddon systemd units in %s (Docker installs update by pulling a new image)", s.Layout.UnitDir())
	}
	// Reverse order: the server before the helper.
	rev := make([]string, len(u))
	for i := range u {
		rev[len(u)-1-i] = u[i]
	}
	return s.run(append([]string{"stop"}, rev...)...)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
