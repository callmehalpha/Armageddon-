package doctor

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// currentUser runs workspace Git commands as the test's own user, so the
// fixtures never create OS accounts.
func currentUser(wsID, osUser, dir string, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	return cmd, nil
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a healthy data directory with one ready workspace.
type fixture struct {
	env      Env
	data, id string
	cps      string
	current  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	data := t.TempDir()
	os.Chmod(data, 0o755)
	st, err := store.Open(filepath.Join(data, "armageddon.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	u := &store.User{ID: "U1", Username: "abdul", PasswordHash: "x", Role: "admin", CreatedAt: now}
	if err := st.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	id := "01WS000000000000000000000A"
	root := filepath.Join(data, "workspaces", id)
	cps, repo := filepath.Join(root, "checkpoints.git"), filepath.Join(root, "repo.git")
	os.MkdirAll(root, 0o755)
	run(t, root, "init", "-q", "--bare", repo)
	empty := run(t, root, "--git-dir="+repo, "commit-tree", run(t, root, "--git-dir="+repo, "mktree", "--missing"), "-m", "init")
	run(t, root, "--git-dir="+repo, "update-ref", "refs/heads/main", empty)
	run(t, root, "init", "-q", "--bare", cps)
	os.Chmod(cps, 0o700)
	cp := run(t, root, "--git-dir="+cps, "commit-tree", run(t, root, "--git-dir="+cps, "mktree"), "-m", "checkpoint 1")
	run(t, root, "--git-dir="+cps, "update-ref", "refs/checkpoints/1", cp)
	run(t, root, "--git-dir="+cps, "update-ref", "refs/checkpoints/current", cp)
	w := &store.Workspace{ID: id, OwnerID: "U1", Name: "hello", Slug: "hello", State: "ready", SourceKind: "empty", OSUser: "ws-test0000", CreatedAt: now, UpdatedAt: now}
	if err := st.Tx(context.Background(), func(tx *sql.Tx) error { return st.CreateWorkspace(tx, w) }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE workspaces SET current_checkpoint_id = ?, checkpoint_seq = 1 WHERE id = ?`, cp, id); err != nil {
		t.Fatal(err)
	}
	st.Close()
	os.Chmod(filepath.Join(data, "armageddon.db"), 0o600)
	cg := t.TempDir()
	os.WriteFile(filepath.Join(cg, "cgroup.controllers"), []byte("cpuset cpu io memory pids\n"), 0o644)
	f := &fixture{data: data, id: id, cps: cps, current: cp}
	f.env = Env{DataDir: data, CgroupRoot: cg, WorkspaceGit: currentUser,
		Statfs:  func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil },
		Openat2: func(string) error { return nil }}
	f.env.defaults()
	return f
}

func byCheck(rs []Result, prefix string) Result {
	for _, r := range rs {
		if strings.HasPrefix(r.Check, prefix) {
			return r
		}
	}
	return Result{}
}

func TestHealthyFixturePasses(t *testing.T) {
	f := newFixture(t)
	rs := Run(f.env)
	for _, r := range rs {
		if r.Status == Fail || r.Status == Warn {
			t.Errorf("healthy fixture flagged: %+v", r)
		}
	}
	if byCheck(rs, "helper").Status != Skip {
		t.Error("helper check should be skipped without a helper")
	}
}

func flagged(t *testing.T, r Result, want Status, remedyHas string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %s, want %s (%s)", r.Check, r.Status, want, r.Detail)
	}
	if !strings.Contains(r.Remedy, remedyHas) {
		t.Fatalf("%s: remedy %q should mention %q", r.Check, r.Remedy, remedyHas)
	}
}

func TestDiskFull(t *testing.T) {
	f := newFixture(t)
	f.env.Statfs = func(string) (uint64, uint64, error) { return 3 << 30, 100 << 30, nil }
	flagged(t, CheckDisk(&f.env), Fail, "disk_full")
	f.env.Statfs = func(string) (uint64, uint64, error) { return 8 << 30, 100 << 30, nil }
	flagged(t, CheckDisk(&f.env), Warn, "Free space")
}

func TestPermissions(t *testing.T) {
	f := newFixture(t)
	os.Chmod(filepath.Join(f.data, "armageddon.db"), 0o644)
	r := CheckPermissions(&f.env)
	flagged(t, r, Fail, "chmod go-rwx "+filepath.Join(f.data, "armageddon.db"))
	os.Chmod(filepath.Join(f.data, "armageddon.db"), 0o600)
	os.Chmod(f.cps, 0o755)
	flagged(t, CheckPermissions(&f.env), Fail, "chmod go-rwx "+f.cps)
	os.Chmod(f.cps, 0o700)
	os.Chmod(f.data, 0o700)
	flagged(t, CheckPermissions(&f.env), Fail, "chmod 755")
}

func TestHelper(t *testing.T) {
	f := newFixture(t)
	f.env.HelperSupported = true
	f.env.HelperSocket = filepath.Join(t.TempDir(), "helper.sock")
	flagged(t, CheckHelper(&f.env), Fail, "systemctl start armageddon-helper")
	os.WriteFile(f.env.HelperSocket, nil, 0o600)
	flagged(t, CheckHelper(&f.env), Fail, "restart the helper")
	os.Remove(f.env.HelperSocket)
	ln, err := net.Listen("unix", f.env.HelperSocket)
	if err != nil {
		t.Fatal(err)
	}
	if r := CheckHelper(&f.env); r.Status != OK {
		t.Fatalf("listening helper: %+v", r)
	}
	ln.Close() // a stale socket file with nobody listening
	if r := CheckHelper(&f.env); r.Status != Fail {
		t.Fatalf("dead helper: %+v", r)
	}
}

func fakeGit(t *testing.T, version string) string {
	p := filepath.Join(t.TempDir(), "git")
	os.WriteFile(p, []byte("#!/bin/sh\necho 'git version "+version+"'\n"), 0o755)
	return p
}

func TestGitVersion(t *testing.T) {
	f := newFixture(t)
	f.env.Git = fakeGit(t, "2.30.2")
	flagged(t, CheckGit(&f.env), Fail, "2.39 or newer")
	f.env.Git = fakeGit(t, "2.39.5")
	if r := CheckGit(&f.env); r.Status != OK {
		t.Fatal(r)
	}
	f.env.Git = fakeGit(t, "3.0")
	if r := CheckGit(&f.env); r.Status != OK {
		t.Fatal(r)
	}
	f.env.Git = filepath.Join(t.TempDir(), "nope")
	flagged(t, CheckGit(&f.env), Fail, "Install Git")
}

func TestCgroupAndOpenat2(t *testing.T) {
	f := newFixture(t)
	f.env.CgroupRoot = t.TempDir() // cgroup v1 or none: no cgroup.controllers
	flagged(t, CheckCgroup(&f.env), Warn, "unified")
	os.WriteFile(filepath.Join(f.env.CgroupRoot, "cgroup.controllers"), []byte("cpu io\n"), 0o644)
	flagged(t, CheckCgroup(&f.env), Warn, "Delegate")
	f.env.Openat2 = func(string) error { return unix.ENOSYS }
	flagged(t, CheckOpenat2(&f.env), Warn, "5.6")
	// The real probe works on this kernel (5.6+) or reports ENOSYS.
	if err := probeOpenat2(f.data); err != nil && !errors.Is(err, unix.ENOSYS) {
		t.Fatalf("openat2 probe: %v", err)
	}
}

func TestFsckFindsCorruption(t *testing.T) {
	f := newFixture(t)
	wss, err := workspaces(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if r := CheckFsck(&f.env, wss); r.Status != OK {
		t.Fatalf("clean fixture: %+v", r)
	}
	// Delete the checkpoint commit's object: the ref now dangles.
	obj := filepath.Join(f.cps, "objects", f.current[:2], f.current[2:])
	if err := os.Remove(obj); err != nil {
		t.Fatal(err)
	}
	flagged(t, CheckFsck(&f.env, wss), Fail, "F10")
}

func TestReconcile(t *testing.T) {
	f := newFixture(t)
	root := filepath.Dir(f.cps)
	other := run(t, root, "--git-dir="+f.cps, "commit-tree", run(t, root, "--git-dir="+f.cps, "mktree"), "-m", "stray")
	run(t, root, "--git-dir="+f.cps, "update-ref", "refs/checkpoints/current", other)
	wss, _ := workspaces(f.data)
	rs := CheckReconcile(&f.env, wss)
	flagged(t, rs[0], Fail, "doctor --repair")
	f.env.Repair = true
	if rs := CheckReconcile(&f.env, wss); rs[0].Status != Repaired {
		t.Fatalf("repair: %+v", rs[0])
	}
	if got := run(t, root, "--git-dir="+f.cps, "rev-parse", "refs/checkpoints/current"); got != f.current {
		t.Fatalf("ref not repaired: %s", got)
	}
	f.env.Repair = false
	if rs := CheckReconcile(&f.env, wss); rs[0].Status != OK {
		t.Fatalf("after repair: %+v", rs[0])
	}
	// The DB points at a checkpoint that does not exist: cannot be repaired.
	os.Remove(filepath.Join(f.cps, "objects", f.current[:2], f.current[2:]))
	flagged(t, CheckReconcile(&f.env, wss)[0], Fail, "backup")
}

func TestMissingEmptyTreeRepaired(t *testing.T) {
	f := newFixture(t)
	repo := filepath.Join(filepath.Dir(f.cps), "repo.git")
	// v0.1.0-mvp wrote the initial commit against Git's implicit empty tree.
	if err := os.Remove(filepath.Join(repo, "objects", emptyTree[:2], emptyTree[2:])); err != nil {
		t.Fatal(err)
	}
	wss, _ := workspaces(f.data)
	flagged(t, CheckFsck(&f.env, wss), Fail, "doctor --repair")
	f.env.Repair = true
	if r := CheckFsck(&f.env, wss); r.Status != Repaired {
		t.Fatalf("repair: %+v", r)
	}
	f.env.Repair = false
	if r := CheckFsck(&f.env, wss); r.Status != OK {
		t.Fatalf("after repair: %+v", r)
	}
}

func TestMissingDatabase(t *testing.T) {
	f := newFixture(t)
	os.Remove(filepath.Join(f.data, "armageddon.db"))
	rs := Run(f.env)
	if !Failed(rs) {
		t.Fatal("missing database not flagged")
	}
}
