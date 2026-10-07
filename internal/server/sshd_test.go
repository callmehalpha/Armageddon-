package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

type sshFixture struct {
	h      *harness
	owner  *testUser
	w      *store.Workspace
	signer ssh.Signer
	devID  string
	addr   string
}

func newSSHFixture(t *testing.T) *sshFixture {
	h := newHarness(t)
	owner := h.user("owner")
	w := h.workspace(owner, "demo")
	d, err := h.s.startSSH(h.s.ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.s.sshd = d
	f := &sshFixture{h: h, owner: owner, w: w, addr: d.ln.Addr().String()}
	f.devID, f.signer = f.device(owner)
	return f
}

// device pairs a new device key for u (directly in the store).
func (f *sshFixture) device(u *testUser) (string, ssh.Signer) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := ids.New()
	now := store.Now()
	if _, err := f.h.s.store.DB().Exec(`INSERT INTO devices (id, user_id, name, public_key, created_at, last_seen_at) VALUES (?, ?, 'laptop', ?, ?, ?)`,
		id, u.ID, base64.StdEncoding.EncodeToString(pub), now, now); err != nil {
		f.h.t.Fatal(err)
	}
	s, _ := ssh.NewSignerFromKey(priv)
	return id, s
}

func (f *sshFixture) dial(user string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
}

func (f *sshFixture) client() *ssh.Client {
	c, err := f.dial("ws-demo", f.signer)
	if err != nil {
		f.h.t.Fatal(err)
	}
	f.h.t.Cleanup(func() { c.Close() })
	return c
}

func run(t *testing.T, c *ssh.Client, cmd string) (string, string, error) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out, errb bytes.Buffer
	s.Stdout, s.Stderr = &out, &errb
	err = s.Run(cmd)
	return out.String(), errb.String(), err
}

func TestSSHAuthentication(t *testing.T) {
	f := newSSHFixture(t)
	stranger := f.h.user("stranger")
	f.h.workspace(stranger, "theirs")
	_, strangerKey := f.device(stranger)
	_, unknown, _ := ed25519.GenerateKey(rand.Reader)
	unknownSigner, _ := ssh.NewSignerFromKey(unknown)

	for _, user := range []string{"ws-demo", "demo", strings.ToLower(f.w.ID)} {
		c, err := f.dial(user, f.signer)
		if err != nil {
			t.Fatalf("user %q: %v", user, err)
		}
		c.Close()
	}
	for name, try := range map[string]func() error{
		"unknown key":            func() error { _, err := f.dial("ws-demo", unknownSigner); return err },
		"non-member's device":    func() error { _, err := f.dial("ws-demo", strangerKey); return err },
		"member's key, other ws": func() error { _, err := f.dial("ws-theirs", f.signer); return err },
		"no such workspace":      func() error { _, err := f.dial("ws-nope", f.signer); return err },
		"password auth (not offered)": func() error {
			_, err := ssh.Dial("tcp", f.addr, &ssh.ClientConfig{User: "ws-demo", Auth: []ssh.AuthMethod{ssh.Password("x")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
			return err
		},
	} {
		if try() == nil {
			t.Errorf("%s: authenticated", name)
		}
	}

	// Revocation drops live connections and refuses new ones.
	// ssh.Dial returns once the client handshake is done, before the server
	// registers the connection, so wait for the earlier connections to
	// leave and the new one to arrive: closeDevice must find it.
	devConns := func() int {
		f.h.s.sshd.mu.Lock()
		defer f.h.s.sshd.mu.Unlock()
		n := 0
		for sc := range f.h.s.sshd.conns {
			if sc.deviceID == f.devID {
				n++
			}
		}
		return n
	}
	waitConns := func(want int) {
		for deadline := time.Now().Add(5 * time.Second); devConns() != want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("device has %d live connections, want %d", devConns(), want)
			}
		}
	}
	waitConns(0)
	c := f.client()
	waitConns(1)
	if err := f.h.s.store.RevokeDevice(f.devID, f.owner.ID, store.Now()); err != nil {
		t.Fatal(err)
	}
	f.h.s.sshd.closeDevice(f.devID, "revoked")
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked device's connection still open")
	}
	if _, err := f.dial("ws-demo", f.signer); err == nil {
		t.Fatal("revoked device authenticated")
	}
}

