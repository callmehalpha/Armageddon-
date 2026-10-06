package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/callmehalpha/Armageddon-/internal/helper"
)

// sessState is what a session channel's requests set up before it starts.
type sessState struct {
	mu         sync.Mutex
	term       string
	env        []string
	wantPTY    bool
	cols, rows uint32
	ptmx       *os.File
}

func (st *sessState) resize(cols, rows uint32) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cols, st.rows = cols, rows
	if st.ptmx != nil && cols > 0 && rows > 0 {
		pty.Setsize(st.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

func (st *sessState) setPTY(f *os.File) {
	st.mu.Lock()
	st.ptmx = f
	cols, rows := st.cols, st.rows
	st.mu.Unlock()
	st.resize(cols, rows)
}

func (d *sshServer) session(c *sshConn, nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		ch.Close()
		return
	}
	c.sessions[ch] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.sessions, ch)
		c.mu.Unlock()
		ch.Close()
	}()

	st := &sessState{term: "xterm-256color"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	started := false
	// reqs closes when the channel closes, from either side.
	for req := range reqs {
		ok := false
		switch req.Type {
		case "pty-req":
			var p struct {
				Term          string
				Cols, Rows    uint32
				Width, Height uint32
				Modes         string
			}
			if !started && ssh.Unmarshal(req.Payload, &p) == nil {
				st.mu.Lock()
				st.wantPTY = true
				if p.Term != "" {
					st.term = p.Term
				}
				st.cols, st.rows = p.Cols, p.Rows
				st.mu.Unlock()
				ok = true
			}
		case "window-change":
			var p struct{ Cols, Rows, Width, Height uint32 }
			if ssh.Unmarshal(req.Payload, &p) == nil {
				st.resize(p.Cols, p.Rows)
				ok = true
			}
		case "env":
			var p struct{ Name, Value string }
			if !started && ssh.Unmarshal(req.Payload, &p) == nil && envAllowed(p.Name) && !strings.ContainsRune(p.Value, 0) {
				st.mu.Lock()
				st.env = append(st.env, p.Name+"="+p.Value)
				st.mu.Unlock()
				ok = true
			}
		case "shell", "exec", "subsystem":
			if started {
				break
			}
			var arg struct{ Value string }
			if req.Type != "shell" && ssh.Unmarshal(req.Payload, &arg) != nil {
				break
			}
			if req.Type == "subsystem" && arg.Value != "sftp" {
				break
			}
			started = true
			req.Reply(true, nil)
			kind := req.Type
			go func() {
				defer close(finished)
				d.run(ctx, c, ch, st, kind, arg.Value)
			}()
			continue
		}
		if req.WantReply {
			req.Reply(ok, nil)
		}
	}
	cancel() // the client went away: stop the process
	if started {
		<-finished
	}
}

// run starts the session's process as the workspace user and bridges it to
// the channel until it exits or ctx ends.
func (d *sshServer) run(ctx context.Context, c *sshConn, ch ssh.Channel, st *sessState, kind, arg string) {
	s := d.s
	if !s.serverHoldsLease(c.ws.ID) {
		refuse(ch, leaseRefusal)
		return
	}
	rt := s.runtimeFor(c.ws.ID)
	if rt == nil {
		refuse(ch, "workspace not running")
		return
	}
	creds, err := s.openSeatCredentials(rt, c.userID)
	if err != nil {
		refuse(ch, "credentials: "+err.Error())
		return
	}
	defer creds.Close()

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	var argv []string
	switch kind {
	case "subsystem": // sftp: the helper runs its own `armageddon sftp-server`
		argv = []string{helper.SFTPServerArgv}
	case "exec":
		argv = []string{shell, "-c", arg}
	default:
		argv = []string{shell, "-l"}
	}
	st.mu.Lock()
	usePTY := st.wantPTY && kind != "subsystem"
	extra := append(creds.Env(), "TERM="+st.term)
	extra = append(extra, st.env...)
	cols, rows := clampSize(st.cols), clampSize(st.rows)
	st.mu.Unlock()
	env := append(rt.acct.BaseEnv(), seatEnv(rt, c.ws, creds.GitConfig(), extra...)...)

	// Everything runs as the workspace user through the helper: with a
	// terminal as pty-shell (the helper opens the PTY and makes the
	// workspace user own it), without one as ssh-session over pipes.
	var proc helper.Process
	var outputDone sync.WaitGroup
	var ptmx *os.File
	if usePTY {
		proc, err = s.helper.Spawn(ctx, c.ws.ID, helper.SpawnSpec{Kind: helper.KindPTYShell, Argv: argv, Env: env,
			Dir: rt.p.Tree, Cols: cols, Rows: rows})
		if err != nil {
			refuse(ch, "start: "+err.Error())
			return
		}
		ptmx = proc.PTY()
		defer ptmx.Close()
		st.setPTY(ptmx)
		go io.Copy(ptmx, ch)
		outputDone.Add(1)
		go func() { defer outputDone.Done(); io.Copy(ch, ptmx) }()
	} else {
		var pipes [6]*os.File // child stdin, our stdin; our stdout, child stdout; our stderr, child stderr
		for i := 0; i < 6; i += 2 {
			r, w, err := os.Pipe()
			if err != nil {
				closeFiles(pipes[:i]...)
				refuse(ch, "pipes failed")
				return
			}
			pipes[i], pipes[i+1] = r, w
		}
		inR, inW, outR, outW, errR, errW := pipes[0], pipes[1], pipes[2], pipes[3], pipes[4], pipes[5]
		proc, err = s.helper.Spawn(ctx, c.ws.ID, helper.SpawnSpec{Kind: helper.KindSSHSession, Argv: argv, Env: env,
			Dir: rt.p.Tree, Stdin: inR, Stdout: outW, Stderr: errW})
		closeFiles(inR, outW, errW) // the child has its own copies now
		if err != nil {
			closeFiles(inW, outR, errR)
			refuse(ch, "start: "+err.Error())
			return
		}
		go func() { io.Copy(inW, ch); inW.Close() }()
		outputDone.Add(2)
		go func() { defer outputDone.Done(); io.Copy(ch, outR); outR.Close() }()
		go func() { defer outputDone.Done(); io.Copy(ch.Stderr(), errR); errR.Close() }()
	}
	s.event(c.ws.ID, "device", c.deviceID, "ssh.session", map[string]any{"kind": kind, "pty": usePTY, "pid": proc.Pid(), "handle": proc.Handle()})

	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proc.Signal(syscall.SIGHUP)
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				proc.Signal(syscall.SIGKILL)
			}
		case <-exited:
		}
	}()
	var es helper.ExitStatus
	var waitErr error
	if usePTY {
		es, waitErr = proc.Wait()
		close(exited)
		// Let the last output drain; a background job holding the tty must
		// not keep the session open.
		drained := make(chan struct{})
		go func() { outputDone.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(200 * time.Millisecond):
			ptmx.Close()
		}
	} else {
		es, waitErr = proc.Wait()
		close(exited)
		// The pipes reach EOF once every holder of the write ends (the
		// process and its children) is gone; a daemonised child must not
		// keep the session open forever.
		drained := make(chan struct{})
		go func() { outputDone.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-ctx.Done():
		}
	}
	code := es.ShellCode()
	if waitErr != nil || code < 0 {
		code = 255
	}
	exitStatus(ch, code)
	ch.CloseWrite()
	ch.Close()
}

