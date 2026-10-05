package backup

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/server"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// TestMain lets this test binary act as the hook binary: the server copies
// os.Executable() to <data>/bin and Git hooks run `<bin> hook <name>`.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "hook" {
		os.Exit(server.HookMain(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

// skipRoot: as root the server creates real OS users (ws-<id>). The same
// flow runs as root in test/e2e/ops.sh.
func skipRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("creates OS users when run as root; covered by test/e2e/ops.sh")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newServer(t *testing.T, data string) *server.Server {
	t.Helper()
	cfg := config.Default(data)
	cfg.CaptureIntervalMS = int(time.Hour / time.Millisecond) // checkpoints only when the test asks
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func waitReady(t *testing.T, s *server.Server, id string) *store.Workspace {
	t.Helper()
	for i := 0; i < 200; i++ {
		w, err := s.Store().WorkspaceByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if w.State == "ready" && w.CheckpointSeq > 0 {
			return w
		}
		if w.State == "failed" {
			t.Fatalf("workspace failed: %s", w.StateReason)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("workspace not ready")
	return nil
}

func refs(t *testing.T, gitDir string, patterns ...string) string {
	return git(t, "/", append([]string{"--git-dir=" + gitDir, "for-each-ref", "--format=%(objectname) %(refname)"}, patterns...)...)
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	skipRoot(t)
	base := t.TempDir()
	os.Chmod(base, 0o755)
	data := filepath.Join(base, "data")
	s := newServer(t, data)
	owner := &store.User{ID: "01USER0000000000000000000A", Username: "abdul", PasswordHash: "x", Role: "admin", CreatedAt: store.Now()}
	if err := s.Store().CreateUser(owner); err != nil {
		t.Fatal(err)
	}
	w, err := s.CreateWorkspace(owner, "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, s, w.ID)
	tree := filepath.Join(server.WorkspaceDir(data, w.ID), "tree")

	// Committed work on a new branch, a deleted branch (trash ref),
	// staged and unstaged changes, an untracked .env.
	os.WriteFile(filepath.Join(tree, "main.go"), []byte("package main\n"), 0o644)
	git(t, tree, "checkout", "-q", "-b", "feature")
	git(t, tree, "add", "main.go")
	git(t, tree, "commit", "-qm", "add main")
	git(t, tree, "branch", "doomed")
	if _, err := s.CheckpointNow(w.ID); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tree, "staged.txt"), []byte("staged\n"), 0o644)
	git(t, tree, "add", "staged.txt")
	os.WriteFile(filepath.Join(tree, "staged.txt"), []byte("staged\nand then edited\n"), 0o644)
	os.WriteFile(filepath.Join(tree, ".env"), []byte("API_KEY=dev\n"), 0o600)
	os.WriteFile(filepath.Join(tree, "wip.txt"), []byte("not committed\n"), 0o644)
	if _, err := s.CheckpointNow(w.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Store().WorkspaceByID(w.ID)
	if before.CheckpointSeq < 3 {
		t.Fatalf("expected at least 3 checkpoints, got %d", before.CheckpointSeq)
	}
	// Keys, to check the encrypted round trip.
	os.MkdirAll(filepath.Join(data, "keys"), 0o700)
	os.WriteFile(filepath.Join(data, "keys", "server.key"), []byte("secret key material"), 0o600)

	var log bytes.Buffer
	dir, err := Run(Options{DataDir: data, To: filepath.Join(base, "backups"), Passphrase: []byte("correct horse"), ServerVersion: "test", Log: &log})
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, log.String())
	}
	srcRoot := server.WorkspaceDir(data, w.ID)
	wantRepoRefs := refs(t, filepath.Join(srcRoot, "repo.git"))
	wantCpRefs := refs(t, filepath.Join(srcRoot, "checkpoints.git"), "refs/checkpoints/")
	if !strings.Contains(wantRepoRefs, "refs/heads/doomed") {
		t.Fatal("fixture: branch missing")
	}

	// A checkpoint after the backup must not appear in the restore.
	os.WriteFile(filepath.Join(tree, "after-backup.txt"), []byte("later\n"), 0o644)
	if seq, err := s.CheckpointNow(w.ID); err != nil || seq == 0 {
		t.Fatalf("post-backup checkpoint: %d %v", seq, err)
	}
	s.Close()

	// Wrong passphrase: refused before anything is written.
	fresh := filepath.Join(base, "fresh")
	if _, err := Restore(RestoreOptions{Backup: dir, DataDir: fresh, Passphrase: []byte("wrong")}); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	os.RemoveAll(fresh)

	rep, err := Restore(RestoreOptions{Backup: dir, DataDir: fresh, Passphrase: []byte("correct horse"), Log: &log})
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, log.String())
	}
	if len(rep.Restored) != 1 || len(rep.Inexact) != 0 {
		t.Fatalf("restore report %+v\n%s", rep, log.String())
	}
	// Restoring again onto the same directory is refused.
	if _, err := Restore(RestoreOptions{Backup: dir, DataDir: fresh}); err == nil {
		t.Fatal("restore onto a used data directory accepted")
	}

	dstRoot := server.WorkspaceDir(fresh, w.ID)
	if got := refs(t, filepath.Join(dstRoot, "repo.git")); got != wantRepoRefs {
		t.Fatalf("repo refs differ\nwant:\n%s\ngot:\n%s", wantRepoRefs, got)
	}
	if got := refs(t, filepath.Join(dstRoot, "checkpoints.git"), "refs/checkpoints/"); got != wantCpRefs {
		t.Fatalf("checkpoint refs differ\nwant:\n%s\ngot:\n%s", wantCpRefs, got)
	}
	b, _ := os.ReadFile(filepath.Join(fresh, "keys", "server.key"))
	if string(b) != "secret key material" {
		t.Fatal("keys not restored")
	}

	s2 := newServerExisting(t, fresh)
	defer s2.Close()
	after, err := s2.Store().WorkspaceByID(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" || after.CurrentCheckpoint != before.CurrentCheckpoint || after.CheckpointSeq != before.CheckpointSeq {
		t.Fatalf("workspace after restore: %+v, want current %s #%d", after, before.CurrentCheckpoint, before.CheckpointSeq)
	}
	lease, err := s2.Store().LeaseOf(nil, w.ID)
	if err != nil || lease.HolderKind != "server" || lease.Epoch != 2 {
		t.Fatalf("lease after restore: %+v %v", lease, err)
	}
	rtree := filepath.Join(dstRoot, "tree")
	for f, want := range map[string]string{"wip.txt": "not committed\n", ".env": "API_KEY=dev\n", "staged.txt": "staged\nand then edited\n", "main.go": "package main\n"} {
		if b, err := os.ReadFile(filepath.Join(rtree, f)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", f, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rtree, "after-backup.txt")); err == nil {
		t.Error("a file from after the backup was restored")
	}
	if got := git(t, rtree, "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Errorf("HEAD %s", got)
	}
	if got := git(t, rtree, "show", ":staged.txt"); got != "staged" {
		t.Errorf("index content of staged.txt: %q", got)
	}
	// rep.Inexact is empty: a fresh capture of the rebuilt seat equals the
	// current checkpoint, so the restored server records no new one.
}

func newServerExisting(t *testing.T, data string) *server.Server {
	t.Helper()
	cfg, err := config.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEncryptDecrypt(t *testing.T) {
	sealed, err := Encrypt([]byte("keys"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := Decrypt(sealed, []byte("pw")); err != nil || string(p) != "keys" {
		t.Fatal(p, err)
	}
	if _, err := Decrypt(sealed, []byte("nope")); err != ErrPassphrase {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := Decrypt(sealed, []byte("pw")); err != ErrPassphrase {
		t.Fatal("tampered archive accepted")
	}
}

func TestDamagedBackupRefused(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "armageddon.db"), []byte("db"), 0o600)
	os.WriteFile(filepath.Join(dir, "backup.json"), []byte(`{"format":1,"sha256":{"armageddon.db":"00"}}`), 0o600)
	if _, err := ReadManifest(dir); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("want damaged, got %v", err)
	}
}
