package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// handleTerminal bridges a WebSocket to a login shell running as the
// workspace user in the server-seat worktree (§2.3, plan M4.2).
//
// Client → server: JSON text frames {"t":"i","d":"<input>"} and
// {"t":"r","c":<cols>,"r":<rows>}. Server → client: binary output frames.
func (s *Server) handleTerminal(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	if sessionOf(r) == nil {
		writeErr(rw, 403, "terminals are opened from a browser session")
		return
	}
	rt := s.runtimeFor(w.ID)
	if rt == nil {
		writeErr(rw, 503, "workspace not running")
		return
	}
	lease, err := s.store.LeaseOf(nil, w.ID)
	if err != nil || lease.HolderKind != "server" {
		writeErr(rw, 409, "the workspace is owned by a device; the server seat is read-only until it is handed back")
		return
	}
	// websocket.Accept enforces a same-origin check, which is the CSRF
	// protection for this endpoint.
	c, err := websocket.Accept(rw, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	// The helper opens the PTY (owned by ws-<id>), starts the login shell
	// as the workspace user and passes the master back (§2.5, M4.2).
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	env := append(rt.acct.BaseEnv(),
		"TERM=xterm-256color", "SHELL="+shell,
		"ARMAGEDDON_WORKSPACE="+w.Name,
		// P-13: interactive shells get the trash hooks through the
		// environment, not only through repo config.
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+rt.p.Hooks,
	)
	proc, err := s.helper.Spawn(r.Context(), w.ID, helper.SpawnSpec{Kind: helper.KindPTYShell,
		Argv: []string{shell, "-l"}, Env: env, Dir: rt.p.Tree, Cols: 80, Rows: 24})
	if err != nil {
		c.Close(websocket.StatusInternalError, "shell: "+err.Error())
		return
	}
	ptmx := proc.PTY()
	defer ptmx.Close()
	s.event(w.ID, "user", userOf(r).ID, "terminal.started", map[string]any{"pid": proc.Pid(), "handle": proc.Handle()})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	closeReason := "shell exited"
	var reasonMu sync.Mutex
	unregister := s.sessions.Register(w.ID, SessionTerminal, userOf(r).ID, func(reason string) {
		// Banner first, then hang up (contract §4.3).
		c.Write(ctx, websocket.MessageBinary, []byte("\r\n\x1b[33m[armageddon] "+reason+"\x1b[0m\r\n"))
		reasonMu.Lock()
		closeReason = reason
		reasonMu.Unlock()
		cancel()
	})
	defer unregister()
	go func() { // shell → browser
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		cancel()
	}()
	go func() { // browser → shell
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				break
			}
			var m struct {
				T    string `json:"t"`
				D    string `json:"d"`
				C, R uint16
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.T {
			case "i":
				ptmx.Write([]byte(m.D))
			case "r":
				if m.C > 0 && m.R > 0 {
					pty.Setsize(ptmx, &pty.Winsize{Cols: m.C, Rows: m.R})
				}
			}
		}
		cancel()
	}()
	<-ctx.Done()
	proc.Signal(syscall.SIGHUP)
	waited := make(chan struct{})
	go func() { proc.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		proc.Signal(syscall.SIGKILL)
		<-waited
	}
	reasonMu.Lock()
	defer reasonMu.Unlock()
	c.Close(websocket.StatusNormalClosure, closeReason)
}
