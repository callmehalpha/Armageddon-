package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Daemon is `armageddon helper`: the root process behind the socket.
type Daemon struct {
	DataDir   string // absolute; everything it touches is below it
	Socket    string
	ServerUID uint32
	ServerGID uint32
	// Exe is this binary, executed (as the workspace user) to set
	// no_new_privs before exec'ing the requested program.
	Exe string

	mu    sync.Mutex
	procs map[string]*daemonProc

	cg *cgroups

	compose *composeRunner
}

type daemonProc struct {
	ws     string
	pid    int // also the process group ID
	exited bool
}

// NewDaemon validates the configuration. It must run as root.
func NewDaemon(dataDir, socket, serverUser string) (*Daemon, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("armageddon helper must run as root")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	u, err := user.Lookup(serverUser)
	if err != nil {
		return nil, fmt.Errorf("server user %q: %w (create it with: useradd --system --user-group --no-create-home --shell /usr/sbin/nologin %s)", serverUser, err, serverUser)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	if uid == 0 {
		return nil, errors.New("the server user must not be root")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	d := &Daemon{DataDir: filepath.Clean(abs), Socket: socket, ServerUID: uint32(uid), ServerGID: uint32(gid), Exe: exe,
		procs: map[string]*daemonProc{}}
	d.cg = newCgroups()
	// Compose's project directory: root-only, outside the run directory
	// (which the server user owns).
	d.compose = &composeRunner{projectDir: filepath.Join(filepath.Dir(filepath.Dir(filepath.Clean(socket))), "armageddon-helper", "compose"),
		config: d.composeConfig}
	if d.cg.warning != "" {
		log.Printf("WARNING: %s", d.cg.warning)
	}
	return d, nil
}

// Serve listens on the socket until ctx ends.
func (d *Daemon) Serve(ctx context.Context) error {
	dir := filepath.Dir(d.Socket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// The run directory also holds the per-workspace authority sockets,
	// which the server creates (P-14).
	if err := os.Chown(dir, int(d.ServerUID), int(d.ServerGID)); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(d.Socket); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", d.Socket)
		}
		os.Remove(d.Socket)
	}
	old := syscall.Umask(0o177)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: d.Socket, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return err
	}
	l.SetUnlinkOnClose(true)
	if err := os.Chown(d.Socket, int(d.ServerUID), int(d.ServerGID)); err != nil {
		l.Close()
		return err
	}
	if err := os.Chmod(d.Socket, 0o600); err != nil {
		l.Close()
		return err
	}
	log.Printf("armageddon helper listening on %s (server uid %d, data %s)", d.Socket, d.ServerUID, d.DataDir)
	go func() { <-ctx.Done(); l.Close() }()
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go d.handle(conn)
	}
}

func (d *Daemon) handle(conn *net.UnixConn) {
	defer conn.Close()
	uid, err := PeerUID(conn)
	if err != nil || uid != d.ServerUID {
		log.Printf("helper: refused connection from uid %d (only the server uid %d may connect)", uid, d.ServerUID)
		return
	}
	body, files, err := recvFrame(conn)
	if err != nil {
		return
	}
	reply := func(resp *Response, fds ...*os.File) { sendFrame(conn, resp, fds...) }
	req, err := DecodeRequest(body)
	if err != nil {
		closeAll(files)
		reply(&Response{Error: err.Error()})
		return
	}
	if req.Op != OpSpawnInWorkspace || req.Spawn.Kind == KindPTYShell {
		if len(files) > 0 {
			closeAll(files)
			reply(&Response{Error: "unexpected file descriptors"})
			return
		}
	}
	ctx := context.Background()
	var resp *Response
	switch req.Op {
	case OpCreateWorkspaceUser:
		resp, err = d.createUser(req.Workspace)
	case OpDeleteWorkspaceUser:
		err = d.deleteUser(req.Workspace)
	case OpPrepareWorkspaceDirs:
		err = d.prepareDirs(req.Workspace)
	case OpSpawnInWorkspace:
		d.spawn(ctx, conn, req, files) // replies itself, possibly twice
		return
	case OpSignalWorkspace:
		err = d.signal(req.Workspace, req.Signal.Handle, syscall.Signal(req.Signal.Signal))
	case OpSetWorkspaceLimits:
		err = d.setLimits(req.Workspace, *req.Limits)
	case OpRepairDataOwnership:
		err = d.repairOwnership()
	case OpComposeUp, OpComposeDown, OpComposePs:
		resp, err = d.composeOp(ctx, req)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op) // unreachable: DecodeRequest checks
	}
	if err != nil {
		log.Printf("helper: %s %s: %v", req.Op, req.Workspace, err)
		reply(&Response{Error: err.Error()})
		return
	}
	if resp == nil {
		resp = &Response{}
	}
	resp.OK = true
	reply(resp)
}

