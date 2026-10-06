package helper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Socket is the production Client: it talks to `armageddon helper` over
// its unix socket. Commands built through Account (Command, Prepare) run
// the `armageddon helper-exec` shim, which hands its stdio to the helper
// with SCM_RIGHTS and relays the exit status, so callers keep using
// ordinary *exec.Cmd values.
type Socket struct {
	Path    string // helper socket
	DataDir string // the server's data directory (same as the helper's)
	Shim    string // path of the armageddon binary, run as the shim
}

func (c *Socket) Isolated() bool { return true }

func (c *Socket) dial(ctx context.Context) (*net.UnixConn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("helper unreachable at %s: %w", c.Path, err)
	}
	return conn.(*net.UnixConn), nil
}

// call does one request/response exchange on a fresh connection.
func (c *Socket) call(ctx context.Context, req *Request, files ...*os.File) (*Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	resp, rfiles, err := exchange(conn, req, files...)
	closeAll(rfiles)
	return resp, err
}

func exchange(conn *net.UnixConn, req *Request, files ...*os.File) (*Response, []*os.File, error) {
	if err := sendFrame(conn, req, files...); err != nil {
		return nil, nil, fmt.Errorf("helper %s: %w", req.Op, err)
	}
	body, rfiles, err := recvFrame(conn)
	if err != nil {
		return nil, nil, fmt.Errorf("helper %s: %w", req.Op, err)
	}
	resp, err := DecodeResponse(body)
	if err != nil {
		closeAll(rfiles)
		return nil, nil, err
	}
	if !resp.OK {
		closeAll(rfiles)
		return resp, nil, fmt.Errorf("helper %s: %s", req.Op, resp.Error)
	}
	return resp, rfiles, nil
}

func (c *Socket) wsRoot(wsID string) string { return filepath.Join(c.DataDir, "workspaces", wsID) }

func (c *Socket) CreateWorkspaceUser(ctx context.Context, wsID string) (*Account, error) {
	resp, err := c.call(ctx, &Request{Op: OpCreateWorkspaceUser, Workspace: wsID})
	if err != nil {
		return nil, err
	}
	root := c.wsRoot(wsID)
	a := &Account{Name: resp.User, UID: resp.UID, GID: resp.GID, Home: filepath.Join(root, "home"),
		wsID: wsID, root: root, client: c}
	a.wrap = func(cmd *exec.Cmd, kind Kind, dir string) { c.wrap(a, cmd, kind, dir) }
	return a, nil
}

// wrap turns cmd into an invocation of the shim. The original argv is
// passed through; the helper resolves the program on its fixed PATH.
func (c *Socket) wrap(a *Account, cmd *exec.Cmd, kind Kind, dir string) {
	argv := append([]string(nil), cmd.Args...)
	if len(argv) == 0 {
		argv = []string{cmd.Path}
	}
	if kind == KindGitService {
		argv[0] = "git"
	}
	cmd.Path = c.Shim
	cmd.Args = append([]string{c.Shim, "helper-exec", "--socket", c.Path, "--workspace", a.wsID,
		"--kind", string(kind), "--cwd", dir, "--"}, argv...)
	// The server cannot enter workspace-owned directories; the helper
	// sets the real cwd.
	cmd.Dir = "/"
	cmd.Err = nil // a failed LookPath of the original name is irrelevant now
}

func (c *Socket) DeleteWorkspaceUser(ctx context.Context, wsID string) error {
	_, err := c.call(ctx, &Request{Op: OpDeleteWorkspaceUser, Workspace: wsID})
	return err
}

func (c *Socket) PrepareWorkspaceDirs(ctx context.Context, wsID string) error {
	_, err := c.call(ctx, &Request{Op: OpPrepareWorkspaceDirs, Workspace: wsID})
	return err
}

func (c *Socket) SignalWorkspace(ctx context.Context, wsID, handle string, sig syscall.Signal) error {
	_, err := c.call(ctx, &Request{Op: OpSignalWorkspace, Workspace: wsID, Signal: &SignalArgs{Handle: handle, Signal: int(sig)}})
	return err
}

func (c *Socket) SetWorkspaceLimits(ctx context.Context, wsID string, l Limits) error {
	_, err := c.call(ctx, &Request{Op: OpSetWorkspaceLimits, Workspace: wsID, Limits: &l})
	return err
}

func (c *Socket) RepairDataOwnership(ctx context.Context) error {
	_, err := c.call(ctx, &Request{Op: OpRepairDataOwnership})
	return err
}

// Spawn starts a workspace process. The connection stays open for the
// process's lifetime: the helper streams its exit status on it, and kills
// the process if the connection drops first.
func (c *Socket) Spawn(ctx context.Context, wsID string, spec SpawnSpec) (Process, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	req := &Request{Op: OpSpawnInWorkspace, Workspace: wsID, Spawn: &SpawnArgs{Kind: spec.Kind, Argv: spec.Argv,
		Env: spec.Env, Cwd: spec.Dir, Cols: spec.Cols, Rows: spec.Rows}}
	var files []*os.File
	if spec.Kind != KindPTYShell {
		var opened []*os.File
		for _, f := range []*os.File{spec.Stdin, spec.Stdout, spec.Stderr} {
			if f == nil {
				nf, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
				if err != nil {
					closeAll(opened)
					conn.Close()
					return nil, err
				}
				opened = append(opened, nf)
				f = nf
			}
			files = append(files, f)
		}
		defer closeAll(opened)
	}
	resp, rfiles, err := exchange(conn, req, files...)
	if err != nil {
		conn.Close()
		return nil, err
	}
	p := &sockProc{c: c, conn: conn, ws: wsID, handle: resp.Handle, pid: resp.Pid}
	if spec.Kind == KindPTYShell {
		if len(rfiles) != 1 {
			closeAll(rfiles)
			conn.Close()
			return nil, errors.New("helper returned no PTY")
		}
		p.ptmx = rfiles[0]
	} else {
		closeAll(rfiles)
	}
	return p, nil
}

type sockProc struct {
	c      *Socket
	conn   *net.UnixConn
	ws     string
	handle string
	pid    int
	ptmx   *os.File

	once sync.Once
	st   ExitStatus
	err  error
}

func (p *sockProc) Handle() string { return p.handle }
func (p *sockProc) Pid() int       { return p.pid }
func (p *sockProc) PTY() *os.File  { return p.ptmx }

func (p *sockProc) Signal(sig syscall.Signal) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.c.SignalWorkspace(ctx, p.ws, p.handle, sig)
}

func (p *sockProc) Wait() (ExitStatus, error) {
	p.once.Do(func() {
		defer p.conn.Close()
		body, files, err := recvFrame(p.conn)
		closeAll(files)
		if err != nil {
			p.st, p.err = ExitStatus{Code: -1}, fmt.Errorf("lost the helper while waiting: %w", err)
			return
		}
		resp, err := DecodeResponse(body)
		if err != nil || resp.Exit == nil {
			p.st, p.err = ExitStatus{Code: -1}, fmt.Errorf("helper sent no exit status: %v", err)
			return
		}
		p.st = *resp.Exit
	})
	return p.st, p.err
}

// Reachable reports whether a helper socket exists at path.
func Reachable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
