package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/components"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Browser IDE (contract §2.3, plan M4.3): one code-server per workspace,
// started by the helper (SpawnInWorkspace, kind code-server) as the
// workspace user, listening on a unix socket in the workspace's run/
// directory with --auth none. run/ is owned by the workspace user with the
// server's group (mode 2750, created by PrepareWorkspaceDirs) and the socket
// is 0660, so the server can connect and other workspace users cannot even
// reach the name; every connection still checks with SO_PEERCRED that the
// listener is the supervised process. The only way to reach
// it is the authenticated proxy below: browser session, workspace
// membership, the server holding the lease, same-origin WebSockets.
//
// An instance starts on the first request, stops after the idle timeout or
// when its session is closed (lease handoff, §4.3), and is restarted by its
// supervisor if it crashes, up to ideRestartLimit times per
// ideRestartWindow.

const (
	ideRestartLimit  = 3
	ideRestartWindow = 5 * time.Minute
	ideFailCooldown  = time.Minute
	ideStartTimeout  = 30 * time.Second
	ideStopGrace     = 3 * time.Second
)

type ideManager struct {
	s    *Server
	idle time.Duration // stop an instance after this long without traffic
	tick time.Duration // how often the reaper looks

	mu   sync.Mutex
	byWS map[string]*ideInstance
}

func newIDEManager(s *Server) *ideManager {
	idle := time.Duration(s.cfg.CodeServer.IdleTimeoutMin) * time.Minute
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	return &ideManager{s: s, idle: idle, tick: time.Minute, byWS: map[string]*ideInstance{}}
}

type ideInstance struct {
	m      *ideManager
	rt     *runtime
	wsID   string
	sock   string
	prefix string // public path prefix, /api/workspaces/<id>/ide
	ctx    context.Context
	cancel context.CancelFunc // aborts in-flight proxied requests on stop
	done   chan struct{}      // closed when the supervisor has exited
	proxy  *httputil.ReverseProxy

	mu         sync.Mutex
	pid        int
	proc       helper.Process // the current code-server, nil between runs
	ready      chan struct{}  // closed once the current process serves (or failed to)
	readyErr   error
	failed     error
	failedAt   time.Time
	stopping   bool
	active     int
	lastActive time.Time
	conns      map[*wsBridge]struct{}
	unregister func()
	output     tailBuffer
}