func TestSSHExecPTYAndLease(t *testing.T) {
	f := newSSHFixture(t)
	rt := f.h.s.runtimeFor(f.w.ID)
	c := f.client()

	out, _, err := run(t, c, "id -u; pwd; echo $ARMAGEDDON_WORKSPACE; exit 7")
	lines := strings.Fields(out)
	if ee, ok := err.(*ssh.ExitError); !ok || ee.ExitStatus() != 7 {
		t.Fatalf("exit status: %v", err)
	}
	// pwd reports the resolved path; on macOS /tmp is a symlink to /private/tmp.
	tree, _ := filepath.EvalSymlinks(rt.p.Tree)
	if len(lines) != 3 || lines[0] != strconv.Itoa(int(rt.acct.UID)) || (lines[1] != rt.p.Tree && lines[1] != tree) || lines[2] != "demo" {
		t.Fatalf("exec output %q (want uid %d in %s)", out, rt.acct.UID, rt.p.Tree)
	}
	// stdin reaches the process
	s, _ := c.NewSession()
	s.Stdin = strings.NewReader("hello stdin")
	b, err := s.Output("cat")
	if err != nil || string(b) != "hello stdin" {
		t.Fatalf("cat: %q %v", b, err)
	}

	// PTY shell
	s, _ = c.NewSession()
	if err := s.RequestPty("xterm", 40, 100, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var buf syncBuffer
	s.Stdout = &buf
	in, _ := s.StdinPipe()
	if err := s.Start("tty; stty size; exit 0"); err != nil {
		t.Fatal(err)
	}
	in.Close()
	s.Wait()
	if (!strings.Contains(buf.String(), "/dev/pts/") && !strings.Contains(buf.String(), "/dev/ttys")) || !strings.Contains(buf.String(), "40 100") {
		t.Fatalf("pty output: %q", buf.String())
	}

	// No lease, no session.
	f.h.setLease(f.w.ID, "device")
	_, stderr, err := run(t, c, "echo should-not-run")
	if err == nil || !strings.Contains(stderr, "owned by a device") {
		t.Fatalf("exec without lease: %v %q", err, stderr)
	}
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "owned by a device") {
		t.Fatalf("forward without lease: %v", err)
	}
}

func TestSSHForwardingAndSFTP(t *testing.T) {
	f := newSSHFixture(t)
	rt := f.h.s.runtimeFor(f.w.ID)
	c := f.client()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(conn, "from-loopback")
			conn.Close()
		}
	}()
	conn, err := c.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(conn)
	conn.Close()
	if string(b) != "from-loopback" {
		t.Fatalf("forwarded read: %q", b)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if conn, err := c.Dial("tcp", "localhost:"+port); err != nil {
		t.Fatalf("localhost forward: %v", err)
	} else {
		conn.Close()
	}
	for _, dst := range []string{"10.1.2.3:80", "192.168.0.1:22", "example.com:443", "0.0.0.0:" + port} {
		if _, err := c.Dial("tcp", dst); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("forward to %s: %v", dst, err)
		}
	}

	// SFTP runs as the workspace user, rooted at the worktree.
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	wf, err := sc.Create("via-sftp.txt")
	if err != nil {
		t.Fatal(err)
	}
	wf.Write([]byte("sftp works"))
	wf.Close()
	p := filepath.Join(rt.p.Tree, "via-sftp.txt")
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "sftp works" {
		t.Fatalf("file via sftp: %q %v", got, err)
	}
	fi, _ := os.Stat(p)
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != rt.acct.UID {
		t.Fatalf("sftp file owned by uid %d, want %d", st.Uid, rt.acct.UID)
	}
	if rt.acct.Isolated() {
		// The server's own files are out of reach.
		if _, err := sc.Open(filepath.Join(f.h.s.cfg.DataDir, "armageddon.db")); err == nil {
			t.Fatal("sftp opened the server database")
		}
		if _, err := sc.Open(filepath.Join(f.h.s.cfg.DataDir, "keys", "data.key")); err == nil {
			t.Fatal("sftp opened the data key")
		}
	}
}

// M4.6: closing the workspace's sessions closes an SSH session within 2 s
// and shows the reason.
func TestSSHClosedOnHandoff(t *testing.T) {
	f := newSSHFixture(t)
	c := f.client()
	s, _ := c.NewSession()
	if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr syncBuffer
	s.Stdout, s.Stderr = &stdout, &stderr
	if err := s.Start("echo started; sleep 60"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && !strings.Contains(stdout.String(), "started"); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if f.h.s.sessions.Count(f.w.ID)[SessionSSH] != 1 {
		t.Fatal("ssh connection not registered")
	}
	start := time.Now()
	go f.h.s.sessions.CloseAll(f.w.ID, "the workspace was handed to laptop")
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ssh session still open 2 s after CloseAll")
	}
	if !strings.Contains(stdout.String()+stderr.String(), "handed to laptop") {
		t.Fatalf("no banner: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	t.Logf("closed in %v", time.Since(start))
	for i := 0; i < 50 && f.h.s.sessions.Count(f.w.ID)[SessionSSH] != 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if f.h.s.sessions.Count(f.w.ID)[SessionSSH] != 0 {
		t.Fatal("ssh session still registered")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
