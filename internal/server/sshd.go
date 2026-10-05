package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Embedded SSH endpoint (contract §2.3, plan M4.4). Off by default
// (ssh.enabled) until the P8 spike confirms VS Code Remote-SSH and
// JetBrains Gateway work against it.
//
// A paired device's Ed25519 key authenticates as user ws-<slug> (or the
// workspace ID) when the device's user is a member of that workspace. Every
// connection is a server-seat session: refused unless the server holds the
// lease, registered with the session registry, closed with a banner on
// handoff. Shells, exec, SFTP and the processes behind them run as the
// workspace user; local forwarding reaches loopback only.

type sshServer struct {
	s   *Server
	ln  net.Listener
	cfg *ssh.ServerConfig

	hostKey ssh.PublicKey

	mu    sync.Mutex
	conns map[*sshConn]struct{}
}

type sshConn struct {
	conn     *ssh.ServerConn
	ws       *store.Workspace
	userID   string
	deviceID string

	mu       sync.Mutex
	sessions map[ssh.Channel]struct{}
	closed   bool
}

func (s *Server) sshHostKey() (ssh.Signer, error) {
	path := filepath.Join(s.cfg.DataDir, "keys", "ssh_host_ed25519_key")
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "armageddon host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

func (s *Server) startSSH(ctx context.Context, addr string) (*sshServer, error) {
	signer, err := s.sshHostKey()
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	d := &sshServer{s: s, conns: map[*sshConn]struct{}{}, hostKey: signer.PublicKey()}
	d.cfg = &ssh.ServerConfig{PublicKeyCallback: d.authenticate, ServerVersion: "SSH-2.0-Armageddon",
		MaxAuthTries: 6}
	d.cfg.AddHostKey(signer)
	if d.ln, err = net.Listen("tcp", addr); err != nil {
		return nil, err
	}
	log.Printf("ssh endpoint listening on %s (host key %s)", d.ln.Addr(), ssh.FingerprintSHA256(signer.PublicKey()))
	go func() {
		for {
			c, err := d.ln.Accept()
			if err != nil {
				return
			}
			go d.handle(c)
		}
	}()
	go func() { <-ctx.Done(); d.Close() }()
	return d, nil
}

// Close stops listening and drops every connection.
func (d *sshServer) Close() {
	d.ln.Close()
	d.mu.Lock()
	var all []*sshConn
	for c := range d.conns {
		all = append(all, c)
	}
	d.mu.Unlock()
	for _, c := range all {
		c.close("the server is shutting down")
	}
}

// closeDevice drops a revoked device's live connections (§7.3: revocation
// is immediate for live SSH connections).
func (d *sshServer) closeDevice(deviceID, reason string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	var hit []*sshConn
	for c := range d.conns {
		if c.deviceID == deviceID {
			hit = append(hit, c)
		}
	}
	d.mu.Unlock()
	for _, c := range hit {
		c.close(reason)
	}
}

// resolveSSHUser maps an SSH user name to one workspace the user belongs
// to: ws-<slug>, <slug>, or the workspace ID. Ambiguous slugs fail.
func (s *Server) resolveSSHUser(name, userID string) (*store.Workspace, error) {
	if w, _, err := s.store.WorkspaceForMember(strings.ToUpper(name), userID); err == nil {
		return w, nil
	}
	seen := map[string]*store.Workspace{}
	for _, slug := range []string{strings.TrimPrefix(name, "ws-"), name} {
		ws, err := s.store.WorkspacesForMemberBySlug(userID, slug)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			seen[w.ID] = w
		}
	}
	if len(seen) != 1 {
		if len(seen) > 1 {
			return nil, fmt.Errorf("%q matches %d workspaces; use the workspace ID", name, len(seen))
		}
		return nil, errors.New("no such workspace")
	}
	for _, w := range seen {
		return w, nil
	}
	return nil, nil
}

func (d *sshServer) authenticate(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	deny := errors.New("unknown key or workspace")
	ck, ok := key.(ssh.CryptoPublicKey)
	if !ok {
		return nil, deny
	}
	edk, ok := ck.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, deny
	}
	dev, err := d.s.store.DeviceByPublicKey(base64.StdEncoding.EncodeToString(edk))
	if err != nil || dev.Revoked {
		return nil, deny
	}
	u, err := d.s.store.UserByID(dev.UserID)
	if err != nil || u.Disabled {
		return nil, deny
	}
	w, err := d.s.resolveSSHUser(meta.User(), u.ID)
	if err != nil {
		return nil, deny
	}
	return &ssh.Permissions{Extensions: map[string]string{"device": dev.ID, "user": u.ID, "workspace": w.ID}}, nil
}

func (d *sshServer) handle(nc net.Conn) {
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	conn, chans, reqs, err := ssh.NewServerConn(nc, d.cfg)
	if err != nil {
		nc.Close()
		return
	}
	nc.SetDeadline(time.Time{})
	ext := conn.Permissions.Extensions
	w, err := d.s.store.WorkspaceByID(ext["workspace"])
	if err != nil {
		conn.Close()
		return
	}
	c := &sshConn{conn: conn, ws: w, userID: ext["user"], deviceID: ext["device"], sessions: map[ssh.Channel]struct{}{}}
	d.mu.Lock()
	d.conns[c] = struct{}{}
	d.mu.Unlock()
	unregister := d.s.sessions.Register(w.ID, SessionSSH, c.userID, c.close)
	d.s.store.TouchDevice(c.deviceID, store.Now())
	d.s.event(w.ID, "device", c.deviceID, "ssh.connected", map[string]string{"remote": nc.RemoteAddr().String()})
	defer func() {
		unregister()
		d.mu.Lock()
		delete(d.conns, c)
		d.mu.Unlock()
		conn.Close()
	}()
	// Global requests (tcpip-forward, keepalives) are refused: no remote
	// forwarding.
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			go d.session(c, nch)
		case "direct-tcpip":
			go d.directTCPIP(c, nch)
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
}

// close shows reason on every open session, then drops the connection.
func (c *sshConn) close(reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	var chs []ssh.Channel
	for ch := range c.sessions {
		chs = append(chs, ch)
	}
	c.mu.Unlock()
	for _, ch := range chs {
		done := make(chan struct{})
		go func() {
			io.WriteString(ch.Stderr(), "\r\n[armageddon] "+reason+"\r\n")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
	c.conn.Close()
}

func exitStatus(ch ssh.Channel, code int) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
}

// refuse ends a session channel with a message on stderr.
func refuse(ch ssh.Channel, msg string) {
	io.WriteString(ch.Stderr(), "armageddon: "+msg+"\r\n")
	exitStatus(ch, 1)
	ch.Close()
}

// envAllowed lists client environment variables a session accepts.
func envAllowed(name string) bool {
	return name == "LANG" || name == "TERM" || name == "COLORTERM" || strings.HasPrefix(name, "LC_")
}
