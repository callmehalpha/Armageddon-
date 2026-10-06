package server

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Git-provider credentials (contract §5.8, §7.5; plan M2.8 and M4.5).
//
// A user stores a PAT or has the server generate an SSH key per provider
// host. The secret is sealed with the server data key (keys/data.key,
// AES-256-GCM, bound to its row) and never returned by the API. Server-seat
// sessions reach it only through per-session unix sockets the server
// serves: a Git credential helper socket for HTTPS and an SSH agent socket.
// Nothing is written into the workspace.

const (
	credHTTPS = "https-token"
	credSSH   = "ssh-key"
)

var credHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

func credAAD(c *store.ProviderCredential) []byte {
	return []byte("armageddon-credential-v1\x00" + c.ID + "\x00" + c.UserID + "\x00" + c.Host + "\x00" + c.Kind)
}

func (s *Server) sealCredential(c *store.ProviderCredential, secret []byte) {
	kid, ct := s.keys.Seal(secret, credAAD(c))
	c.KeyID, c.Ciphertext = kid, base64.StdEncoding.EncodeToString(ct)
}

func (s *Server) openCredential(c *store.ProviderCredential) ([]byte, error) {
	ct, err := base64.StdEncoding.DecodeString(c.Ciphertext)
	if err != nil {
		return nil, err
	}
	return s.keys.Open(c.KeyID, ct, credAAD(c))
}

func credJSON(c *store.ProviderCredential) map[string]any {
	out := map[string]any{"id": c.ID, "host": c.Host, "kind": c.Kind, "username": c.Username, "created_at": c.CreatedAt}
	if c.PublicKey != "" {
		out["public_key"] = c.PublicKey
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.PublicKey)); err == nil {
			out["fingerprint"] = ssh.FingerprintSHA256(pk)
		}
	}
	return out
}

func (s *Server) handleListCredentials(rw http.ResponseWriter, r *http.Request) {
	cs, err := s.store.CredentialsOfUser(userOf(r).ID)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, c := range cs {
		out = append(out, credJSON(c))
	}
	writeJSON(rw, 200, out)
}

// handleCreateCredential stores a PAT ({host, kind: "https-token", username,
// token}) or generates an SSH key ({host, kind: "ssh-key"}) whose public half
// the response carries, for the user to add to the provider.
func (s *Server) handleCreateCredential(rw http.ResponseWriter, r *http.Request) {
	if sessionOf(r) == nil {
		writeErr(rw, 403, "manage credentials from a browser session")
		return
	}
	var req struct {
		Host     string `json:"host"`
		Kind     string `json:"kind"`
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	req.Host = strings.ToLower(strings.TrimSpace(req.Host))
	req.Host = strings.TrimPrefix(strings.TrimPrefix(req.Host, "https://"), "http://")
	req.Host = strings.TrimRight(req.Host, "/")
	if !credHostRe.MatchString(req.Host) || len(req.Host) > 253 {
		writeErr(rw, 400, "host: a provider host name such as github.com")
		return
	}
	u := userOf(r)
	c := &store.ProviderCredential{ID: ids.New(), UserID: u.ID, Host: req.Host, Kind: req.Kind, CreatedAt: store.Now()}
	var secret []byte
	switch req.Kind {
	case credHTTPS:
		tok := strings.TrimSpace(req.Token)
		if tok == "" || len(tok) > 4096 || strings.ContainsAny(tok, "\r\n\x00") {
			writeErr(rw, 400, "token: paste the personal access token")
			return
		}
		c.Username = strings.TrimSpace(req.Username)
		if c.Username == "" {
			c.Username = "x-access-token"
		}
		if strings.ContainsAny(c.Username, "\r\n\x00") || len(c.Username) > 256 {
			writeErr(rw, 400, "bad username")
			return
		}
		secret = []byte(tok)
	case credSSH:
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			writeErr(rw, 500, err.Error())
			return
		}
		comment := fmt.Sprintf("armageddon-%s@%s", u.Username, req.Host)
		blk, err := ssh.MarshalPrivateKey(priv, comment)
		if err != nil {
			writeErr(rw, 500, err.Error())
			return
		}
		secret = pem.EncodeToMemory(blk)
		sp, _ := ssh.NewPublicKey(pub)
		c.PublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment
	default:
		writeErr(rw, 400, `kind: "https-token" or "ssh-key"`)
		return
	}
	s.credMu.Lock()
	s.sealCredential(c, secret)
	err := s.store.InsertCredential(c)
	s.credMu.Unlock()
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(rw, 409, "you already have a credential of this kind for "+req.Host+"; remove it first")
			return
		}
		writeErr(rw, 500, err.Error())
		return
	}
	s.event("", "user", u.ID, "credential.added", map[string]string{"host": c.Host, "kind": c.Kind})
	writeJSON(rw, 201, credJSON(c))
}

