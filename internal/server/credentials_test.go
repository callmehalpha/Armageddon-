package server

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/callmehalpha/Armageddon-/internal/ids"
)

func (h *harness) api(u *testUser, method, path string, body any) (int, map[string]any, string) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	x, _ := h.s.store.SessionByTokenHash(ids.Hash(u.cookie))
	code, resp, _ := h.do(u, method, path, map[string]string{"Content-Type": "application/json", "X-CSRF-Token": x.CSRF}, string(b))
	var m map[string]any
	json.Unmarshal([]byte(resp), &m)
	return code, m, resp
}

func TestCredentialsAPINeverReturnsSecret(t *testing.T) {
	h := newHarness(t)
	u := h.user("owner")
	const pat = "ghp_SuperSecretToken1234567890"
	code, m, raw := h.api(u, "POST", "/api/credentials", map[string]string{"host": "https://GitHub.com/", "kind": "https-token", "token": pat})
	if code != 201 || m["host"] != "github.com" || strings.Contains(raw, pat) {
		t.Fatalf("create: %d %s", code, raw)
	}
	code, m, raw = h.api(u, "POST", "/api/credentials", map[string]string{"host": "github.com", "kind": "ssh-key"})
	if code != 201 || !strings.HasPrefix(m["public_key"].(string), "ssh-ed25519 ") || strings.Contains(raw, "PRIVATE") {
		t.Fatalf("ssh key: %d %s", code, raw)
	}
	if code, _, _ := h.api(u, "POST", "/api/credentials", map[string]string{"host": "github.com", "kind": "https-token", "token": "x"}); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	for _, bad := range []map[string]string{{"host": "bad host", "kind": "https-token", "token": "x"}, {"host": "a.com", "kind": "other"},
		{"host": "a.com", "kind": "https-token", "token": ""}, {"host": "a.com", "kind": "https-token", "token": "a\nb"}} {
		if code, _, _ := h.api(u, "POST", "/api/credentials", bad); code != 400 {
			t.Errorf("%v: %d", bad, code)
		}
	}
	_, _, list := h.api(u, "GET", "/api/credentials", nil)
	if strings.Contains(list, pat) || strings.Contains(list, "PRIVATE") || strings.Count(list, `"id"`) != 2 {
		t.Fatalf("list: %s", list)
	}
	// At rest: neither the token nor the key appears in the database file.
	h.s.store.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, f := range []string{"armageddon.db", "armageddon.db-wal"} {
		b, _ := os.ReadFile(filepath.Join(h.s.cfg.DataDir, f))
		if bytes.Contains(b, []byte(pat)) || bytes.Contains(b, []byte("OPENSSH PRIVATE KEY")) {
			t.Fatalf("secret in %s", f)
		}
	}
	// Another user cannot see or delete them.
	other := h.user("other")
	if _, _, l := h.api(other, "GET", "/api/credentials", nil); strings.Contains(l, "github.com") {
		t.Fatal("other user sees credentials")
	}
	id := m["id"].(string)
	if code, _, _ := h.api(other, "DELETE", "/api/credentials/"+id, nil); code != 404 {
		t.Fatalf("other user delete: %d", code)
	}
	if code, _, _ := h.api(u, "DELETE", "/api/credentials/"+id, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
}

func TestDataKeyRotation(t *testing.T) {
	h := newHarness(t)
	u := h.user("owner")
	h.api(u, "POST", "/api/credentials", map[string]string{"host": "github.com", "kind": "https-token", "token": "tok-one"})
	h.api(u, "POST", "/api/credentials", map[string]string{"host": "gitlab.com", "kind": "https-token", "token": "tok-two"})
	old := h.s.keys.ActiveID()
	n, err := h.s.rotateDataKey()
	if err != nil || n != 2 {
		t.Fatalf("rotate: %d %v", n, err)
	}
	if ids := h.s.keys.IDs(); len(ids) != 1 || ids[0] == old {
		t.Fatalf("keys after rotation: %v (old %s)", ids, old)
	}
	cs, _ := h.s.store.CredentialsOfUser(u.ID)
	for _, c := range cs {
		if c.KeyID != h.s.keys.ActiveID() {
			t.Fatalf("credential %s still sealed with %s", c.Host, c.KeyID)
		}
		pt, err := h.s.openCredential(c)
		if err != nil || !strings.HasPrefix(string(pt), "tok-") {
			t.Fatalf("after rotation %s: %q %v", c.Host, pt, err)
		}
	}
	// The admin API does the same; a plain user may not.
	if code, _, _ := h.api(u, "POST", "/api/admin/data-key/rotate", nil); code != 403 {
		t.Fatalf("non-admin rotate: %d", code)
	}
	h.s.store.DB().Exec(`UPDATE users SET role = 'admin' WHERE id = ?`, u.ID)
	if code, m, _ := h.api(u, "POST", "/api/admin/data-key/rotate", nil); code != 200 || m["resealed"].(float64) != 2 {
		t.Fatalf("admin rotate: %d %v", code, m)
	}
}

// M4.5: from a terminal-like server-seat process, git push over HTTPS to a
// remote that requires the PAT works; the PAT is never written into the
// workspace.
func TestSeatGitPushWithPAT(t *testing.T) {
	h := newHarness(t)
	u := h.user("owner")
	w := h.workspace(u, "demo")
	rt := h.s.runtimeFor(w.ID)
	const pat = "ghp_PushToken_" + "abcdef0123456789"

	// A test remote: git http-backend behind basic auth requiring the PAT.
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	remotes := filepath.Join(h.dir, "remotes")
	os.MkdirAll(remotes, 0o755)
	if out, err := exec.Command("git", "init", "-q", "--bare", filepath.Join(remotes, "r.git")).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	exec.Command("git", "--git-dir", filepath.Join(remotes, "r.git"), "config", "http.receivepack", "true").Run()
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + remotes, "GIT_HTTP_EXPORT_ALL=1"}}
	var authed, refused int
	remote := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, pw, ok := r.BasicAuth(); !ok || pw != pat {
			refused++
			rw.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(rw, "auth required", 401)
			return
		}
		authed++
		backend.ServeHTTP(rw, r)
	}))
	defer remote.Close()
	remoteURL := remote.URL + "/r.git"

	push := func() (string, error) {
		// What the browser terminal and SSH sessions do: per-session
		// credential sockets and the seat environment, run as the
		// workspace user.
		creds, err := h.s.openSeatCredentials(rt, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer creds.Close()
		cmd := exec.Command("git", "push", "-q", remoteURL, "HEAD:refs/heads/main")
		cmd.Dir = rt.p.Tree
		// SpawnInWorkspace(kind=pty-shell), as in the terminal
		rt.acct.Prepare(cmd, seatEnv(rt, w, creds.GitConfig(), append(creds.Env(), "GIT_TERMINAL_PROMPT=0")...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := push(); err == nil {
		t.Fatalf("push without a stored PAT succeeded: %s", out)
	}
	// A credential.helper=store in the workspace's own config must not
	// capture the PAT.
	cfg := rt.acct.Command(rt.p.Tree, "git", "config", "--global", "credential.helper", "store")
	if out, err := cfg.CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	h.api(u, "POST", "/api/credentials", map[string]string{"host": strings.TrimPrefix(remote.URL, "http://"), "kind": "https-token", "token": pat})
	if out, err := push(); err != nil {
		t.Fatalf("push with the stored PAT: %v\n%s", err, out)
	}
	if authed == 0 {
		t.Fatal("remote saw no authenticated request")
	}
	if out, _ := exec.Command("git", "--git-dir", filepath.Join(remotes, "r.git"), "rev-parse", "refs/heads/main").Output(); len(bytes.TrimSpace(out)) != 40 {
		t.Fatal("nothing arrived at the remote")
	}
	// grep -r the workspace: the PAT is nowhere, and no socket is left.
	filepath.WalkDir(filepath.Join(h.s.cfg.DataDir, "workspaces", w.ID), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSocket != 0 && strings.Contains(p, "/run/") {
			t.Errorf("socket left behind: %s", p)
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(pat)) {
			t.Errorf("PAT written into the workspace: %s", p)
		}
		return nil
	})
	if out, err := exec.Command("grep", "-r", "-l", pat, filepath.Join(h.s.cfg.DataDir, "workspaces")).CombinedOutput(); err == nil {
		t.Fatalf("grep -r found the PAT: %s", out)
	}
}

func TestSeatSSHAgent(t *testing.T) {
	h := newHarness(t)
	u := h.user("owner")
	w := h.workspace(u, "demo")
	rt := h.s.runtimeFor(w.ID)
	_, m, _ := h.api(u, "POST", "/api/credentials", map[string]string{"host": "github.com", "kind": "ssh-key"})
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(m["public_key"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := h.s.openSeatCredentials(rt, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sock string
	for _, e := range creds.Env() {
		if v, ok := strings.CutPrefix(e, "SSH_AUTH_SOCK="); ok {
			sock = v
		}
	}
	// Safe without chown: a random name in a server-owned 0711 directory,
	// a connectable 0666 socket, and SO_PEERCRED on every connection.
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o666 || filepath.Dir(sock) != filepath.Join(h.s.runDir, "seat") {
		t.Fatalf("agent socket %q: %v %v", sock, fi, err)
	}
	if di, err := os.Stat(filepath.Dir(sock)); err != nil || di.Mode().Perm() != 0o711 || !ownedByMe(di) {
		t.Fatalf("socket directory: %v %v", di, err)
	}
	if strings.HasPrefix(sock, rt.p.Root+"/") {
		t.Fatalf("credential socket inside the workspace: %s", sock)
	}
	// A peer that is not the workspace user is dropped unserved.
	creds.uid = rt.acct.UID + 1
	if conn, err := net.Dial("unix", sock); err == nil {
		if keys, err := agent.NewClient(conn).List(); err == nil {
			t.Fatalf("agent served a foreign uid: %v", keys)
		}
		conn.Close()
	}
	creds.uid = rt.acct.UID
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.NewClient(conn)
	keys, err := ag.List()
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0].Marshal(), pub.Marshal()) {
		t.Fatalf("agent keys: %v %v", keys, err)
	}
	data := make([]byte, 32)
	rand.Read(data)
	sig, err := ag.Sign(pub, data)
	if err != nil || pub.Verify(data, sig) != nil {
		t.Fatalf("agent signature: %v", err)
	}
	if err := ag.RemoveAll(); err == nil {
		t.Fatal("agent accepted RemoveAll")
	}
	conn.Close()
	creds.Close()
	if _, err := os.Stat(sock); err == nil {
		t.Fatal("agent socket left after the session")
	}
}
