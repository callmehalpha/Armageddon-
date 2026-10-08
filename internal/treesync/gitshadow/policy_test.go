package gitshadow

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseMaxFileSize(t *testing.T) {
	for _, c := range []struct {
		yaml string
		want int64
		ok   bool
	}{
		{"sync:\n  max_file_size: 200MB\n", 200 << 20, true},
		{"sync:\n  include_env: true\n  max_file_size: \"1 GB\"  # big\n", 1 << 30, true},
		{"sync:\n  max_file_size: 4096\n", 4096, true},
		{"sync:\n  max_file_size: 64KiB\n", 64 << 10, true},
		{"sync:\n  max_file_size: none\n", -1, true},
		{"sync:\n  max_file_size: 0\n", -1, true},
		{"other:\n  max_file_size: 1MB\n", 0, false},
		{"max_file_size: 1MB\n", 0, false},
		{"sync:\n  max_file_size: lots\n", 0, false},
		{"", 0, false},
	} {
		got, ok := ParseMaxFileSize(c.yaml)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseMaxFileSize(%q) = %d, %v; want %d, %v", c.yaml, got, ok, c.want, c.ok)
		}
	}
}

// TestOversizeNotCaptured: files over the limit are left out without being
// read (F17), listed in meta.excluded.oversize, and a tracked file that
// grows past the limit keeps its last captured version.
func TestOversizeNotCaptured(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	os.MkdirAll(a, 0o755)
	git(t, a, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(a, "small.txt"), []byte("small\n"), 0o644)
	os.WriteFile(filepath.Join(a, "grows.bin"), []byte("v1\n"), 0o644)
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "one")
	A := shadow(t, base, "A", a)
	A.MaxFileSize = 1024
	c0, err := A.Checkpoint("refs/c", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "big.bin"), make([]byte, 4096), 0o644)
	os.WriteFile(filepath.Join(a, "grows.bin"), make([]byte, 2048), 0o644)
	os.WriteFile(filepath.Join(a, "new.txt"), []byte("new\n"), 0o644)
	st, err := A.CaptureState()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"big.bin", "grows.bin"}; !reflect.DeepEqual(st.Oversize, want) {
		t.Fatalf("oversize = %v, want %v", st.Oversize, want)
	}
	c1, err := A.CommitState(st, "refs/c", c0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := A.Git(nil, "cat-file", "-e", c1+":worktree/big.bin"); err == nil {
		t.Fatal("big.bin was captured")
	}
	if got, _ := A.Git(nil, "cat-file", "blob", c1+":worktree/grows.bin"); string(got) != "v1\n" {
		t.Fatalf("grows.bin should keep its last captured version, got %q", got)
	}
	if _, err := A.Git(nil, "cat-file", "-e", c1+":worktree/new.txt"); err != nil {
		t.Fatal("new.txt (under the limit) was not captured")
	}
	m, err := A.ReadMeta(c1)
	if err != nil || m.Excluded == nil || !reflect.DeepEqual(m.Excluded.Oversize, []string{"big.bin", "grows.bin"}) {
		t.Fatalf("meta.excluded = %+v (%v)", m.Excluded, err)
	}
	// Recapture: same state, nothing re-read, no new checkpoint needed.
	st2, err := A.CaptureState()
	if err != nil || !st2.Same(st) {
		t.Fatalf("recapture differs: %+v vs %+v (%v)", st2, st, err)
	}
	// Removing the big file changes the state (it is no longer listed).
	os.Remove(filepath.Join(a, "big.bin"))
	if st3, _ := A.CaptureState(); st3.Same(st) {
		t.Fatal("removing an oversize file should change the state")
	}
}