func (s *Server) handleDeleteCredential(rw http.ResponseWriter, r *http.Request) {
	if sessionOf(r) == nil {
		writeErr(rw, 403, "manage credentials from a browser session")
		return
	}
	if err := s.store.DeleteCredential(r.PathValue("id"), userOf(r).ID); err != nil {
		writeErr(rw, 404, "no such credential")
		return
	}
	s.event("", "user", userOf(r).ID, "credential.removed", map[string]string{"id": r.PathValue("id")})
	writeJSON(rw, 200, map[string]bool{"ok": true})
}

// handleRotateDataKey re-seals every stored credential with a fresh data
// key and retires the old keys (admin only).
func (s *Server) handleRotateDataKey(rw http.ResponseWriter, r *http.Request) {
	if sessionOf(r) == nil {
		writeErr(rw, 403, "rotate the data key from a browser session")
		return
	}
	n, err := s.rotateDataKey()
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	s.event("", "user", userOf(r).ID, "data_key.rotated", map[string]any{"resealed": n, "active": s.keys.ActiveID()})
	writeJSON(rw, 200, map[string]any{"active_key": s.keys.ActiveID(), "resealed": n})
}

// rotateDataKey adds a new active data key, re-seals every credential with
// it in one transaction, then retires the old keys. If re-sealing fails the
// old keys are kept, so nothing becomes unreadable.
func (s *Server) rotateDataKey() (int, error) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if _, err := s.keys.Rotate(); err != nil {
		return 0, err
	}
	n, err := s.store.ResealCredentials(func(c *store.ProviderCredential) (string, string, error) {
		pt, err := s.openCredential(c)
		if err != nil {
			return "", "", fmt.Errorf("credential %s: %w", c.ID, err)
		}
		kid, ct := s.keys.Seal(pt, credAAD(c))
		return kid, base64.StdEncoding.EncodeToString(ct), nil
	})
	if err != nil {
		return 0, err
	}
	return n, s.keys.Retire()
}

// ---- per-session exposure to the server seat ----

// seatCredentials serves one session's credential sockets. The zero value
// (a user with no credentials) exposes nothing.
type seatCredentials struct {
	s                   *Server
	wsID, userID        string
	uid                 uint32 // the workspace user, the only peer served
	credPath, agentPath string
	lns                 []net.Listener
	once                sync.Once
}