// get returns a serving instance for the workspace, starting one if needed.
func (m *ideManager) get(ctx context.Context, rt *runtime, w *store.Workspace, userID string) (*ideInstance, error) {
	for {
		m.mu.Lock()
		inst := m.byWS[w.ID]
		if inst != nil {
			inst.mu.Lock()
			failed, failedAt, stopping := inst.failed, inst.failedAt, inst.stopping
			inst.mu.Unlock()
			switch {
			case failed != nil && time.Since(failedAt) < ideFailCooldown:
				m.mu.Unlock()
				return nil, failed
			case failed != nil:
				delete(m.byWS, w.ID)
				inst = nil
			case stopping:
				select {
				case <-inst.done:
					delete(m.byWS, w.ID)
					inst = nil
				default:
					// Wait for the old process to go before starting a new
					// one on the same socket.
					m.mu.Unlock()
					select {
					case <-inst.done:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					continue
				}
			}
		}
		if inst == nil {
			var err error
			inst, err = m.start(rt, w, userID)
			if err != nil {
				m.mu.Unlock()
				return nil, err
			}
			m.byWS[w.ID] = inst
		}
		m.mu.Unlock()
		return inst, inst.waitReady(ctx)
	}
}

func (m *ideManager) start(rt *runtime, w *store.Workspace, userID string) (*ideInstance, error) {
	exe, err := components.ResolveCodeServer(m.s.cfg.CodeServer.Path, m.s.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := m.s.ensureRunDir(rt); err != nil {
		return nil, err
	}
	sock := filepath.Join(rt.p.Run, "ide.sock")
	if len(sock) > 100 {
		return nil, fmt.Errorf("code-server socket path too long (%d bytes): use a shorter data directory", len(sock))
	}
	userData := filepath.Join(rt.p.Home, ".local", "share", "code-server")
	for _, d := range []string{filepath.Join(rt.p.Home, ".local"), filepath.Join(rt.p.Home, ".local", "share"), userData, filepath.Join(userData, "extensions")} {
		if _, err := os.Stat(d); err != nil {
			if err := rt.acct.MkdirOwned(d, 0o700); err != nil {
				return nil, err
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	inst := &ideInstance{m: m, rt: rt, wsID: w.ID, sock: sock, prefix: "/api/workspaces/" + w.ID + "/ide",
		ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}),
		lastActive: time.Now(), conns: map[*wsBridge]struct{}{}}
	inst.proxy = inst.newReverseProxy()
	// Every running instance is an interactive server-seat session: a lease
	// handoff closes it (contract §4.3).
	inst.unregister = m.s.sessions.Register(w.ID, SessionCodeServer, userID, inst.stop)
	argv := []string{exe, "--socket", sock, "--socket-mode", "660", "--auth", "none",
		"--user-data-dir", userData, "--extensions-dir", filepath.Join(userData, "extensions"),
		"--disable-telemetry", "--disable-update-check", "--disable-workspace-trust",
		// code-server's own /proxy/<port> would serve arbitrary local
		// apps on the Armageddon origin; ports go through SSH instead.
		"--disable-proxy",
		rt.p.Tree}
	env := append(rt.acct.BaseEnv(), seatEnv(rt, w, nil)...)
	spawn := func() (helper.Process, error) {
		// A stale socket from an earlier run: only the workspace user can
		// remove it (the server may not write in run/).
		rm := rt.acct.Command(rt.p.Tree, "rm", "-f", "--", sock)
		if out, err := rm.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("remove the old code-server socket: %v: %s", err, strings.TrimSpace(string(out)))
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		// SpawnInWorkspace(kind=code-server): the helper checks that exe is
		// the configured code-server and that no workspace can replace it.
		proc, err := m.s.helper.Spawn(context.Background(), w.ID, helper.SpawnSpec{Kind: helper.KindCodeServer,
			Argv: argv, Env: env, Dir: rt.p.Tree, Stdout: pw, Stderr: pw})
		pw.Close()
		if err != nil {
			pr.Close()
			return nil, err
		}
		go func() { io.Copy(&inst.output, pr); pr.Close() }()
		return proc, nil
	}
	go inst.supervise(spawn, userID)
	return inst, nil
}

// ensureRunDir makes sure run/ exists with the layout above. Workspaces
// created before Phase 5 have none; the helper creates it (the server
// cannot: the directory belongs to the workspace user).
func (s *Server) ensureRunDir(rt *runtime) error {
	if fi, err := os.Stat(rt.p.Run); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !rt.acct.Isolated() || ok && st.Uid == rt.acct.UID && fi.Mode()&os.ModeSetgid != 0 && fi.Mode().Perm() == 0o750 {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.helper.PrepareWorkspaceDirs(ctx, rt.id)
}

// supervise runs code-server, restarting it after crashes within limits.
func (inst *ideInstance) supervise(spawn func() (helper.Process, error), userID string) {
	defer close(inst.done)
	var crashes []time.Time
	for {
		inst.mu.Lock()
		if inst.stopping {
			inst.mu.Unlock()
			return
		}
		ready := inst.ready
		inst.mu.Unlock()

		proc, err := spawn()
		if err != nil {
			inst.mu.Lock()
			inst.readyErr = err
			close(ready)
			inst.mu.Unlock()
			inst.fail(fmt.Errorf("start code-server: %w", err))
			return
		}
		inst.mu.Lock()
		inst.pid, inst.proc = proc.Pid(), proc
		stopping := inst.stopping
		inst.mu.Unlock()
		if stopping { // stop raced with the start: it saw no process to signal
			proc.Signal(syscall.SIGKILL)
		}
		exited := make(chan struct{})
		var waitErr error
		go func() {
			st, err := proc.Wait()
			if err == nil {
				err = fmt.Errorf("exit status %d", st.ShellCode())
			}
			waitErr = err
			close(exited)
		}()

		err = inst.waitSocket(exited)
		inst.mu.Lock()
		inst.readyErr = err
		close(ready)
		inst.mu.Unlock()
		if err == nil {
			inst.m.s.event(inst.wsID, "user", userID, "ide.started", map[string]any{"pid": proc.Pid(), "handle": proc.Handle()})
		} else {
			proc.Signal(syscall.SIGKILL)
		}
		<-exited

		inst.mu.Lock()
		inst.proc = nil
		stopping = inst.stopping
		inst.mu.Unlock()
		if stopping {
			return
		}
		now := time.Now()
		kept := crashes[:0]
		for _, t := range crashes {
			if now.Sub(t) < ideRestartWindow {
				kept = append(kept, t)
			}
		}
		crashes = append(kept, now)
		reason := fmt.Sprintf("code-server exited (%v): %s", waitErr, inst.output.String())
		if err != nil {
			reason = err.Error()
		}
		log.Printf("workspace %s: %s", inst.wsID, reason)
		if len(crashes) >= ideRestartLimit {
			inst.fail(fmt.Errorf("code-server failed %d times in %v; last: %s", len(crashes), ideRestartWindow, reason))
			return
		}
		// Back off, then serve the next request from a fresh process.
		inst.mu.Lock()
		inst.ready = make(chan struct{})
		inst.mu.Unlock()
		select {
		case <-time.After(time.Duration(len(crashes)) * time.Second):
		case <-inst.ctx.Done():
		}
	}
}

// waitSocket waits until code-server accepts connections on its socket.
func (inst *ideInstance) waitSocket(exited <-chan struct{}) error {
	deadline := time.After(ideStartTimeout)
	for {
		if c, err := inst.dial(context.Background()); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("code-server exited during startup: %s", inst.output.String())
		case <-deadline:
			return fmt.Errorf("code-server did not start within %v: %s", ideStartTimeout, inst.output.String())
		case <-inst.ctx.Done():
			return errors.New("code-server stopped")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (inst *ideInstance) waitReady(ctx context.Context) error {
	for {
		inst.mu.Lock()
		ready, failed := inst.ready, inst.failed
		inst.mu.Unlock()
		if failed != nil {
			return failed
		}
		select {
		case <-ready:
		case <-inst.done:
			inst.mu.Lock()
			defer inst.mu.Unlock()
			if inst.failed != nil {
				return inst.failed
			}
			return errors.New("code-server stopped")
		case <-ctx.Done():
			return ctx.Err()
		}
		inst.mu.Lock()
		err, again := inst.readyErr, inst.ready != ready
		inst.mu.Unlock()
		if err == nil {
			return nil
		}
		if !again {
			// Wait for the supervisor to set up the restart.
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// dial connects to code-server's socket and checks that the listener is the
// supervised process (or its direct child: code-server forks its HTTP
// server) running as the workspace user. A workspace process that replaced
// the socket would otherwise be served to every member as "the IDE".
func (inst *ideInstance) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", inst.sock)
	if err != nil {
		return nil, err
	}
	if !peerCheckSupported {
		return c, nil
	}
	pid, uid, err := socketPeer(c)
	inst.mu.Lock()
	want := inst.pid
	inst.mu.Unlock()
	if err != nil || uid != inst.rt.acct.UID || (pid != want && parentPID(pid) != want) {
		c.Close()
		err := fmt.Errorf("code-server socket is served by pid %d uid %d, not the supervised process %d", pid, uid, want)
		log.Printf("workspace %s: refused: %v", inst.wsID, err)
		inst.m.s.event(inst.wsID, "server", "", "ide.socket_refused", map[string]any{"peer_pid": pid, "peer_uid": uid})
		return nil, err
	}
	return c, nil
}

func (inst *ideInstance) fail(err error) {
	inst.mu.Lock()
	inst.failed, inst.failedAt = err, time.Now()
	inst.mu.Unlock()
	log.Printf("workspace %s: %v", inst.wsID, err)
	inst.m.s.event(inst.wsID, "server", "", "ide.failed", map[string]string{"error": err.Error()})
	// Called from the supervisor: the process is gone or being killed, and
	// waiting for inst.done here would wait for ourselves.
	inst.shutdown("the IDE failed: "+err.Error(), false)
}

// stop ends the instance: open IDE connections close with reason, then the
// process group gets SIGTERM and, after a grace period, SIGKILL. It is the
// instance's session-registry close function.
func (inst *ideInstance) stop(reason string) {
	if inst.shutdown(reason, true) {
		inst.m.s.event(inst.wsID, "server", "", "ide.stopped", map[string]string{"reason": reason})
	}
	inst.m.mu.Lock()
	if inst.m.byWS[inst.wsID] == inst {
		inst.mu.Lock()
		failed := inst.failed
		inst.mu.Unlock()
		if failed == nil {
			delete(inst.m.byWS, inst.wsID)
		}
	}
	inst.m.mu.Unlock()
}

func (inst *ideInstance) shutdown(reason string, wait bool) bool {
	inst.mu.Lock()
	if inst.stopping {
		inst.mu.Unlock()
		return false
	}
	inst.stopping = true
	proc := inst.proc
	var conns []*wsBridge
	for c := range inst.conns {
		conns = append(conns, c)
	}
	inst.mu.Unlock()
	inst.cancel()
	for _, c := range conns {
		c.closeWithReason(reason)
	}
	inst.unregister()
	// The process group is signalled through the helper: the server has
	// no kill capability over workspace processes (§2.5).
	if proc != nil && !wait {
		proc.Signal(syscall.SIGKILL)
	} else if proc != nil {
		proc.Signal(syscall.SIGTERM)
		select {
		case <-inst.done:
		case <-time.After(ideStopGrace):
			proc.Signal(syscall.SIGKILL)
			select {
			case <-inst.done:
			case <-time.After(2 * time.Second):
			}
		}
	}
	return true
}

func (m *ideManager) stopAll(reason string) {
	m.mu.Lock()
	var all []*ideInstance
	for _, inst := range m.byWS {
		all = append(all, inst)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, inst := range all {
		wg.Add(1)
		go func() { defer wg.Done(); inst.stop(reason) }()
	}
	wg.Wait()
}

// reap stops instances idle for longer than m.idle.
func (m *ideManager) reap(ctx context.Context) {
	t := time.NewTicker(m.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.reapOnce()
	}
}

func (m *ideManager) reapOnce() {
	m.mu.Lock()
	var idle []*ideInstance
	for _, inst := range m.byWS {
		inst.mu.Lock()
		if inst.active == 0 && !inst.stopping && time.Since(inst.lastActive) > m.idle {
			idle = append(idle, inst)
		}
		inst.mu.Unlock()
	}
	m.mu.Unlock()
	for _, inst := range idle {
		go inst.stop(fmt.Sprintf("the IDE was stopped after %v without use", m.idle))
	}
}

func (inst *ideInstance) begin() {
	inst.mu.Lock()
	inst.active++
	inst.lastActive = time.Now()
	inst.mu.Unlock()
}

func (inst *ideInstance) end() {
	inst.mu.Lock()
	inst.active--
	inst.lastActive = time.Now()
	inst.mu.Unlock()
}

// running reports whether a workspace has a live code-server (for tests and
// the UI).
func (m *ideManager) running(wsID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.byWS[wsID]
	if inst == nil {
		return false
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return !inst.stopping && inst.failed == nil
}

// ---- the proxy ----

var idePathRe = regexp.MustCompile(`^/api/workspaces/[^/]+/ide(/|$)`)

func isIDEPath(p string) bool { return idePathRe.MatchString(p) }

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// handleIDE serves /api/workspaces/{id}/ide/… (requireUser + member have
// already run: anonymous callers got 401, non-members 404).
func (s *Server) handleIDE(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	if sessionOf(r) == nil {
		writeErr(rw, 401, "the IDE is opened from a browser session")
		return
	}
	prefix := "/api/workspaces/" + w.ID + "/ide"
	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if rest == r.URL.EscapedPath() || !strings.HasPrefix(rest, "/") {
		http.Redirect(rw, r, prefix+"/", http.StatusFound)
		return
	}
	upgrade := isWebSocketUpgrade(r)
	if upgrade && !sameOrigin(r) {
		writeErr(rw, 403, "cross-origin WebSocket refused")
		return
	}
	if !s.serverHoldsLease(w.ID) {
		writeErr(rw, 409, leaseRefusal)
		return
	}
	rt := s.runtimeFor(w.ID)
	if rt == nil {
		writeErr(rw, 503, "workspace not running")
		return
	}
	inst, err := s.ide.get(r.Context(), rt, w, userOf(r).ID)
	if err != nil {
		writeErr(rw, 503, "the IDE is unavailable: "+err.Error())
		return
	}
	inst.begin()
	defer inst.end()
	rw.Header().Del("X-Frame-Options") // code-server sends SAMEORIGIN (webviews)
	if upgrade {
		inst.proxyWebSocket(rw, r, rest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopWatch := context.AfterFunc(inst.ctx, cancel)
	defer stopWatch()
	inst.proxy.ServeHTTP(rw, r.WithContext(context.WithValue(ctx, ideRestKey{}, rest)))
}

type ideRestKey struct{}

func (inst *ideInstance) newReverseProxy() *httputil.ReverseProxy {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return inst.dial(ctx) },
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest, _ := pr.In.Context().Value(ideRestKey{}).(string)
			setBackendURL(pr.Out.URL, rest, pr.In.URL.RawQuery)
			pr.Out.Host = pr.In.Host // code-server checks Origin against Host
			pr.SetXForwarded()
			scrubIDERequest(pr.Out.Header)
		},
		Transport:     tr,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			scrubIDEResponse(resp.Header, inst.prefix)
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			if inst.ctx.Err() != nil {
				writeErr(rw, 503, "the IDE was stopped")
				return
			}
			writeErr(rw, 502, "the IDE is not responding")
		},
	}
}

func setBackendURL(u *url.URL, rest, query string) {
	u.Scheme, u.Host = "http", "code-server"
	u.RawPath = rest
	if p, err := url.PathUnescape(rest); err == nil {
		u.Path = p
	} else {
		u.Path = rest
	}
	u.RawQuery = query
}

// scrubIDERequest removes Armageddon credentials before a request reaches
// code-server, which runs as (and can be replaced by) the workspace user.
func scrubIDERequest(h http.Header) {
	h.Del("Authorization")
	h.Del("X-Csrf-Token")
	var keep []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name == "" || strings.HasPrefix(name, "arm_") {
				continue
			}
			keep = append(keep, strings.TrimSpace(part))
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

// scrubIDEResponse stops code-server from setting Armageddon cookies, lets
// it frame itself (webviews), and keeps absolute redirects under the prefix.
func scrubIDEResponse(h http.Header, prefix string) {
	var keep []string
	for _, sc := range h.Values("Set-Cookie") {
		name, _, _ := strings.Cut(sc, "=")
		if !strings.HasPrefix(strings.TrimSpace(name), "arm_") {
			keep = append(keep, sc)
		}
	}
	h.Del("Set-Cookie")
	for _, sc := range keep {
		h.Add("Set-Cookie", sc)
	}
	h.Set("X-Frame-Options", "SAMEORIGIN")
	if loc := h.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && !strings.HasPrefix(loc, prefix+"/") {
		h.Set("Location", prefix+loc)
	}
}

// proxyWebSocket bridges a WebSocket to code-server. Frames from code-server
// are copied whole, so closing the bridge can always end with a proper close
// frame carrying the reason.
func (inst *ideInstance) proxyWebSocket(rw http.ResponseWriter, r *http.Request, rest string) {
	bc, err := inst.dial(r.Context())
	if err != nil {
		writeErr(rw, 502, "the IDE is not responding")
		return
	}
	out := &http.Request{Method: r.Method, URL: &url.URL{}, Header: r.Header.Clone(), Host: r.Host,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	setBackendURL(out.URL, rest, r.URL.RawQuery)
	out.URL.Scheme, out.URL.Host = "", ""
	scrubIDERequest(out.Header)
	out.Header.Set("X-Forwarded-Host", r.Host)
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		out.Header.Set("X-Forwarded-For", host)
	}
	bc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := out.Write(bc); err != nil {
		bc.Close()
		writeErr(rw, 502, "the IDE is not responding")
		return
	}
	br := bufio.NewReader(bc)
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		bc.Close()
		writeErr(rw, 502, "the IDE is not responding")
		return
	}
	bc.SetDeadline(time.Time{})
	scrubIDEResponse(resp.Header, inst.prefix)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer bc.Close()
		for k, v := range resp.Header {
			rw.Header()[k] = v
		}
		rw.WriteHeader(resp.StatusCode)
		io.Copy(rw, resp.Body)
		return
	}
	cc, cbuf, err := http.NewResponseController(rw).Hijack()
	if err != nil {
		bc.Close()
		return
	}
	fmt.Fprintf(cc, "HTTP/1.1 101 Switching Protocols\r\n")
	resp.Header.Write(cc)
	io.WriteString(cc, "\r\n")

	b := &wsBridge{client: cc, backend: bc}
	inst.mu.Lock()
	if inst.stopping {
		inst.mu.Unlock()
		b.closeWithReason("the IDE was stopped")
		return
	}
	inst.conns[b] = struct{}{}
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		delete(inst.conns, b)
		inst.mu.Unlock()
	}()
	done := make(chan struct{}, 2)
	go func() { io.Copy(bc, cbuf); done <- struct{}{} }() // browser → code-server, verbatim
	go func() { b.pumpBackend(br); done <- struct{}{} }()
	<-done
	b.closeWithReason("")
	<-done
}

// wsBridge is one proxied IDE WebSocket.
type wsBridge struct {
	client, backend net.Conn
	mu              sync.Mutex
	closed          bool
	midFrame        bool // a frame is partly written to the client
}

// pumpBackend copies whole WebSocket frames from code-server to the browser.
func (b *wsBridge) pumpBackend(br *bufio.Reader) {
	var hdr [14]byte
	buf := make([]byte, 32*1024)
	write := func(p []byte, mid bool) bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed {
			return false
		}
		b.midFrame = mid
		_, err := b.client.Write(p)
		return err == nil
	}
	for {
		if _, err := io.ReadFull(br, hdr[:2]); err != nil {
			return
		}
		n, plen := 2, uint64(hdr[1]&0x7f)
		switch plen {
		case 126:
			if _, err := io.ReadFull(br, hdr[2:4]); err != nil {
				return
			}
			plen, n = uint64(binary.BigEndian.Uint16(hdr[2:4])), 4
		case 127:
			if _, err := io.ReadFull(br, hdr[2:10]); err != nil {
				return
			}
			plen, n = binary.BigEndian.Uint64(hdr[2:10]), 10
		}
		if hdr[1]&0x80 != 0 { // masked (servers should not, but copy it faithfully)
			if _, err := io.ReadFull(br, hdr[n:n+4]); err != nil {
				return
			}
			n += 4
		}
		if !write(hdr[:n], plen > 0) {
			return
		}
		for plen > 0 {
			chunk := buf
			if uint64(len(chunk)) > plen {
				chunk = chunk[:plen]
			}
			k, err := br.Read(chunk)
			if k > 0 {
				plen -= uint64(k)
				if !write(chunk[:k], plen > 0) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
}

// closeWithReason ends the bridge. With a reason, the browser gets a close
// frame (1001 going away) carrying it, unless a frame is half-written, in
// which case only the connection is dropped.
func (b *wsBridge) closeWithReason(reason string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	if reason != "" && !b.midFrame {
		if len(reason) > 123 {
			reason = reason[:123]
		}
		frame := []byte{0x88, byte(2 + len(reason)), 0x03, 0xe9} // FIN|close, 1001
		frame = append(frame, reason...)
		b.client.SetWriteDeadline(time.Now().Add(time.Second))
		b.client.Write(frame)
	}
	b.mu.Unlock()
	b.backend.Close()
	b.client.Close()
}

// tailBuffer keeps the last 4 KiB of a process's output for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.buf))
	if len(s) > 600 {
		s = "…" + s[len(s)-600:]
	}
	return s
}
