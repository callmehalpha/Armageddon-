package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"

	"github.com/coder/websocket"
	"github.com/creack/pty"

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

	ptmx, tty, err := pty.Open()
	if err != nil {
		c.Close(websocket.StatusInternalError, "pty: "+err.Error())
		return
	}
	defer ptmx.Close()
	os.Chown(tty.Name(), int(rt.acct.UID), int(rt.acct.GID))

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = rt.p.Tree
	rt.acct.Prepare(cmd,
		"TERM=xterm-256color",
		"SHELL="+shell,
		"ARMAGEDDON_WORKSPACE="+w.Name,
		// P-13: interactive shells get the trash hooks through the
		// environment, not only through repo config.
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+rt.p.Hooks,
		fmt.Sprintf("PS1=\\[\\e[1;36m\\]%s\\[\\e[0m\\]:\\w\\$ ", w.Slug),
	)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if err := cmd.Start(); err != nil {
		tty.Close()
		c.Close(websocket.StatusInternalError, "shell: "+err.Error())
		return
	}
	tty.Close()
	s.event(w.ID, "user", userOf(r).ID, "terminal.started", map[string]int{"pid": cmd.Process.Pid})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
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
	cmd.Process.Signal(syscall.SIGHUP)
	cmd.Wait()
	c.Close(websocket.StatusNormalClosure, "shell exited")
}