// openSeatCredentials creates the per-session sockets for a user who has
// stored credentials. The server listens and workspace processes connect,
// and the server cannot give a socket to the workspace user, so the
// sockets are made safe without chown:
//
//   - they live in <run>/seat/, a server-owned directory of mode 0711
//     (nobody else can create, remove or list entries), under a random
//     name only the session's environment carries;
//   - the socket file is 0666, so the workspace user can connect;
//   - every accepted connection is checked with SO_PEERCRED and served
//     only if the peer is the workspace's uid. Anything else is logged,
//     recorded as an event and dropped before a byte is read.
func (s *Server) openSeatCredentials(rt *runtime, userID string) (*seatCredentials, error) {
	sc := &seatCredentials{s: s, wsID: rt.id, userID: userID, uid: rt.acct.UID}
	cs, err := s.store.CredentialsOfUser(userID)
	if err != nil || len(cs) == 0 {
		return sc, err
	}
	var https, sshKeys bool
	for _, c := range cs {
		https = https || c.Kind == credHTTPS
		sshKeys = sshKeys || c.Kind == credSSH
	}
	dir, err := s.seatSocketDir()
	if err != nil {
		return nil, err
	}
	listen := func(path string, serve func(net.Conn)) error {
		if len(path) > 100 {
			return fmt.Errorf("socket path too long (%d bytes): %s", len(path), path)
		}
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return err
		}
		ln.SetUnlinkOnClose(true)
		sc.lns = append(sc.lns, ln)
		if err := os.Chmod(path, 0o666); err != nil {
			return err
		}
		go func() {
			for {
				c, err := ln.AcceptUnix()
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						continue
					}
					return
				}
				if !sc.peerAllowed(c, filepath.Base(path)) {
					c.Close()
					continue
				}
				go serve(c)
			}
		}()
		return nil
	}
	tag := ids.Secret(16)
	if https {
		sc.credPath = filepath.Join(dir, "c-"+tag+".sock")
		if err := listen(sc.credPath, sc.serveCredential); err != nil {
			sc.Close()
			return nil, err
		}
	}
	if sshKeys {
		sc.agentPath = filepath.Join(dir, "a-"+tag+".sock")
		if err := listen(sc.agentPath, func(c net.Conn) {
			defer c.Close()
			agent.ServeAgent(&seatAgent{sc: sc}, c)
		}); err != nil {
			sc.Close()
			return nil, err
		}
	}
	return sc, nil
}

