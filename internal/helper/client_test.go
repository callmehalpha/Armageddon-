package helper

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFramesCarryDescriptors(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "sp")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	a, b := conn(fds[0]), conn(fds[1])
	defer a.Close()
	defer b.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	req := &Request{Op: OpRepairDataOwnership}
	if err := sendFrame(a, req, w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	body, files, err := recvFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRequest(body); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d descriptors", len(files))
	}
	files[0].Write([]byte("through SCM_RIGHTS"))
	files[0].Close()
	got, _ := io.ReadAll(r)
	if string(got) != "through SCM_RIGHTS" {
		t.Fatalf("pipe carried %q", got)
	}
	// More than three descriptors in one frame are refused.
	var many []*os.File
	for i := 0; i < 4; i++ {
		f, _ := os.Open(os.DevNull)
		defer f.Close()
		many = append(many, f)
	}
	if err := sendFrame(a, req, many...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recvFrame(b); err == nil {
		t.Fatal("accepted four descriptors")
	}
}

func devWorkspace(t *testing.T) (*Dev, *Account, string) {
	data := t.TempDir()
	d := NewDev(data)
	root := filepath.Join(data, "workspaces", ws)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.PrepareWorkspaceDirs(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	a, err := d.CreateWorkspaceUser(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return d, a, root
}

func TestDevAccountSeam(t *testing.T) {
	_, a, root := devWorkspace(t)
	if a.Isolated() {
		t.Fatal("dev account claims isolation")
	}
	tree := filepath.Join(root, "tree")
	cmd := a.Command(tree, "sh", "-c", "pwd; echo $HOME")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := tree + "\n" + filepath.Join(root, "home") + "\n"; string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
	f := filepath.Join(root, "home", "sub", "f")
	if err := a.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.MkdirOwned(filepath.Join(root, "seat", "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Chown(f); err != nil {
		t.Fatal(err)
	}
}

func TestDevSpawn(t *testing.T) {
	d, _, root := devWorkspace(t)
	ctx := context.Background()
	tree := filepath.Join(root, "tree")
	r, w, _ := os.Pipe()
	p, err := d.Spawn(ctx, ws, SpawnSpec{Kind: KindRuntimeCommand, Argv: []string{"sh", "-c", "echo $FOO$GIT_X; pwd; exit 3"},
		Env: []string{"FOO=dropped", "GIT_X=kept"}, Dir: tree, Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	out, _ := io.ReadAll(r)
	st, err := p.Wait()
	if err != nil || st.Code != 3 {
		t.Fatalf("exit %+v %v", st, err)
	}
	if string(out) != "kept\n"+tree+"\n" {
		t.Fatalf("output %q", out)
	}

	if _, err := d.Spawn(ctx, ws, SpawnSpec{Kind: KindRuntimeCommand, Argv: []string{"true"}, Dir: "/tmp"}); err == nil {
		t.Fatal("cwd outside the workspace accepted")
	}
	if _, err := d.Spawn(ctx, ws, SpawnSpec{Kind: KindCodeServer, Argv: []string{"true"}, Dir: tree}); err != ErrReservedKind {
		t.Fatalf("reserved kind: %v", err)
	}

	// pty-shell: the PTY comes back, signals reach the handle.
	p, err = d.Spawn(ctx, ws, SpawnSpec{Kind: KindPTYShell, Argv: []string{"/bin/sh"}, Dir: tree, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer p.PTY().Close()
	p.PTY().Write([]byte("echo pty-ok\n"))
	buf := make([]byte, 4096)
	var seen strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(seen.String(), "pty-ok\r\n") && time.Now().Before(deadline) {
		p.PTY().SetReadDeadline(time.Now().Add(time.Second))
		n, _ := p.PTY().Read(buf)
		seen.Write(buf[:n])
	}
	if !strings.Contains(seen.String(), "pty-ok") {
		t.Fatalf("pty output %q", seen.String())
	}
	if err := d.SignalWorkspace(ctx, ws, p.Handle(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.Wait(); st.Signal != int(syscall.SIGKILL) {
		t.Fatalf("exit %+v", st)
	}
	if err := d.SignalWorkspace(ctx, "06GGQXZ2K8M4T6V1R3P5N7B9ZZ", p.Handle(), syscall.SIGTERM); err == nil {
		t.Fatal("signalled a handle of another workspace")
	}
}

func TestDevPrepareRefusesSymlink(t *testing.T) {
	data := t.TempDir()
	d := NewDev(data)
	root := filepath.Join(data, "workspaces", ws)
	os.MkdirAll(root, 0o755)
	os.Symlink("/etc", filepath.Join(root, "home"))
	if err := d.PrepareWorkspaceDirs(context.Background(), ws); err == nil {
		t.Fatal("followed a symlinked home")
	}
}