// ---- users ----

type wsUser struct {
	name     string
	uid, gid uint32
}

func (d *Daemon) lookupUser(wsID string) (*wsUser, error) {
	name := UserName(wsID)
	if !ValidUserName(name) {
		return nil, fmt.Errorf("invalid workspace user name %q", name)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	if uid == 0 || gid == 0 || uint32(uid) == d.ServerUID || uint32(gid) == d.ServerGID {
		return nil, fmt.Errorf("workspace user %s has a privileged uid/gid (%d/%d): refused", name, uid, gid)
	}
	return &wsUser{name: name, uid: uint32(uid), gid: uint32(gid)}, nil
}

func (d *Daemon) wsRoot(wsID string) string { return filepath.Join(d.DataDir, "workspaces", wsID) }

func (d *Daemon) createUser(wsID string) (*Response, error) {
	name := UserName(wsID)
	if !ValidUserName(name) {
		return nil, fmt.Errorf("refusing invalid workspace user name %q", name)
	}
	if _, err := user.Lookup(name); err != nil {
		// No login shell, no password (useradd leaves it locked), no
		// supplementary groups, own primary group.
		cmd := exec.Command("useradd", "--system", "--user-group", "--no-create-home",
			"--home-dir", filepath.Join(d.wsRoot(wsID), "home"), "--shell", "/usr/sbin/nologin", "--", name)
		cmd.Env = []string{"PATH=" + SafePath}
		if out, err := cmd.CombinedOutput(); err != nil {
			if _, lerr := user.Lookup(name); lerr != nil {
				return nil, fmt.Errorf("useradd %s: %v: %s", name, err, out)
			}
		}
	}
	u, err := d.lookupUser(wsID)
	if err != nil {
		return nil, err
	}
	return &Response{User: u.name, UID: u.uid, GID: u.gid}, nil
}

func (d *Daemon) deleteUser(wsID string) error {
	u, err := d.lookupUser(wsID)
	if err != nil {
		var unk user.UnknownUserError
		if errors.As(err, &unk) {
			return nil
		}
		return err
	}
	d.killAll(wsID, u, syscall.SIGKILL)
	cmd := exec.Command("userdel", "--", u.name)
	cmd.Env = []string{"PATH=" + SafePath}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("userdel %s: %v: %s", u.name, err, out)
	}
	d.cg.remove(u.name)
	return nil
}

// ---- directories ----

// ErrNoOpenat2 is returned on kernels without openat2(2) (Linux < 5.6):
// the helper refuses to resolve workspace paths without it.
var ErrNoOpenat2 = errors.New("this kernel lacks openat2(2) (Linux 5.6 or newer is required): refusing to resolve workspace paths without RESOLVE_BENEATH")

// openBeneath opens rel below dirfd, refusing symlinks anywhere in the
// path and any escape from dirfd.
func openBeneath(dirfd int, rel string, flags int) (int, error) {
	how := &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS}
	fd, err := unix.Openat2(dirfd, rel, how)
	if errors.Is(err, unix.ENOSYS) {
		return -1, ErrNoOpenat2
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return -1, fmt.Errorf("%s: symlink or escape in path: refused", rel)
	}
	return fd, err
}

// openData opens the data directory, which must be owned by the server
// user (or root, before RepairDataOwnership).
func (d *Daemon) openData() (int, error) {
	fd, err := unix.Open(d.DataDir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("data directory: %w", err)
	}
	return fd, nil
}