// TestPolicyFileRaisesTheLimit: max_file_size in .armageddon/sync.yaml
// applies from the capture after the one that picked it up.
func TestPolicyFileRaisesTheLimit(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	os.MkdirAll(filepath.Join(a, ".armageddon"), 0o755)
	git(t, a, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(a, ".armageddon", "sync.yaml"), []byte("sync:\n  max_file_size: 1KB\n"), 0o644)
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "one")
	A := shadow(t, base, "A", a)
	if _, err := A.CaptureState(); err != nil { // picks the policy up
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "mid.bin"), make([]byte, 2048), 0o644)
	st, _ := A.CaptureState()
	if !reflect.DeepEqual(st.Oversize, []string{"mid.bin"}) {
		t.Fatalf("1KB policy: oversize = %v", st.Oversize)
	}
	os.WriteFile(filepath.Join(a, ".armageddon", "sync.yaml"), []byte("sync:\n  max_file_size: 1MB\n"), 0o644)
	A.CaptureState() // picks the new policy up
	st, _ = A.CaptureState()
	if len(st.Oversize) != 0 {
		t.Fatalf("1MB policy: oversize = %v", st.Oversize)
	}
}

// TestRegularSizesThroughPrepare covers the server-seat path: stat(1) run
// through Prepare, with awkward names.
func TestRegularSizesThroughPrepare(t *testing.T) {
	dir := t.TempDir()
	names := []string{"plain", "with space", "-dash", "tab\there"}
	for i, n := range names {
		os.WriteFile(filepath.Join(dir, n), []byte(strings.Repeat("x", i+1)), 0o644)
	}
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)
	os.Symlink("plain", filepath.Join(dir, "link"))
	called := 0
	s := &Shadow{WorkTree: dir, Prepare: func(c *exec.Cmd) { called++; c.Env = os.Environ() }}
	got := map[string]int64{}
	if err := s.regularSizes(append(names, "adir", "link", "gone"), func(p string, n int64) { got[p] = n }); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"plain": 1, "with space": 2, "-dash": 3, "tab\there": 4}
	if called == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("sizes = %v (prepare called %d times), want %v", got, called, want)
	}
}

// TestLateOversizeUndone: a file that appears, or grows past the limit,
// between the scan and `git add` is taken back out of the index: a new
// file is dropped, a tracked one returns to its previous version.
func TestLateOversizeUndone(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	os.MkdirAll(a, 0o755)
	git(t, a, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(a, "tracked.bin"), []byte("v1\n"), 0o644)
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "one")
	A := shadow(t, base, "A", a)
	A.MaxFileSize = 1024
	t0, _, err := A.CaptureTree()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := A.sizeScan() // nothing changed yet
	if err != nil || len(pol.big) != 0 {
		t.Fatalf("scan: %v %v", pol.big, err)
	}
	// The race: both change after the scan.
	os.WriteFile(filepath.Join(a, "new.bin"), make([]byte, 4096), 0o644)
	os.WriteFile(filepath.Join(a, "tracked.bin"), make([]byte, 4096), 0o644)
	out, err := A.Git(nil, "add", "-A", "-v", "--", ".")
	if err != nil {
		t.Fatal(err)
	}
	late, err := A.dropLateOversize(out, pol, t0)
	if err != nil || !reflect.DeepEqual(late, []string{"new.bin", "tracked.bin"}) {
		t.Fatalf("late = %v (%v)", late, err)
	}
	tree, _ := A.Git(nil, "write-tree")
	wt := strings.TrimSpace(string(tree))
	if _, err := A.Git(nil, "cat-file", "-e", wt+":new.bin"); err == nil {
		t.Fatal("new.bin is still in the index")
	}
	if got, _ := A.Git(nil, "cat-file", "blob", wt+":tracked.bin"); string(got) != "v1\n" {
		t.Fatalf("tracked.bin = %q, want its previous version", got)
	}
	// The next capture leaves both out without re-adding them.
	st, err := A.CaptureState()
	if err != nil || !reflect.DeepEqual(st.Oversize, []string{"new.bin", "tracked.bin"}) || st.Tree != wt {
		t.Fatalf("next capture: oversize %v tree %s want %s (%v)", st.Oversize, st.Tree, wt, err)
	}
}
