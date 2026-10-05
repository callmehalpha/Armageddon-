package helper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// Dev is the in-process helper for non-root development and unit tests:
// everything runs as the current user and there is no isolation. It keeps
// the validation of the real helper (operation parameters, process kinds,
// cwd inside the workspace) so code exercised in dev mode stays honest.
type Dev struct {
	DataDir string

	mu    sync.Mutex
	procs map[string]*devProc
}

// NewDev returns a dev helper for a data directory.
func NewDev(dataDir string) *Dev { return &Dev{DataDir: dataDir, procs: map[string]*devProc{}} }

func (d *Dev) Isolated() bool { return false }

func (d *Dev) wsRoot(wsID string) string { return filepath.Join(d.DataDir, "workspaces", wsID) }

func (d *Dev) CreateWorkspaceUser(ctx context.Context, wsID string) (*Account, error) {
	if !ValidWorkspaceID(wsID) {
		return nil, errors.New("invalid workspace ID")
	}
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	root := d.wsRoot(wsID)
	return &Account{Name: u.Username, UID: uint32(uid), GID: uint32(gid), Home: filepath.Join(root, "home"),
		wsID: wsID, root: root, client: d}, nil
}

func (d *Dev) DeleteWorkspaceUser(ctx context.Context, wsID string) error {
	if !ValidWorkspaceID(wsID) {
		return errors.New("invalid workspace ID")
	}
	return nil
}

func (d *Dev) PrepareWorkspaceDirs(ctx context.Context, wsID string) error {
	if !ValidWorkspaceID(wsID) {
		return errors.New("invalid workspace ID")
	}
	root := d.wsRoot(wsID)
	if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("workspace directory %s missing or not a directory", root)
	}
	for _, name := range WorkspaceDirs {
		p := filepath.Join(root, name)
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink: refused", p)
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// WorkspaceDirs are the workspace-owned directories PrepareWorkspaceDirs
// creates under workspaces/<id>/ (contract §2.4).
var WorkspaceDirs = []string{"repo.git", "tree", "home", "seat"}

type devProc struct {
	handle string
	cmd    *exec.Cmd
	ptmx   *os.File
	done   chan struct{}
	st     ExitStatus
	err    error
	d      *Dev
	wsID   string
}

func (p *devProc) Handle() string { return p.handle }
func (p *devProc) Pid() int       { return p.cmd.Process.Pid }
func (p *devProc) PTY() *os.File  { return p.ptmx }
func (p *devProc) Signal(sig syscall.Signal) error {
	return syscall.Kill(-p.cmd.Process.Pid, sig)
}
func (p *devProc) Wait() (ExitStatus, error) {
	<-p.done
	return p.st, p.err
}

func (d *Dev) Spawn(ctx context.Context, wsID string, spec SpawnSpec) (Process, error) {
	req := &Request{Op: OpSpawnInWorkspace, Workspace: wsID, Spawn: &SpawnArgs{Kind: spec.Kind, Argv: spec.Argv, Env: spec.Env, Cwd: spec.Dir, Cols: spec.Cols, Rows: spec.Rows}}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if !kinds[spec.Kind] {
		return nil, ErrReservedKind
	}
	if !within(d.wsRoot(wsID), spec.Dir) {
		return nil, fmt.Errorf("cwd %s is outside the workspace", spec.Dir)
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = filterEnv(spec.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	p := &devProc{handle: newHandle(), cmd: cmd, done: make(chan struct{}), d: d, wsID: wsID}
	var tty *os.File
	if spec.Kind == KindPTYShell {
		ptmx, t, err := pty.Open()
		if err != nil {
			return nil, err
		}
		if spec.Cols > 0 && spec.Rows > 0 {
			pty.Setsize(ptmx, &pty.Winsize{Cols: spec.Cols, Rows: spec.Rows})
		}
		p.ptmx, tty = ptmx, t
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr.Setsid = true
		cmd.SysProcAttr.Setctty = true
	} else {
		if spec.Stdin != nil {
			cmd.Stdin = spec.Stdin
		}
		if spec.Stdout != nil {
			cmd.Stdout = spec.Stdout
		}
		if spec.Stderr != nil {
			cmd.Stderr = spec.Stderr
		}
		cmd.SysProcAttr.Setpgid = true
	}
	err := cmd.Start()
	if tty != nil {
		tty.Close()
	}
	if err != nil {
		if p.ptmx != nil {
			p.ptmx.Close()
		}
		return nil, err
	}
	d.mu.Lock()
	d.procs[p.handle] = p
	d.mu.Unlock()
	go func() {
		err := cmd.Wait()
		p.st = exitStatusOf(cmd.ProcessState)
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			p.err = err
		}
		d.mu.Lock()
		delete(d.procs, p.handle)
		d.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (d *Dev) SignalWorkspace(ctx context.Context, wsID, handle string, sig syscall.Signal) error {
	req := &Request{Op: OpSignalWorkspace, Workspace: wsID, Signal: &SignalArgs{Handle: handle, Signal: int(sig)}}
	if err := req.Validate(); err != nil {
		return err
	}
	d.mu.Lock()
	var targets []*devProc
	for h, p := range d.procs {
		if p.wsID == wsID && (handle == "all" || h == handle) {
			targets = append(targets, p)
		}
	}
	d.mu.Unlock()
	if handle != "all" && len(targets) == 0 {
		return errors.New("no such process in this workspace")
	}
	for _, p := range targets {
		p.Signal(sig)
	}
	return nil
}

func (d *Dev) SetWorkspaceLimits(ctx context.Context, wsID string, l Limits) error {
	return errors.New("workspace limits need the root helper (cgroup v2); unavailable in dev mode")
}

func (d *Dev) RepairDataOwnership(ctx context.Context) error { return nil }

func exitStatusOf(ps *os.ProcessState) ExitStatus {
	if ps == nil {
		return ExitStatus{Code: -1}
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ExitStatus{Code: -1, Signal: int(ws.Signal())}
	}
	return ExitStatus{Code: ps.ExitCode()}
}

// filterEnv keeps allowlisted caller variables and appends the fixed ones.
func filterEnv(env []string) []string {
	fixed := map[string]string{}
	var out []string
	for _, kv := range env {
		k, v, _ := splitEnv(kv)
		switch k {
		case "HOME", "USER", "LOGNAME", "SHELL":
			fixed[k] = v
			continue
		}
		if EnvAllowed(kv) {
			out = append(out, kv)
		}
	}
	out = append(out, "PATH="+SafePath)
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SHELL"} {
		if v, ok := fixed[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func splitEnv(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

// within reports whether p is root or lexically below it.
func within(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || len(p) > len(root) && p[:len(root)] == root && p[len(root)] == '/'
}