// openWorkspace opens workspaces/<id> beneath the data directory and
// checks it is a server-owned directory not writable by others.
func (d *Daemon) openWorkspace(wsID string) (int, error) {
	data, err := d.openData()
	if err != nil {
		return -1, err
	}
	defer unix.Close(data)
	fd, err := openBeneath(data, filepath.Join("workspaces", wsID), unix.O_DIRECTORY|unix.O_RDONLY)
	if err != nil {
		return -1, fmt.Errorf("workspace directory: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if st.Uid != d.ServerUID || st.Mode&0o022 != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("workspace directory must be owned by the server user and not group/other-writable (uid %d, mode %o)", st.Uid, st.Mode&0o7777)
	}
	return fd, nil
}

func (d *Daemon) prepareDirs(wsID string) error {
	u, err := d.lookupUser(wsID)
	if err != nil {
		return err
	}
	wfd, err := d.openWorkspace(wsID)
	if err != nil {
		return err
	}
	defer unix.Close(wfd)
	for _, name := range WorkspaceDirs {
		if err := unix.Mkdirat(wfd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("mkdir %s: %w", name, err)
		}
		fd, err := openBeneath(wfd, name, unix.O_DIRECTORY|unix.O_RDONLY)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		err = unix.Fstat(fd, &st)
		if err == nil && st.Uid != 0 && st.Uid != d.ServerUID && st.Uid != u.uid {
			err = fmt.Errorf("%s is owned by uid %d, not this workspace: refused", name, st.Uid)
		}
		gid, mode := u.gid, uint32(0o700)
		if name == RunDir {
			// The code-server socket lives here: the server's group may
			// traverse and connect (setgid, so sockets get that group),
			// other workspace users may not even look up names.
			gid, mode = d.ServerGID, RunDirMode
		}
		if err == nil {
			err = unix.Fchown(fd, int(u.uid), int(gid))
		}
		if err == nil {
			err = unix.Fchmod(fd, mode)
		}
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// checkCwd verifies that cwd is the workspace directory or below it, with
// no symlink on the way. The chdir itself happens after the credential
// switch, so a later swap can only lead where the workspace user could go
// anyway.
func (d *Daemon) checkCwd(wsID, cwd string) error {
	root := d.wsRoot(wsID)
	if !within(root, cwd) {
		return fmt.Errorf("cwd %s is outside the workspace", cwd)
	}
	wfd, err := d.openWorkspace(wsID)
	if err != nil {
		return err
	}
	defer unix.Close(wfd)
	rel, _ := filepath.Rel(root, cwd)
	fd, err := openBeneath(wfd, rel, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	unix.Close(fd)
	return nil
}

// repairOwnership upgrades an MVP data directory, written by a root
// server, to the split layout. It gives the server user every root-owned
// entry below the data directory. It never follows symlinks, and it never
// enters a directory owned by anyone other than root or the server user
// (workspace-owned trees stay as they are). Nothing outside DataDir is
// touched.
func (d *Daemon) repairOwnership() error {
	fd, err := d.openData()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != 0 && st.Uid != d.ServerUID {
		return fmt.Errorf("data directory is owned by uid %d: refusing to repair", st.Uid)
	}
	if st.Uid == 0 {
		if err := unix.Fchown(fd, int(d.ServerUID), int(d.ServerGID)); err != nil {
			return err
		}
	}
	n, err := d.repairDir(fd, 0)
	log.Printf("helper: data directory upgrade: %d entries handed to the server user", n)
	return err
}

func (d *Daemon) repairDir(dirfd int, depth int) (int, error) {
	if depth > 64 {
		return 0, errors.New("directory tree too deep")
	}
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		owner := st.Uid
		if owner == 0 {
			if err := unix.Fchownat(dirfd, name, int(d.ServerUID), int(d.ServerGID), unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return n, fmt.Errorf("chown %s: %w", name, err)
			}
			n++
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || (owner != 0 && owner != d.ServerUID) {
			continue
		}
		cfd, err := unix.Openat(dirfd, name, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return n, fmt.Errorf("open %s: %w", name, err)
		}
		var cst unix.Stat_t
		if err := unix.Fstat(cfd, &cst); err != nil || cst.Ino != st.Ino || cst.Dev != st.Dev {
			unix.Close(cfd)
			return n, fmt.Errorf("%s changed during repair: refused", name)
		}
		m, err := d.repairDir(cfd, depth+1)
		unix.Close(cfd)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ---- processes ----

func (d *Daemon) spawn(ctx context.Context, conn *net.UnixConn, req *Request, files []*os.File) {
	defer closeAll(files)
	fail := func(err error) {
		log.Printf("helper: SpawnInWorkspace %s: %v", req.Workspace, err)
		sendFrame(conn, &Response{Error: err.Error()})
	}
	s := req.Spawn
	if !kinds[s.Kind] {
		fail(ErrReservedKind)
		return
	}
	prog, argv, err := programFor(s.Kind, s.Argv, d.Exe)
	if err != nil {
		fail(err)
		return
	}
	if s.Kind == KindCodeServer {
		if err := d.checkCodeServer(prog); err != nil {
			fail(err)
			return
		}
	}
	if s.Kind != KindPTYShell && len(files) != 3 {
		fail(errors.New("stdin, stdout and stderr descriptors are required"))
		return
	}
	u, err := d.lookupUser(req.Workspace)
	if err != nil {
		fail(err)
		return
	}
	if err := d.checkCwd(req.Workspace, s.Cwd); err != nil {
		fail(err)
		return
	}
	env := []string{"PATH=" + SafePath, "HOME=" + filepath.Join(d.wsRoot(req.Workspace), "home"), "USER=" + u.name, "LOGNAME=" + u.name}
	if sh := shellFor(s.Kind, s.Argv); sh != "" {
		env = append(env, "SHELL="+sh)
	}
	for _, kv := range s.Env {
		if EnvAllowed(kv) {
			env = append(env, kv)
		}
	}
	// The child runs this binary's no_new_privs trampoline as the
	// workspace user, which then execs the program (Go's SysProcAttr has
	// no no_new_privs switch).
	var ptmx, tty *os.File
	if s.Kind == KindPTYShell {
		ptmx, tty, err = pty.Open()
		if err != nil {
			fail(err)
			return
		}
		defer tty.Close()
		// The terminal belongs to the workspace user (contract §2.5).
		if err := tty.Chown(int(u.uid), int(u.gid)); err != nil {
			ptmx.Close()
			fail(err)
			return
		}
		tty.Chmod(0o620)
		if s.Cols > 0 && s.Rows > 0 {
			pty.Setsize(ptmx, &pty.Winsize{Cols: s.Cols, Rows: s.Rows})
		}
	}
	build := func() *exec.Cmd {
		cmd := &exec.Cmd{Path: d.Exe, Args: append([]string{d.Exe, "helper-nnp", "--", prog}, argv...), Env: env, Dir: s.Cwd}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: []uint32{}}}
		if tty != nil {
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
			cmd.SysProcAttr.Setsid = true
			cmd.SysProcAttr.Setctty = true
		} else {
			cmd.Stdin, cmd.Stdout, cmd.Stderr = files[0], files[1], files[2]
			cmd.SysProcAttr.Setpgid = true
		}
		return cmd
	}
	cmd := build()
	warning := d.cg.attach(cmd, u.name)
	err = cmd.Start()
	if err != nil && cmd.SysProcAttr.UseCgroupFD {
		// Kernels without clone3(CLONE_INTO_CGROUP): run without.
		d.cg.disable(fmt.Sprintf("cannot start processes in a cgroup (%v); running without per-workspace cgroups", err))
		d.cg.release(cmd)
		cmd = build()
		err = cmd.Start()
	}
	d.cg.release(cmd)
	closeAll(files)
	files = nil
	if err != nil {
		if ptmx != nil {
			ptmx.Close()
		}
		fail(err)
		return
	}
	h := newHandle()
	p := &daemonProc{ws: req.Workspace, pid: cmd.Process.Pid}
	d.mu.Lock()
	d.procs[h] = p
	d.mu.Unlock()
	resp := &Response{OK: true, Handle: h, Pid: p.pid, Warning: warning}
	if ptmx != nil {
		sendFrame(conn, resp, ptmx)
		ptmx.Close() // the server holds the master now
	} else {
		sendFrame(conn, resp)
	}
	// If the requester goes away before the process ends, end the process.
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := conn.Read(buf); err != nil {
				break
			}
		}
		d.mu.Lock()
		if !p.exited {
			syscall.Kill(-p.pid, syscall.SIGKILL)
		}
		d.mu.Unlock()
	}()
	cmd.Wait()
	st := exitStatusOf(cmd.ProcessState)
	d.mu.Lock()
	p.exited = true
	delete(d.procs, h)
	d.mu.Unlock()
	sendFrame(conn, &Response{OK: true, Handle: h, Exit: &st})
}

func (d *Daemon) signal(wsID, handle string, sig syscall.Signal) error {
	u, err := d.lookupUser(wsID)
	if err != nil {
		return err
	}
	if handle == "all" {
		d.killAll(wsID, u, sig)
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.procs[handle]
	if !ok || p.ws != wsID || p.exited {
		return errors.New("no such process in this workspace")
	}
	// The handle's process group; it was created as the workspace user.
	return syscall.Kill(-p.pid, sig)
}

// killAll signals every process of the workspace: its cgroup's members
// when cgroups are available, and in any case every process whose real or
// effective uid is the workspace user's.
func (d *Daemon) killAll(wsID string, u *wsUser, sig syscall.Signal) {
	for _, pid := range d.cg.pids(u.name) {
		if uidOf(pid) == u.uid {
			syscall.Kill(pid, sig)
		}
	}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if uidOf(pid) == u.uid {
			syscall.Kill(pid, sig)
		}
	}
}

// uidOf returns the real uid of pid, or MaxUint32 if unknown. A process
// is only signalled if its real uid is the workspace user's.
func uidOf(pid int) uint32 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return ^uint32(0)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) >= 2 && f[0] == f[1] {
				v, err := strconv.ParseUint(f[0], 10, 32)
				if err == nil {
					return uint32(v)
				}
			}
			return ^uint32(0)
		}
	}
	return ^uint32(0)
}

