package server

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitProc polls GET /runtime until the named process satisfies ok.
func (h *harness) waitProc(u *testUser, wsID, name string, ok func(p map[string]any) bool) map[string]any {
	h.t.Helper()
	var last string
	for i := 0; i < 200; i++ {
		_, m, raw := h.api(u, "GET", "/api/workspaces/"+wsID+"/runtime", nil)
		last = raw
		procs, _ := m["processes"].([]any)
		for _, x := range procs {
			p := x.(map[string]any)
			if p["name"] == name && ok(p) {
				return m
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, logs, _ := h.api(u, "GET", "/api/workspaces/"+wsID+"/runtime/logs?name="+name, nil)
	h.t.Fatalf("process %s never got there: %s\nlogs: %v", name, last, logs["data"])
	return nil
}

func procOf(m map[string]any, name string) map[string]any {
	for _, x := range m["processes"].([]any) {
		if p := x.(map[string]any); p["name"] == name {
			return p
		}
	}
	return nil
}

// TestRuntimeDevServerLifecycle drives M8.3/M8.5/M8.6 through the API with
// the PHP provider (PHP's built-in server needs no downloads): plan,
// install, start, health, port listing, the authenticated proxy, a handoff
// that stops the dev server, and the restart of `work remote --restart`.
func TestRuntimeDevServerLifecycle(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("port listing reads /proc (Linux servers only)")
	}
	if _, err := exec.LookPath("php"); err != nil {
		t.Skip("php not installed")
	}
	if _, err := os.Stat("/usr/bin/php"); err != nil {
		if _, err2 := os.Stat("/usr/local/bin/php"); err2 != nil {
			t.Skip("php is not on the workspace PATH")
		}
	}
	h := newHarness(t)
	owner, other := h.user("owner"), h.user("other")
	w := h.workspace(owner, "phpapp")
	rt := h.s.runtimeFor(w.ID)
	os.WriteFile(filepath.Join(rt.p.Tree, "index.php"), []byte(`<?php echo "hello from ", $_SERVER["REQUEST_URI"], " cookie=", $_SERVER["HTTP_COOKIE"] ?? "";`), 0o644)
	base := "/api/workspaces/" + w.ID

	code, m, raw := h.api(owner, "GET", base+"/runtime", nil)
	plan, _ := m["plan"].(map[string]any)
	if code != 200 || plan == nil || plan["provider"] != "php" || plan["port"].(float64) != 8000 {
		t.Fatalf("plan: %d %s", code, raw)
	}
	if code, _, _ := h.api(other, "GET", base+"/runtime", nil); code != 404 {
		t.Fatalf("non-member saw the runtime: %d", code)
	}

	if code, _, raw := h.api(owner, "POST", base+"/runtime/install", nil); code != 202 {
		t.Fatalf("install: %d %s", code, raw)
	}
	h.waitProc(owner, w.ID, "install", func(p map[string]any) bool { return p["state"] == "exited" && p["exit_code"] == 0.0 })
	_, logs, _ := h.api(owner, "GET", base+"/runtime/logs?name=install", nil)
	if !strings.Contains(fmt.Sprint(logs["data"]), "runtime ready") {
		t.Fatalf("install log: %v", logs["data"])
	}

	port := freePort(t)
	if code, _, raw := h.api(owner, "POST", base+"/runtime/start", map[string]int{"port": port}); code != 202 {
		t.Fatalf("start: %d %s", code, raw)
	}
	if code, _, _ := h.api(owner, "POST", base+"/runtime/start", map[string]int{"port": port}); code != 409 {
		t.Fatalf("second start: %d, want 409", code)
	}
	m = h.waitProc(owner, w.ID, "dev", func(p map[string]any) bool { return p["health"] == "healthy" })
	found := false
	for _, x := range m["ports"].([]any) {
		if p := x.(map[string]any); p["port"].(float64) == float64(port) && p["source"] == "process" {
			found = true
		}
	}
	if !found {
		t.Fatalf("port %d not listed: %v", port, m["ports"])
	}

	// The proxy: members only, path prefix stripped, Armageddon's cookie
	// never reaches the app.
	path := fmt.Sprintf("%s/ports/%d/some/page?x=1", base, port)
	code, body, _ := h.do(owner, "GET", path, nil, "")
	if code != 200 || !strings.HasPrefix(body, "hello from /some/page?x=1") || strings.Contains(body, owner.cookie) {
		t.Fatalf("proxy: %d %q", code, body)
	}
	if code, _, _ := h.do(other, "GET", path, nil, ""); code != 404 {
		t.Fatalf("non-member through the proxy: %d", code)
	}
	if code, _, _ := h.do(nil, "GET", path, nil, ""); code != 401 {
		t.Fatalf("anonymous through the proxy: %d", code)
	}
	if code, _, _ := h.do(owner, "POST", path, map[string]string{"Origin": "https://evil.example"}, "x"); code != 403 {
		t.Fatalf("cross-origin POST through the proxy: %d", code)
	}
	if code, _, _ := h.do(owner, "GET", fmt.Sprintf("%s/ports/%d/", base, freePort(t)), nil, ""); code != 404 {
		t.Fatalf("a port nothing listens on: %d", code)
	}

	// Handoff (Q1): the dev server stops; work remote --restart brings it
	// back.
	(&serverSeat{s: h.s, rt: rt}).Quiesce("test handoff")
	m = h.waitProc(owner, w.ID, "dev", func(p map[string]any) bool { return p["state"] == "stopped" })
	if p := procOf(m, "dev"); p["stopped_by"] != "handoff" {
		t.Fatalf("dev after handoff: %v", p)
	}
	names, err := h.s.procs.restartAfterHandoff(rt, w)
	if err != nil || len(names) != 1 {
		t.Fatalf("restart: %v %v", names, err)
	}
	h.waitProc(owner, w.ID, "dev", func(p map[string]any) bool { return p["health"] == "healthy" })

	if code, _, raw := h.api(owner, "POST", base+"/runtime/stop", nil); code != 200 {
		t.Fatalf("stop: %d %s", code, raw)
	}
	m = h.waitProc(owner, w.ID, "dev", func(p map[string]any) bool { return p["state"] == "stopped" })
	if names, _ := h.s.procs.restartAfterHandoff(rt, w); len(names) != 0 {
		t.Fatal("a dev server the user stopped was restarted")
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"stopped_by":"user"`) {
		t.Fatalf("stop: %s", b)
	}
}

// TestRuntimeNeedsTheLease: dev processes run only on the writing seat.
func TestRuntimeNeedsTheLease(t *testing.T) {
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "leased")
	h.setLease(w.ID, "device")
	for _, p := range []string{"/runtime/install", "/runtime/start"} {
		if code, _, raw := h.api(owner, "POST", "/api/workspaces/"+w.ID+p, nil); code != 409 {
			t.Errorf("%s while a device writes: %d %s", p, code, raw)
		}
	}
}

func TestRuntimeNothingDetected(t *testing.T) {
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "empty")
	_, m, raw := h.api(owner, "GET", "/api/workspaces/"+w.ID+"/runtime", nil)
	if !strings.Contains(fmt.Sprint(m["plan_error"]), "no runtime detected") {
		t.Fatalf("empty workspace: %s", raw)
	}
	if code, _, _ := h.api(owner, "POST", "/api/workspaces/"+w.ID+"/runtime/start", nil); code != 400 {
		t.Fatalf("start with nothing to start: %d", code)
	}
}

func TestProcNetTCP(t *testing.T) {
	in := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 12345 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 12346 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0BB8 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000  1001        0 12347 1 0000000000000000 100 0 0 10 0
`
	got := parseProcNetTCP(strings.NewReader(in))
	if len(got) != 2 || got[0].Port != 3000 || !got[0].IP.Equal(net.IPv4(127, 0, 0, 1)) || got[0].UID != 1001 ||
		got[1].Port != 8000 || !got[1].IP.IsUnspecified() {
		t.Fatalf("%+v", got)
	}
	in6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1 0 100 0 0 10 0
`
	got = parseProcNetTCP(strings.NewReader(in6))
	if len(got) != 1 || !got[0].IP.Equal(net.IPv6loopback) || reachableIP(got[0].IP) != "::1" {
		t.Fatalf("v6: %+v", got)
	}
	if reachableIP(net.ParseIP("10.0.0.5")) != "" {
		t.Fatal("a non-loopback address is reachable")
	}
}

func TestReachableIPUnspecified(t *testing.T) {
	if got := reachableIP(net.IPv4zero); got != "127.0.0.1" {
		t.Errorf("0.0.0.0 → %q, want 127.0.0.1", got)
	}
	if got := reachableIP(net.IPv6unspecified); got != "::1" {
		t.Errorf(":: → %q, want ::1", got)
	}
}
