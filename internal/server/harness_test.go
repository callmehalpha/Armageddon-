package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/identity"
	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
	"github.com/callmehalpha/Armageddon-/internal/testutil/fakecodeserver"
)

// The test binary doubles as the helpers the server spawns: the server
// copies it to <data>/bin/armageddon (hookBin), so "hookBin seat-credential"
// and "hookBin sftp-server" land here, and a wrapper script makes it the
// fake code-server.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "fake-code-server":
			os.Exit(fakecodeserver.Main(os.Args[2:]))
		case "seat-credential":
			op := ""
			if len(os.Args) > 2 {
				op = os.Args[2]
			}
			if err := SeatCredentialHelper(op, os.Stdin, os.Stdout); err != nil {
				os.Stderr.WriteString(err.Error() + "\n")
				os.Exit(1)
			}
			os.Exit(0)
		case "sftp-server":
			if err := SFTPServerMain(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

type harness struct {
	t   *testing.T
	s   *Server
	ts  *httptest.Server
	dir string
}

type testUser struct {
	*store.User
	cookie string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// Short path: unix socket paths are limited to ~108 bytes. Traversable,
	// since workspace users (when the test runs as root) must reach it.
	dir, err := os.MkdirTemp("/tmp", "a5-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	cfg := config.Default(filepath.Join(dir, "d"))
	cfg.CaptureIntervalMS = 3600 * 1000
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	script := filepath.Join(dir, "code-server")
	os.WriteFile(script, []byte("#!/bin/sh\nexec '"+s.hookBin+"' fake-code-server \"$@\"\n"), 0o755)
	os.Chmod(script, 0o755)
	cfg.CodeServer.Path = script
	h := &harness{t: t, s: s, dir: dir}
	h.ts = httptest.NewServer(s.routes())
	cfg.PublicURL = h.ts.URL
	t.Cleanup(func() {
		h.ts.Close()
		s.ide.stopAll("test over")
		if s.sshd != nil {
			s.sshd.Close()
		}
		cancel()
		s.mu.Lock()
		for _, rt := range s.rts {
			rt.stop()
		}
		s.mu.Unlock()
		s.store.Close()
		os.RemoveAll(dir)
	})
	return h
}

func (h *harness) user(name string) *testUser {
	h.t.Helper()
	hash, _ := identity.HashPassword("throwaway-" + ids.Secret(6))
	u := &store.User{ID: ids.New(), Username: name, PasswordHash: hash, Role: "user", CreatedAt: store.Now()}
	if err := h.s.store.CreateUser(u); err != nil {
		h.t.Fatal(err)
	}
	tok := ids.Secret(32)
	now := store.Now()
	if err := h.s.store.CreateSession(&store.Session{ID: ids.New(), UserID: u.ID, TokenHash: ids.Hash(tok), CSRF: ids.Secret(24),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now + time.Hour.Milliseconds(), ReauthAt: now}); err != nil {
		h.t.Fatal(err)
	}
	return &testUser{User: u, cookie: tok}
}

func (h *harness) workspace(owner *testUser, name string) *store.Workspace {
	h.t.Helper()
	w, err := h.s.CreateWorkspace(owner.User, name, "")
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		cur, err := h.s.store.WorkspaceByID(w.ID)
		if err == nil && cur.State == StateReady && h.s.runtimeFor(w.ID) != nil && cur.CheckpointSeq > 0 {
			return cur
		}
		if err == nil && cur.State == StateFailed {
			h.t.Fatalf("workspace failed: %s", cur.StateReason)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("workspace not ready")
	return nil
}

// do sends a request as u (nil: anonymous) and returns status and body.
func (h *harness) do(u *testUser, method, path string, hdr map[string]string, body string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
	if u != nil {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: u.cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 30 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (h *harness) setLease(wsID, holder string) {
	h.t.Helper()
	dev := any(nil)
	if holder == "device" {
		dev = "dev-test"
	}
	if _, err := h.s.store.DB().Exec(`UPDATE leases SET holder_kind = ?, holder_device_id = ? WHERE workspace_id = ?`, holder, dev, wsID); err != nil {
		h.t.Fatal(err)
	}
}