func (d *Daemon) setLimits(wsID string, l Limits) error {
	u, err := d.lookupUser(wsID)
	if err != nil {
		return err
	}
	return d.cg.setLimits(u.name, l)
}

// NNPMain is `armageddon helper-nnp -- PROG ARGV...`: run by the helper as
// the workspace user, it sets no_new_privs (setuid binaries and file
// capabilities no longer elevate) and execs PROG with ARGV.
func NNPMain(args []string) int {
	if len(args) < 3 || args[0] != "--" {
		fmt.Fprintln(os.Stderr, "usage: armageddon helper-nnp -- PROG ARGV0 [ARGS...]")
		return 127
	}
	if os.Geteuid() == 0 || os.Getuid() == 0 {
		fmt.Fprintln(os.Stderr, "armageddon helper-nnp: refusing to run as root")
		return 127
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "armageddon helper-nnp: no_new_privs:", err)
		return 127
	}
	err := unix.Exec(args[1], args[2:], os.Environ())
	fmt.Fprintf(os.Stderr, "armageddon: exec %s: %v\n", args[1], err)
	return 127
}

// ---- Docker Compose (plan M8.4; see compose.go) ----

func (d *Daemon) composeOp(ctx context.Context, req *Request) (*Response, error) {
	if _, err := d.lookupUser(req.Workspace); err != nil {
		return nil, err
	}
	if err := ensureRootDir(d.compose.projectDir); err != nil {
		return nil, err
	}
	switch req.Op {
	case OpComposeUp:
		w, err := d.compose.up(ctx, req.Workspace, *req.Compose)
		return &Response{Warning: w}, err
	case OpComposeDown:
		return nil, d.compose.down(ctx, req.Workspace)
	}
	svcs, err := d.compose.ps(ctx, req.Workspace)
	return &Response{Services: svcs}, err
}