// seatSocketDir is the server-owned directory of the per-session credential
// sockets: <run>/seat, mode 0711.
func (s *Server) seatSocketDir() (string, error) {
	dir := filepath.Join(s.runDir, "seat")
	if err := os.Mkdir(dir, 0o711); err != nil && !os.IsExist(err) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || !ownedByMe(fi) {
		return "", fmt.Errorf("%s is not a directory owned by the server user: refusing to put credential sockets there", dir)
	}
	if fi.Mode().Perm() != 0o711 {
		if err := os.Chmod(dir, 0o711); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// peerAllowed is the mandatory SO_PEERCRED check of a credential socket:
// only the workspace's uid is served.
func (sc *seatCredentials) peerAllowed(c *net.UnixConn, name string) bool {
	uid, err := helper.PeerUID(c)
	if err == nil && uid == sc.uid {
		return true
	}
	log.Printf("workspace %s: credential socket %s refused a connection from uid %d (%v); only uid %d is served", sc.wsID, name, uid, err, sc.uid)
	sc.s.event(sc.wsID, "server", "", "credential.refused", map[string]any{"peer_uid": uid})
	return false
}

// GitConfig is the Git configuration a session gets: the helper list is
// reset first, so a credential.helper=store from the workspace's own config
// can never write the secret into the workspace.
func (sc *seatCredentials) GitConfig() [][2]string {
	if sc == nil || sc.credPath == "" {
		return nil
	}
	return [][2]string{{"credential.helper", ""}, {"credential.helper", "!" + shellQuote(sc.s.hookBin) + " seat-credential"}}
}

func (sc *seatCredentials) Env() []string {
	var env []string
	if sc == nil {
		return nil
	}
	if sc.credPath != "" {
		env = append(env, "ARMAGEDDON_CREDENTIAL_SOCKET="+sc.credPath)
	}
	if sc.agentPath != "" {
		env = append(env, "SSH_AUTH_SOCK="+sc.agentPath)
	}
	return env
}

// Close stops serving and removes the sockets.
func (sc *seatCredentials) Close() {
	if sc == nil {
		return
	}
	sc.once.Do(func() {
		for _, ln := range sc.lns {
			ln.Close() // unlinks the socket
		}
	})
}

type credRequest struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
}

type credResponse struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

func isLoopbackHost(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (sc *seatCredentials) serveCredential(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var req credRequest
	if err := json.NewDecoder(io.LimitReader(c, 4096)).Decode(&req); err != nil {
		return
	}
	reply := func(r credResponse) { json.NewEncoder(c).Encode(r) }
	host := strings.ToLower(req.Host)
	// PATs travel over TLS only; plain http is allowed for loopback test
	// remotes.
	if req.Protocol != "https" && !(req.Protocol == "http" && isLoopbackHost(host)) {
		reply(credResponse{Error: "credentials are only sent over https"})
		return
	}
	cred, err := sc.s.store.CredentialFor(sc.userID, host, credHTTPS)
	if err != nil {
		reply(credResponse{Error: "no stored credential for " + host})
		return
	}
	secret, err := sc.s.openCredential(cred)
	if err != nil {
		log.Printf("credential %s: %v", cred.ID, err)
		reply(credResponse{Error: "stored credential unreadable"})
		return
	}
	sc.s.event(sc.wsID, "user", sc.userID, "credential.used", map[string]string{"host": host, "kind": credHTTPS})
	reply(credResponse{Username: cred.Username, Password: string(secret)})
}

// seatAgent is a read-only SSH agent over the user's stored SSH keys. Keys
// are decrypted per signature and never leave the server.
type seatAgent struct{ sc *seatCredentials }

var errReadOnlyAgent = errors.New("the armageddon agent is read-only")

func (a *seatAgent) keys() ([]*store.ProviderCredential, error) {
	cs, err := a.sc.s.store.CredentialsOfUser(a.sc.userID)
	var out []*store.ProviderCredential
	for _, c := range cs {
		if c.Kind == credSSH {
			out = append(out, c)
		}
	}
	return out, err
}

func (a *seatAgent) List() ([]*agent.Key, error) {
	cs, err := a.keys()
	if err != nil {
		return nil, err
	}
	var out []*agent.Key
	for _, c := range cs {
		pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(c.PublicKey))
		if err != nil {
			continue
		}
		out = append(out, &agent.Key{Format: pk.Type(), Blob: pk.Marshal(), Comment: comment})
	}
	return out, nil
}

func (a *seatAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	cs, err := a.keys()
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.PublicKey))
		if err != nil || !bytes.Equal(pk.Marshal(), key.Marshal()) {
			continue
		}
		pemBytes, err := a.sc.s.openCredential(c)
		if err != nil {
			return nil, errors.New("stored key unreadable")
		}
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, err
		}
		a.sc.s.event(a.sc.wsID, "user", a.sc.userID, "credential.used", map[string]string{"host": c.Host, "kind": credSSH})
		return signer.Sign(rand.Reader, data)
	}
	return nil, errors.New("no such key")
}

func (a *seatAgent) Add(agent.AddedKey) error       { return errReadOnlyAgent }
func (a *seatAgent) Remove(ssh.PublicKey) error     { return errReadOnlyAgent }
func (a *seatAgent) RemoveAll() error               { return errReadOnlyAgent }
func (a *seatAgent) Lock([]byte) error              { return errReadOnlyAgent }
func (a *seatAgent) Unlock([]byte) error            { return errReadOnlyAgent }
func (a *seatAgent) Signers() ([]ssh.Signer, error) { return nil, errReadOnlyAgent }

// SeatCredentialHelper is `armageddon seat-credential <op>`: the Git
// credential helper configured in server-seat sessions. It runs as the
// workspace user and asks the session's credential socket for a PAT.
func SeatCredentialHelper(op string, in io.Reader, out io.Writer) error {
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	sock := os.Getenv("ARMAGEDDON_CREDENTIAL_SOCKET")
	if op != "get" || sock == "" {
		return nil // store/erase: the server is the store
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return fmt.Errorf("credential socket: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(credRequest{Protocol: attrs["protocol"], Host: attrs["host"]}); err != nil {
		return err
	}
	var resp credResponse
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "armageddon:", resp.Error)
		return nil
	}
	fmt.Fprintf(out, "username=%s\npassword=%s\n\n", resp.Username, resp.Password)
	return nil
}

// shellQuote quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ownedByMe reports whether fi belongs to the server's own uid.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
