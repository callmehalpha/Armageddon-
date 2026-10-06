package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Every IDE route, every method, refuses callers without a browser session
// and non-members, and never starts or reaches code-server for them.
func TestIDERefusesUnauthenticatedAndNonMembers(t *testing.T) {
	h := newHarness(t)
	owner, stranger := h.user("owner"), h.user("stranger")
	w := h.workspace(owner, "demo")
	other := h.workspace(stranger, "theirs")
	base := "/api/workspaces/" + w.ID + "/ide"

	// A device token of the owner: authenticated, member, but not a browser
	// session.
	devTok := ids.Secret(32)
	devID := ids.New()
	now := store.Now()
	if _, err := h.s.store.DB().Exec(`INSERT INTO devices (id, user_id, name, public_key, created_at, last_seen_at) VALUES (?, ?, 'laptop', ?, ?, ?)`,
		devID, owner.ID, "pk-"+devID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := h.s.store.CreateDeviceToken(ids.Hash(devTok), devID, now+60000); err != nil {
		t.Fatal(err)
	}

	paths := []string{"/", "/echo", "/env", "/ws", "/static/app.js", "/setcookie", "/crash", "/a/../echo", "/%2e%2e/echo", "/stable-abc/static/out/vs/code.js"}
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD"}
	wsHdr := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Origin": h.ts.URL}
	type probe struct {
		who  string
		u    *testUser
		hdr  map[string]string
		want []int
	}
	probes := []probe{
		{"anonymous", nil, nil, []int{401}},
		{"anonymous websocket", nil, wsHdr, []int{401}},
		{"non-member", stranger, map[string]string{"Origin": h.ts.URL}, []int{404}},
		{"non-member websocket", stranger, wsHdr, []int{404}},
		{"member device token", nil, map[string]string{"Authorization": "Bearer " + devTok}, []int{401}},
		{"bogus cookie", &testUser{cookie: "nope"}, nil, []int{401}},
	}
	for _, p := range probes {
		for _, m := range methods {
			for _, sub := range append(paths, "") {
				code, body, _ := h.do(p.u, m, base+sub, p.hdr, "")
				ok := false
				for _, c := range p.want {
					ok = ok || code == c
				}
				// The mux redirects /ide and unclean paths before auth;
				// a redirect never reaches code-server.
				if code == 301 || code == 307 || code == 308 {
					ok = true
				}
				if !ok {
					t.Errorf("%s %s %s: %d %q, want %v", p.who, m, base+sub, code, body, p.want)
				}
				if strings.Contains(body, "fake") {
					t.Errorf("%s %s %s reached code-server: %q", p.who, m, base+sub, body)
				}
			}
		}
	}
	// A member probing another workspace's IDE gets 404.
	if code, _, _ := h.do(owner, "GET", "/api/workspaces/"+other.ID+"/ide/", nil, ""); code != 404 {
		t.Errorf("member of another workspace: %d, want 404", code)
	}
	if h.s.ide.running(w.ID) || h.s.ide.running(other.ID) {
		t.Fatal("code-server was started by a refused request")
	}
	if n := h.s.sessions.Count(w.ID)[SessionCodeServer]; n != 0 {
		t.Fatalf("%d code-server sessions registered", n)
	}
}

func TestIDEProxy(t *testing.T) {
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "demo")
	rt := h.s.runtimeFor(w.ID)
	base := "/api/workspaces/" + w.ID + "/ide"

	start := time.Now()
	code, body, hdr := h.do(owner, "GET", base+"/", nil, "")
	if code != 200 || !strings.Contains(body, "fake code-server folder="+rt.p.Tree) {
		t.Fatalf("GET ide/: %d %q", code, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("on-demand start took %v", d)
	}
	if xfo := hdr.Values("X-Frame-Options"); len(xfo) != 1 || xfo[0] != "SAMEORIGIN" {
		t.Fatalf("X-Frame-Options = %v", xfo)
	}
	if h.s.sessions.Count(w.ID)[SessionCodeServer] != 1 {
		t.Fatal("running instance not registered with the session registry")
	}

	// Runs as the workspace user, with its data under the workspace home.
	_, body, _ = h.do(owner, "GET", base+"/env", nil, "")
	var env struct {
		UID          int      `json:"uid"`
		Home         string   `json:"home"`
		UserDataDir  string   `json:"user_data_dir"`
		ExtensionDir string   `json:"extensions_dir"`
		Env          []string `json:"env"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(body)
	}
	if env.UID != int(rt.acct.UID) || env.Home != rt.p.Home || !strings.HasPrefix(env.UserDataDir, rt.p.Home+"/") || !strings.HasPrefix(env.ExtensionDir, rt.p.Home+"/") {
		t.Fatalf("code-server env: %+v (want uid %d, home %s)", env, rt.acct.UID, rt.p.Home)
	}
	if fi, err := os.Stat(filepath.Join(rt.p.Run, "ide.sock")); err != nil || fi.Mode().Perm()&0o007 != 0 {
		t.Fatalf("socket: %v %v", fi, err)
	}

	// Armageddon credentials are stripped; other cookies and Host pass.
	req := map[string]string{"Cookie": "arm_session=" + owner.cookie + "; cs_pref=dark; arm_other=x", "Authorization": "Bearer abc",
		"X-CSRF-Token": "t"}
	code, body, _ = h.doRaw(owner, "GET", base+"/echo?x=1", req)
	var echo struct {
		Path, Query, Host string
		Headers           http.Header
	}
	json.Unmarshal([]byte(body), &echo)
	if code != 200 || echo.Path != "/echo" || echo.Query != "x=1" || echo.Host != strings.TrimPrefix(h.ts.URL, "http://") {
		t.Fatalf("echo: %d %+v", code, echo)
	}
	if c := strings.Join(echo.Headers["Cookie"], ";"); strings.Contains(c, "arm_") || strings.Contains(c, owner.cookie) || !strings.Contains(c, "cs_pref=dark") {
		t.Fatalf("cookies forwarded to code-server: %q", c)
	}
	if echo.Headers.Get("Authorization") != "" || echo.Headers.Get("X-Csrf-Token") != "" {
		t.Fatalf("credentials forwarded: %v", echo.Headers)
	}
	// code-server cannot set Armageddon cookies.
	_, _, hdr = h.do(owner, "GET", base+"/setcookie", nil, "")
	sc := strings.Join(hdr.Values("Set-Cookie"), "\n")
	if strings.Contains(sc, "arm_session") || !strings.Contains(sc, "cs_pref") {
		t.Fatalf("Set-Cookie passed: %q", sc)
	}
	// Unsafe methods need a same-origin Origin.
	if code, _, _ := h.do(owner, "POST", base+"/echo", nil, "x"); code != 403 {
		t.Fatalf("POST without Origin: %d", code)
	}
	if code, _, _ := h.do(owner, "POST", base+"/echo", map[string]string{"Origin": "https://evil.example"}, "x"); code != 403 {
		t.Fatalf("cross-origin POST: %d", code)
	}
	if code, body, _ := h.do(owner, "POST", base+"/echo", map[string]string{"Origin": h.ts.URL}, "x"); code != 200 || !strings.Contains(body, `"body":"x"`) {
		t.Fatalf("same-origin POST: %d %s", code, body)
	}

	// WebSockets: same origin only, then bridged both ways.
	wsURL := "ws" + strings.TrimPrefix(h.ts.URL, "http") + base + "/ws"
	hh := http.Header{"Cookie": {sessionCookie + "=" + owner.cookie}, "Origin": {"https://evil.example"}}
	if _, resp, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{HTTPHeader: hh}); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-origin websocket: %v %v", err, resp)
	}
	c := h.dialIDE(owner, wsURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, m, err := c.Read(ctx); err != nil || string(m) != "hello" {
		t.Fatalf("ws hello: %q %v", m, err)
	}
	c.Write(ctx, websocket.MessageText, []byte("ping"))
	if _, m, err := c.Read(ctx); err != nil || string(m) != "ping" {
		t.Fatalf("ws echo: %q %v", m, err)
	}
	big := []byte(strings.Repeat("x", 200000))
	c.Write(ctx, websocket.MessageBinary, big)
	if _, m, err := c.Read(ctx); err != nil || len(m) != len(big) {
		t.Fatalf("ws big echo: %d %v", len(m), err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// Without the lease the IDE is refused.
	h.setLease(w.ID, "device")
	if code, _, _ := h.do(owner, "GET", base+"/", nil, ""); code != 409 {
		t.Fatalf("IDE without lease: %d", code)
	}
}

func (h *harness) doRaw(u *testUser, method, path string, hdr map[string]string) (int, string, http.Header) {
	// Like do, but the caller controls the Cookie header completely.
	h.t.Helper()
	return h.do(nil, method, path, hdr, "")
}

func (h *harness) dialIDE(u *testUser, url string) *websocket.Conn {
	h.t.Helper()
	hh := http.Header{"Cookie": {sessionCookie + "=" + u.cookie}, "Origin": {h.ts.URL}}
	c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: hh})
	if err != nil {
		h.t.Fatalf("dial %s: %v", url, err)
	}
	c.SetReadLimit(1 << 24)
	return c
}

// M4.6: closing the workspace's sessions (what a lease handoff does) closes
// an open IDE connection with the reason within 2 s and stops code-server.
func TestIDEClosedOnHandoff(t *testing.T) {
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "demo")
	base := "/api/workspaces/" + w.ID + "/ide"
	c := h.dialIDE(owner, "ws"+strings.TrimPrefix(h.ts.URL, "http")+base+"/ws")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, m, err := c.Read(ctx); err != nil || string(m) != "hello" {
		t.Fatalf("hello: %q %v", m, err)
	}
	pid := h.s.ide.byWS[w.ID].pid

	start := time.Now()
	go h.s.sessions.CloseAll(w.ID, "the workspace was handed to laptop")
	_, _, err := c.Read(ctx)
	elapsed := time.Since(start)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusGoingAway || !strings.Contains(ce.Reason, "handed to laptop") {
		t.Fatalf("read after CloseAll: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("IDE connection closed after %v", elapsed)
	}
	for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("code-server still running after CloseAll")
	}
	if h.s.ide.running(w.ID) || h.s.sessions.Count(w.ID)[SessionCodeServer] != 0 {
		t.Fatal("instance still registered")
	}
	// The next request starts a fresh instance.
	if code, _, _ := h.do(owner, "GET", base+"/", nil, ""); code != 200 {
		t.Fatalf("restart after handoff: %d", code)
	}
}

func TestIDEIdleStopAndCrashLimits(t *testing.T) {
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "demo")
	base := "/api/workspaces/" + w.ID + "/ide"
	h.s.ide.idle = 300 * time.Millisecond

	if code, _, _ := h.do(owner, "GET", base+"/", nil, ""); code != 200 {
		t.Fatal(code)
	}
	h.s.ide.reapOnce()
	if !h.s.ide.running(w.ID) {
		t.Fatal("stopped while recently used")
	}
	time.Sleep(400 * time.Millisecond)
	h.s.ide.reapOnce()
	for i := 0; i < 100 && h.s.ide.running(w.ID); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if h.s.ide.running(w.ID) {
		t.Fatal("idle instance not stopped")
	}
	start := time.Now()
	if code, _, _ := h.do(owner, "GET", base+"/", nil, ""); code != 200 || time.Since(start) > 5*time.Second {
		t.Fatalf("restart on demand: %d after %v", code, time.Since(start))
	}

	// A crash is restarted by the supervisor...
	h.do(owner, "GET", base+"/crash", nil, "")
	ok := false
	for i := 0; i < 100; i++ {
		if code, _, _ := h.do(owner, "GET", base+"/", nil, ""); code == 200 {
			ok = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		t.Fatal("not restarted after a crash")
	}
	// ...but not forever.
	var code int
	var body string
	for i := 0; i < 200; i++ {
		code, body, _ = h.do(owner, "GET", base+"/crash", nil, "")
		if code == 503 && strings.Contains(body, "failed 3 times") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 503 || !strings.Contains(body, "failed 3 times") {
		t.Fatalf("crash loop not stopped: %d %s", code, body)
	}
	if h.s.sessions.Count(w.ID)[SessionCodeServer] != 0 {
		t.Fatal("failed instance still registered")
	}
}

// A process that replaces code-server's socket is not served as the IDE.
func TestIDEPeerCheck(t *testing.T) {
	if !peerCheckSupported {
		t.Skip("no SO_PEERCRED")
	}
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "demo")
	base := "/api/workspaces/" + w.ID + "/ide"
	if code, _, _ := h.do(owner, "GET", base+"/", nil, ""); code != 200 {
		t.Fatal(code)
	}
	inst := h.s.ide.byWS[w.ID]
	inst.mu.Lock()
	real := inst.pid
	// Pretend the supervised process is someone else. (Never a pid the
	// shutdown path could signal as a group: restore it below.)
	inst.pid = real + 1000000
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.pid = real
		inst.mu.Unlock()
	}()
	inst.proxy.Transport.(*http.Transport).CloseIdleConnections()
	if code, body, _ := h.do(owner, "GET", base+"/", nil, ""); code != 502 || strings.Contains(body, "fake") {
		t.Fatalf("impostor socket served: %d %s", code, body)
	}
}