func closeFiles(fs ...*os.File) {
	for _, f := range fs {
		if f != nil {
			f.Close()
		}
	}
}

// clampSize turns an SSH terminal dimension into a PTY one.
func clampSize(n uint32) uint16 {
	if n > 0xffff {
		return 0xffff
	}
	return uint16(n)
}

// directTCPIP is local port forwarding (ssh -L, VS Code port forwards). It
// reaches the server's loopback only; per-workspace network isolation is a
// later phase, so today every workspace shares the same loopback.
func (d *sshServer) directTCPIP(c *sshConn, nch ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad request")
		return
	}
	var host string
	switch strings.Trim(strings.ToLower(p.Host), "[]") {
	case "localhost", "127.0.0.1":
		host = "127.0.0.1"
	case "::1":
		host = "::1"
	default:
		nch.Reject(ssh.Prohibited, "port forwarding is limited to the workspace's loopback (127.0.0.1, ::1)")
		return
	}
	if p.Port == 0 || p.Port > 65535 {
		nch.Reject(ssh.ConnectionFailed, "bad port")
		return
	}
	if !d.s.serverHoldsLease(c.ws.ID) {
		nch.Reject(ssh.Prohibited, leaseRefusal)
		return
	}
	tc, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(p.Port))), 10*time.Second)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		tc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	done := make(chan struct{}, 2)
	go func() { io.Copy(tc, ch); tc.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(ch, tc); ch.CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
	tc.Close()
	ch.Close()
}

// sshPublicAddr is host:port as clients reach the SSH endpoint.
func (s *Server) sshPublicAddr() string {
	if s.cfg.SSH.PublicAddr != "" {
		return s.cfg.SSH.PublicAddr
	}
	host := "localhost"
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	port := "2222"
	if s.sshd != nil {
		if _, p, err := net.SplitHostPort(s.sshd.ln.Addr().String()); err == nil {
			port = p
		}
	} else if _, p, err := net.SplitHostPort(s.cfg.SSH.Listen); err == nil {
		port = p
	}
	return net.JoinHostPort(host, port)
}

// handleSSHInfo tells `armageddon ssh-config` where the endpoint is and
// which host key to pin.
func (s *Server) handleSSHInfo(rw http.ResponseWriter, r *http.Request) {
	if s.sshd == nil {
		writeJSON(rw, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(rw, 200, map[string]any{"enabled": true, "addr": s.sshPublicAddr(),
		"host_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.sshd.hostKey)))})
}

// SFTPServerMain is `armageddon sftp-server`: an SFTP server on stdin and
// stdout, started by the SSH endpoint as the workspace user, so every file
// operation has exactly that user's permissions.
func SFTPServerMain() error {
	wd, _ := os.Getwd()
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{os.Stdin, os.Stdout}, sftp.WithServerWorkingDirectory(wd))
	if err != nil {
		return err
	}
	if err := srv.Serve(); err != nil && err != io.EOF {
		return err
	}
	return nil
}