// ensureRootDir creates dir (and its parent) as root-only directories and
// checks that neither is a symlink or writable by anyone else.
func ensureRootDir(dir string) error {
	for _, p := range []string{filepath.Dir(dir), dir} {
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0o077 != 0 {
			return fmt.Errorf("%s must be a root-owned directory with mode 0700: refused", p)
		}
	}
	return nil
}

// composeConfig runs `docker compose … config` as the workspace user, with
// no_new_privs, in the workspace's tree/: every file the Compose project
// refers to is read with the workspace's permissions. Its output is only
// data for NormalizeCompose.
func (d *Daemon) composeConfig(ctx context.Context, wsID string, argv []string) ([]byte, error) {
	u, err := d.lookupUser(wsID)
	if err != nil {
		return nil, err
	}
	tree := filepath.Join(d.wsRoot(wsID), "tree")
	if err := d.checkCwd(wsID, tree); err != nil {
		return nil, err
	}
	docker, err := dockerPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Exe, append([]string{"helper-nnp", "--", docker}, argv...)...)
	cmd.Dir = tree
	cmd.Env = []string{"PATH=" + SafePath, "HOME=" + filepath.Join(d.wsRoot(wsID), "home"), "USER=" + u.name, "LOGNAME=" + u.name}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: []uint32{}}, Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return composeConfigOutput(cmd)
}
