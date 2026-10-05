// Package helper is the throwaway root helper for P6. It implements the
// contract §2.5 typed API over a unix socket and nothing else: there is no
// request that runs a caller-supplied program as root. Every spawn drops to
// the workspace user before exec.
package helper

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"armageddon/prototypes/p6-privilege-boundary/proto"
)

// Config configures a helper instance.
type Config struct {
	Socket    string // unix socket path, created with mode 0600
	ServerUID int    // the only uid SO_PEERCRED will accept
	BaseDir   string // /var/lib/armageddon/workspaces equivalent
	Self      string // path to this binary, for the stub re-exec
	Logf      func(string, ...any)
}

type process struct {
	cmd  *exec.Cmd
	pgid int
}

// Helper holds running state.
type Helper struct {
	cfg  Config
	mu   sync.Mutex
	cg   map[string]*cgroupV2           // workspace -> cgroup (nil if v2 absent)
	proc map[string]map[string]*process // workspace -> handle -> process
	next int
}

// Serve listens on the socket and handles requests until the listener closes.
func Serve(cfg Config) (*Helper, *net.UnixListener, error) {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	_ = os.Remove(cfg.Socket)
	addr := &net.UnixAddr{Name: cfg.Socket, Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, nil, err
	}
	// Mode 0600. E4 proves the SO_PEERCRED check is the real boundary even if
	// this mode is loosened; the mode is defence in depth.
	if err := os.Chmod(cfg.Socket, 0o600); err != nil {
		l.Close()
		return nil, nil, err
	}
	h := &Helper{cfg: cfg, cg: map[string]*cgroupV2{}, proc: map[string]map[string]*process{}}
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go h.handleConn(c)
		}
	}()
	return h, l, nil
}

func (h *Helper) handleConn(c *net.UnixConn) {
	defer c.Close()
	uid, _, pid, err := PeerUID(c)
	if err != nil {
		return
	}
	if uid != h.cfg.ServerUID {
		h.cfg.Logf("p6-helper: REJECT peer uid=%d pid=%d (only server uid %d may connect)", uid, pid, h.cfg.ServerUID)
		_ = writeMsg(c, proto.Response{Error: "unauthorized peer"}, nil)
		return
	}
	for {
		body, fds, err := readMsg(c, 8)
		if err != nil {
			closeAll(fds)
			return
		}
		req, err := proto.DecodeRequest(body)
		if err != nil {
			closeAll(fds)
			_ = writeMsg(c, proto.Response{Error: "decode: " + err.Error()}, nil)
			continue
		}
		resp, respFDs := h.dispatch(req, fds)
		closeAll(fds)
		_ = writeMsg(c, resp, respFDs)
		closeAll(respFDs)
	}
}

func (h *Helper) dispatch(req *proto.Request, fds []int) (proto.Response, []int) {
	switch req.Op {
	case proto.OpCreateWorkspaceUser:
		return h.createUser(req), nil
	case proto.OpDeleteWorkspaceUser:
		return h.deleteUser(req), nil
	case proto.OpPrepareWorkspaceDirs:
		return h.prepareDirs(req), nil
	case proto.OpSpawnInWorkspace:
		return h.spawn(req, fds)
	case proto.OpSignalWorkspace:
		return h.signal(req), nil
	case proto.OpSetWorkspaceLimits:
		return h.setLimits(req), nil
	}
	return proto.Response{Error: "unreachable"}, nil
}

func (h *Helper) createUser(req *proto.Request) proto.Response {
	name := proto.UserName(req.Workspace)
	home := filepath.Join(h.cfg.BaseDir, req.Workspace, "home")
	// No login shell, no password, no supplementary groups, no home creation.
	cmd := exec.Command("useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", home, "--shell", "/usr/sbin/nologin", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		if _, lerr := lookupUser(name); lerr == nil {
			return proto.Response{OK: true} // already exists
		}
		return proto.Response{Error: fmt.Sprintf("useradd: %v: %s", err, out)}
	}
	return proto.Response{OK: true}
}

func (h *Helper) deleteUser(req *proto.Request) proto.Response {
	name := proto.UserName(req.Workspace)
	h.signal(&proto.Request{Op: proto.OpSignalWorkspace, Workspace: req.Workspace, Handle: "all", Signal: int(unix.SIGKILL)})
	if out, err := exec.Command("userdel", name).CombinedOutput(); err != nil {
		if _, lerr := lookupUser(name); lerr != nil {
			return proto.Response{OK: true}
		}
		return proto.Response{Error: fmt.Sprintf("userdel: %v: %s", err, out)}
	}
	if cg := h.cgroupFor(req.Workspace, false); cg != nil {
		cg.remove()
	}
	return proto.Response{OK: true}
}

func (h *Helper) prepareDirs(req *proto.Request) proto.Response {
	u, err := lookupUser(proto.UserName(req.Workspace))
	if err != nil {
		return proto.Response{Error: "user missing: " + err.Error()}
	}
	wsDir := filepath.Join(h.cfg.BaseDir, req.Workspace)
	// The workspace directory itself is created root-owned (0755) so the
	// workspace user cannot swap it for a symlink (E6). Children are resolved
	// strictly beneath it with openat2.
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return proto.Response{Error: err.Error()}
	}
	dirfd, err := unix.Open(wsDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return proto.Response{Error: "open base: " + err.Error()}
	}
	defer unix.Close(dirfd)
	for _, sub := range []string{"repo.git", "tree", "home"} {
		fd, err := openatBeneath(dirfd, sub)
		if err != nil {
			return proto.Response{Error: fmt.Sprintf("openat2 %s: %v", sub, err)}
		}
		// fchown the resolved fd, never a path: the resolution already
		// refused any symlink component (E6).
		if err := unix.Fchown(fd, int(u.uid), int(u.gid)); err != nil {
			unix.Close(fd)
			return proto.Response{Error: "fchown: " + err.Error()}
		}
		_ = unix.Fchmod(fd, 0o700)
		unix.Close(fd)
	}
	return proto.Response{OK: true}
}

