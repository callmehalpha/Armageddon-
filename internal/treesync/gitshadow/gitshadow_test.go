package gitshadow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func shadow(t *testing.T, base, name, wt string) *Shadow {
	t.Helper()
	gd := filepath.Join(base, name+".shadow")
	if err := Init(gd, nil); err != nil {
		t.Fatal(err)
	}
	return &Shadow{GitDir: gd, WorkTree: wt, IndexFile: filepath.Join(base, name+".index"), Preserve: []string{".env*"}}
}

// TestRoundTripAcrossCommit covers the path P2 did not: HEAD moving between
// checkpoints (a commit on the writer), staged changes, CRLF, deletes, .env.
func TestRoundTripAcrossCommit(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	os.MkdirAll(a, 0o755)
	git(t, a, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(a, ".gitignore"), []byte(".env*\nnode_modules/\n"), 0o644)
	os.WriteFile(filepath.Join(a, ".gitattributes"), []byte("*.txt text eol=lf\n"), 0o644)
	os.WriteFile(filepath.Join(a, "crlf.txt"), []byte("a\r\nb\r\n"), 0o644)
	os.WriteFile(filepath.Join(a, "old.go"), []byte("package old\n"), 0o644)
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "one")
	git(t, base, "clone", "-q", a, b)

	A, B := shadow(t, base, "A", a), shadow(t, base, "B", b)
	ref := "refs/checkpoints/current"
	c0, err := A.Checkpoint(ref, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TransferPack(A, B, ref, c0, ""); err != nil {
		t.Fatal(err)
	}
	if err := B.Seed(c0); err != nil { // clone wrote LF; seed restores CRLF bytes
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "crlf.txt")); string(got) != "a\r\nb\r\n" {
		t.Fatalf("seed did not restore exact bytes: %q", got)
	}

	// Writer: commit, then leave uncommitted, staged and preserved state.
	os.WriteFile(filepath.Join(a, "new.go"), []byte("package new\n"), 0o644)
	os.Remove(filepath.Join(a, "old.go"))
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "two")
	os.WriteFile(filepath.Join(a, "staged.go"), []byte("v1\n"), 0o644)
	git(t, a, "add", "staged.go")
	os.WriteFile(filepath.Join(a, "staged.go"), []byte("v2 unstaged\n"), 0o644)
	os.WriteFile(filepath.Join(a, ".env"), []byte("SECRET=1\n"), 0o600)
	os.MkdirAll(filepath.Join(a, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(a, "node_modules", "x", "i.js"), []byte("ignored"), 0o644)
	c1, err := A.Checkpoint(ref, c0, 1)
	if err != nil {
		t.Fatal(err)
	}
	n, err := TransferPack(A, B, ref, c1, c0)
	if err != nil {
		t.Fatal(err)
	}
	if n > 20000 {
		t.Errorf("incremental pack unexpectedly large: %d bytes", n)
	}
	// Follower: history first (as the agent does), then apply.
	git(t, b, "fetch", "-q", "--update-head-ok", a, "+refs/heads/*:refs/heads/*")
	if err := B.Apply(c0, c1, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"new.go", "staged.go", ".env", "crlf.txt"} {
		x, _ := os.ReadFile(filepath.Join(a, f))
		y, err := os.ReadFile(filepath.Join(b, f))
		if err != nil || string(x) != string(y) {
			t.Errorf("%s differs: %q vs %q (%v)", f, x, y, err)
		}
	}
	if _, err := os.Stat(filepath.Join(b, "old.go")); !os.IsNotExist(err) {
		t.Error("old.go should be deleted on the replica")
	}
	if _, err := os.Stat(filepath.Join(b, "node_modules")); !os.IsNotExist(err) {
		t.Error("ignored node_modules must not replicate")
	}
	if ia, ib := git(t, a, "ls-files", "-s"), git(t, b, "ls-files", "-s"); ia != ib {
		t.Errorf("index differs:\n%s\n---\n%s", ia, ib)
	}
	// Compare status with stat caches forced fresh on both sides: the writer
	// may report a CRLF file clean only because its cached stat data matches.
	for _, d := range []string{a, b} {
		exec.Command("git", "-C", d, "update-index", "-q", "--really-refresh").Run()
	}
	// crlf.txt is excluded: with "text eol=lf", Git reports a CRLF working
	// file as modified in any repository other than the one that ran
	// "git add" on it, even with identical bytes and index (reproduced with
	// plain git, no Armageddon involved). Bytes and index are compared above.
	if sa, sb := git(t, a, "status", "--porcelain", "--", ".", ":!crlf.txt"), git(t, b, "status", "--porcelain", "--", ".", ":!crlf.txt"); sa != sb {
		t.Errorf("status differs:\n%s\n---\n%s", sa, sb)
	}
}

func TestDivergedReplicaRefused(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	os.MkdirAll(a, 0o755)
	git(t, a, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(a, "f"), []byte("1\n"), 0o644)
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "one")
	b := filepath.Join(base, "b")
	git(t, base, "clone", "-q", a, b)
	A, B := shadow(t, base, "A", a), shadow(t, base, "B", b)
	ref := "refs/checkpoints/current"
	c0, _ := A.Checkpoint(ref, "", 0)
	TransferPack(A, B, ref, c0, "")
	if err := B.Seed(c0); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "f"), []byte("2\n"), 0o644)
	c1, _ := A.Checkpoint(ref, c0, 1)
	TransferPack(A, B, ref, c1, c0)
	os.WriteFile(filepath.Join(b, "f"), []byte("local edit\n"), 0o644)
	if err := B.Apply(c0, c1, ApplyOptions{}); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("expected ErrDiverged, got %v", err)
	}
}
