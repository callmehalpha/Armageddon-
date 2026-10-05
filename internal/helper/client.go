package helper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Client is the server's view of the privileged helper (contract §2.5).
// internal/server reaches workspace users only through it.
type Client interface {
	// Isolated reports whether workspace processes run as separate OS users.
	Isolated() bool
	// CreateWorkspaceUser creates ws-<id> if needed and returns its account.
	// It is idempotent: an existing user is returned as is.
	CreateWorkspaceUser(ctx context.Context, wsID string) (*Account, error)
	DeleteWorkspaceUser(ctx context.Context, wsID string) error
	// PrepareWorkspaceDirs creates repo.git, tree, home and seat under the
	// workspace directory (which the server creates), owned by ws-<id>.
	PrepareWorkspaceDirs(ctx context.Context, wsID string) error
	// Spawn starts a process in the workspace as ws-<id>.
	Spawn(ctx context.Context, wsID string, spec SpawnSpec) (Process, error)
	SignalWorkspace(ctx context.Context, wsID, handle string, sig syscall.Signal) error
	SetWorkspaceLimits(ctx context.Context, wsID string, l Limits) error
	// RepairDataOwnership upgrades an MVP (root-owned) data directory.
	RepairDataOwnership(ctx context.Context) error
}

// SpawnSpec describes a workspace process. For pty-shell the helper opens
// the PTY and Process.PTY returns its master; Stdin/Stdout/Stderr are
// ignored. Otherwise the three files become the process's stdio (nil means
// /dev/null).
type SpawnSpec struct {
	Kind                  Kind
	Argv                  []string
	Env                   []string
	Dir                   string
	Cols, Rows            uint16
	Stdin, Stdout, Stderr *os.File
}

// Process is a running workspace process.
type Process interface {
	Handle() string
	Pid() int // informational only; signal through Signal
	// PTY is the master side of a pty-shell's terminal (nil otherwise). The
	// caller owns it.
	PTY() *os.File
	Signal(sig syscall.Signal) error
	// Wait blocks until the process exits. Calling it releases resources.
	Wait() (ExitStatus, error)
}

// Account is a workspace's OS user, as the server sees it. Its methods are
// the seam that server code uses to run things "as the workspace": Command
// and Prepare return ordinary *exec.Cmd values that, in isolated mode, go
// through the helper (via the `armageddon helper-exec` shim) instead of
// switching credentials in-process.
type Account struct {
	Name     string
	UID, GID uint32
	Home     string

	wsID   string
	root   string // workspace directory
	client Client
	// wrap rewrites a prepared command to run in the workspace. nil: run
	// directly as the current user (dev mode).
	wrap func(cmd *exec.Cmd, kind Kind, dir string)
}

// Workspace returns the workspace ID of the account.
func (a *Account) Workspace() string { return a.wsID }

// Isolated reports whether the account is a separate OS user.
func (a *Account) Isolated() bool { return a.wrap != nil }

// BaseEnv is the environment every workspace process starts from. The
// helper re-imposes PATH, HOME, USER and LOGNAME regardless of the request.
func (a *Account) BaseEnv() []string {
	return []string{
		"PATH=" + SafePath,
		"HOME=" + a.Home,
		"USER=" + a.Name,
		"LOGNAME=" + a.Name,
		"LANG=C.UTF-8",
	}
}

// SafePath is the PATH of workspace processes.
const SafePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func kindOf(cmd *exec.Cmd) Kind {
	if len(cmd.Args) > 0 && filepath.Base(cmd.Args[0]) == "git" {
		return KindGitService
	}
	return KindRuntimeCommand
}

// Prepare configures cmd to run as the account, with a clean environment
// (no inherited GIT_* or server secrets) plus extraEnv. The process kind is
// git-service for git and runtime-command otherwise. Set cmd.Dir before
// calling Prepare; callers may still append to cmd.Env afterwards.
func (a *Account) Prepare(cmd *exec.Cmd, extraEnv ...string) {
	a.PrepareKind(cmd, kindOf(cmd), extraEnv...)
}

// PrepareKind is Prepare with an explicit process kind.
func (a *Account) PrepareKind(cmd *exec.Cmd, kind Kind, extraEnv ...string) {
	cmd.Env = append(a.BaseEnv(), extraEnv...)
	if a.wrap != nil {
		dir := cmd.Dir
		if dir == "" {
			dir = a.root
		}
		a.wrap(cmd, kind, dir)
	}
}

// Command builds a command that runs as the account in dir.
func (a *Account) Command(dir string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	a.Prepare(cmd)
	return cmd
}

// CommandContext is Command bound to a context. Cancelling it ends the
// shim, and the helper then kills the workspace process.
func (a *Account) CommandContext(ctx context.Context, dir string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	a.Prepare(cmd)
	return cmd
}

// WriteFile writes a file as the account (so it is owned by it), creating
// missing parent directories (mode 0700). The server cannot write into
// workspace-owned directories itself.
func (a *Account) WriteFile(path string, data []byte, perm os.FileMode) error {
	if a.wrap == nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, perm); err != nil {
			return err
		}
		return os.Chmod(path, perm)
	}
	cmd := exec.Command("/bin/sh", "-c", `umask 077 && mkdir -p -- "$(dirname -- "$1")" && cat > "$1" && chmod "$2" "$1"`,
		"sh", path, fmt.Sprintf("%o", perm.Perm()))
	cmd.Dir = a.root
	a.PrepareKind(cmd, KindRuntimeCommand)
	w, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.Write(data)
	w.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("write %s as %s: %w", path, a.Name, err)
	}
	return nil
}

// MkdirOwned creates a directory (and parents) owned by the account with
// the given mode. In isolated mode it runs mkdir as the account, so the
// parent must be writable by it; the top-level workspace directories come
// from Client.PrepareWorkspaceDirs.
func (a *Account) MkdirOwned(p string, mode os.FileMode) error {
	if a.wrap == nil {
		if err := os.MkdirAll(p, mode); err != nil {
			return err
		}
		return os.Chmod(p, mode)
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() && owned(fi, a.UID) {
		return nil
	}
	cmd := exec.Command("mkdir", "-p", "-m", fmt.Sprintf("%o", mode.Perm()), "--", p)
	cmd.Dir = a.root
	a.PrepareKind(cmd, KindRuntimeCommand)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir %s as %s: %v: %s", p, a.Name, err, out)
	}
	return nil
}

// ErrNotOwned is returned by Chown in isolated mode for a path the account
// does not already own: the unprivileged server cannot change ownership.
var ErrNotOwned = errors.New("path is not owned by the workspace user (create it as the workspace user instead)")

// Chown is a compatibility shim for MVP code that wrote files as root and
// then gave them away. With the privilege split the server cannot chown;
// files must be created as the workspace user (WriteFile, MkdirOwned,
// Command). Chown now only verifies that each path is already owned by the
// account, and is a no-op in dev mode.
func (a *Account) Chown(paths ...string) error {
	if a.wrap == nil {
		return nil
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !owned(fi, a.UID) {
			return fmt.Errorf("%s: %w", p, ErrNotOwned)
		}
	}
	return nil
}

func owned(fi os.FileInfo, uid uint32) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}
