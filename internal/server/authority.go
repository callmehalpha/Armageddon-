package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/helper"
)

// HookMessage is what a server-owned hook (running as ws-<id>) reports to
// the authority over the workspace's socket.
type HookMessage struct {
	Hook    string      `json:"hook"`
	Updates [][3]string `json:"updates"` // old, new, ref
	Trash   []string    `json:"trash,omitempty"`
}

func (s *Server) authoritySocket(id string) string { return filepath.Join(s.runDir, id+".sock") }

// listenAuthority opens /run/armageddon/<ws-id>.sock (P-14), the channel
// from the workspace's hooks to its authority. The socket file is
// world-connectable; every connection is checked with SO_PEERCRED and only
// the workspace's own uid is served. Hooks are a convenience, not the
// security boundary (§2.5): the fence and the HTTP front end are.
func (s *Server) listenAuthority(rt *runtime) error {
	path := s.authoritySocket(rt.id)
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		os.Remove(path)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	l.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return err
	}
	rt.authority = l
	go func() {
		for {
			conn, err := l.AcceptUnix()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue
				}
				return
			}
			go s.serveHookConn(rt, conn)
		}
	}()
	return nil
}

func (s *Server) serveHookConn(rt *runtime, conn *net.UnixConn) {
	defer conn.Close()
	uid, err := helper.PeerUID(conn)
	if err != nil || uid != rt.acct.UID {
		log.Printf("workspace %s: authority socket refused uid %d", rt.id, uid)
		return
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, 16<<20)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var m HookMessage
	if err := json.Unmarshal(line, &m); err != nil || (m.Hook != "post-receive" && m.Hook != "reference-transaction") || len(m.Updates) > 10000 {
		conn.Write([]byte("error\n"))
		return
	}
	s.event(rt.id, "workspace", rt.acct.Name, "refs.updated", m)
	conn.Write([]byte("ok\n"))
}

// notifyAuthority is the hook side: best effort, never blocks the Git
// operation for long.
func notifyAuthority(m HookMessage) {
	path := os.Getenv("ARMAGEDDON_AUTHORITY_SOCK")
	if path == "" || len(m.Updates) == 0 {
		return
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	b, _ := json.Marshal(m)
	conn.Write(append(b, '\n'))
	bufio.NewReader(conn).ReadBytes('\n')
}