// openatBeneath creates (if needed) and opens a single-component child of
// dirfd, refusing symlinks and any escape above dirfd (RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS). If the child exists as a symlink, the open fails,
// which is exactly the E6 defence.
func openatBeneath(dirfd int, name string) (int, error) {
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(dirfd, name, how)
	if err == nil {
		return fd, nil
	}
	if err != unix.ENOENT {
		return -1, err // ELOOP here means a symlink is in the way (E6)
	}
	// Create it, still beneath and without following symlinks.
	mkHow := &unix.OpenHow{
		Flags:   uint64(unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_CREAT | unix.O_EXCL),
		Mode:    0o700,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	// openat2 cannot O_CREAT a directory; use mkdirat then re-open beneath.
	if err := unix.Mkdirat(dirfd, name, 0o700); err != nil && err != unix.EEXIST {
		return -1, err
	}
	_ = mkHow
	return unix.Openat2(dirfd, name, how)
}

func (h *Helper) spawn(req *proto.Request, fds []int) (proto.Response, []int) {
	u, err := lookupUser(proto.UserName(req.Workspace))
	if err != nil {
		return proto.Response{Error: "user missing: " + err.Error()}, nil
	}
	wsDir := filepath.Join(h.cfg.BaseDir, req.Workspace)
	cwd := wsDir
	if req.Kind == proto.KindGitService || req.Kind == proto.KindPTYShell {
		cwd = filepath.Join(wsDir, "tree")
	}

	stubArgs := []string{"stub"}
	if req.WantPTY {
		stubArgs = append(stubArgs, "--pty")
	}
	stubArgs = append(stubArgs, "--")
	stubArgs = append(stubArgs, req.Argv...)
	cmd := exec.Command(h.cfg.Self, stubArgs...)
	cmd.Dir = cwd
	cmd.Env = append(baseEnv(u.name, filepath.Join(wsDir, "home")), req.Env...)

	// Wire the passed descriptors as stdio.
	var files []*os.File
	for i, fd := range fds {
		files = append(files, os.NewFile(uintptr(fd), fmt.Sprintf("passed-%d", i)))
	}
	if len(files) >= 1 {
		cmd.Stdin = files[0]
	}
	if len(files) >= 2 {
		cmd.Stdout = files[1]
	}
	if len(files) >= 3 {
		cmd.Stderr = files[2]
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: []uint32{}, NoSetGroups: false},
		Setsid:     true,
	}
	// cgroup v2 placement at clone time, when available.
	if cg := h.cgroupFor(req.Workspace, true); cg != nil {
		if cfd, err := cg.FD(); err == nil {
			cmd.SysProcAttr.UseCgroupFD = true
			cmd.SysProcAttr.CgroupFD = cfd
			defer unix.Close(cfd)
		}
	}
	if err := cmd.Start(); err != nil {
		return proto.Response{Error: "start: " + err.Error()}, nil
	}
	pgid, _ := unix.Getpgid(cmd.Process.Pid)
	h.mu.Lock()
	h.next++
	handle := "h" + strconv.Itoa(h.next)
	if h.proc[req.Workspace] == nil {
		h.proc[req.Workspace] = map[string]*process{}
	}
	h.proc[req.Workspace][handle] = &process{cmd: cmd, pgid: pgid}
	h.mu.Unlock()
	go func() { cmd.Wait() }()
	return proto.Response{OK: true, Handle: handle, PID: cmd.Process.Pid}, nil
}

func (h *Helper) signal(req *proto.Request) proto.Response {
	sig := unix.Signal(req.Signal)
	if cg := h.cgroupFor(req.Workspace, false); cg != nil && req.Handle == "all" {
		_ = cg.kill(req.Signal)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	procs := h.proc[req.Workspace]
	for handle, p := range procs {
		if req.Handle != "all" && req.Handle != handle {
			continue
		}
		if p.pgid > 0 {
			_ = unix.Kill(-p.pgid, sig) // whole process group
		} else if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(sig)
		}
	}
	return proto.Response{OK: true}
}

func (h *Helper) setLimits(req *proto.Request) proto.Response {
	cg := h.cgroupFor(req.Workspace, true)
	if cg == nil {
		return proto.Response{Error: "cgroup v2 unavailable: " + describeCgroups()}
	}
	if err := cg.setLimits(req.MemoryBytes, req.PidsMax, req.CPUWeight); err != nil {
		return proto.Response{Error: err.Error()}
	}
	return proto.Response{OK: true}
}

func (h *Helper) cgroupFor(ws string, create bool) *cgroupV2 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cg, ok := h.cg[ws]; ok {
		return cg
	}
	if !create {
		return nil
	}
	cg, err := ensureCgroupV2(ws)
	if err != nil {
		h.cfg.Logf("p6-helper: cgroup for %s: %v", ws, err)
	}
	h.cg[ws] = cg // may be nil (v2 unavailable); cached either way
	return cg
}

func baseEnv(name, home string) []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home, "USER=" + name, "LOGNAME=" + name, "LANG=C.UTF-8",
	}
}
