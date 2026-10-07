package gitshadow

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const nfc, nfd = "café.txt", "café.txt"

func withForceFold(t *testing.T) {
	t.Helper()
	old := forceFold
	forceFold = true
	t.Cleanup(func() { forceFold = old })
}

func treePaths(t *testing.T, s *Shadow, tree string) []string {
	t.Helper()
	out, err := s.Git(nil, "ls-tree", "-r", "-z", "--name-only", tree)
	if err != nil {
		t.Fatal(err)
	}
	return splitZ(out)
}

// renameVia renames through a temporary name so a case- or
// normalisation-only rename also works on an insensitive filesystem.
func renameVia(t *testing.T, root, from, to string) {
	t.Helper()
	tmp := filepath.Join(root, ".tmp-rename")
	if err := os.Rename(filepath.Join(root, from), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(root, to)); err != nil {
		t.Fatal(err)
	}
}

func TestCollisions(t *testing.T) {
	for _, c := range []struct {
		paths []string
		want  int
	}{
		{[]string{"README.md", "readme.md"}, 1},
		{[]string{nfc, nfd}, 1},
		{[]string{"straße", "STRASSE"}, 1}, // full case folding
		{[]string{"a/b", "A/b"}, 2},        // the dirs and the files collide
		{[]string{"A/x", "a/y"}, 1},        // only the directory collides
		{[]string{"a", "b", "c"}, 0},
	} {
		if got := len(Collisions(c.paths)); got != c.want {
			t.Errorf("Collisions(%q) = %d groups, want %d", c.paths, got, c.want)
		}
	}
}

// TestCaseOnlyRenameCapture runs everywhere; on the macOS CI leg
// (case-insensitive APFS) it is the regression test for ⟨P-20⟩: before the
// fix the captured tree kept the old spellings next to the new ones.
func TestCaseOnlyRenameCapture(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "scoped"}[scoped], func(t *testing.T) {
			base := t.TempDir()
			wt := filepath.Join(base, "wt")
			os.MkdirAll(filepath.Join(wt, "dir"), 0o755)
			os.WriteFile(filepath.Join(wt, "README.md"), []byte("readme\n"), 0o644)
			os.WriteFile(filepath.Join(wt, "dir", "file.txt"), []byte("in dir\n"), 0o644)
			os.WriteFile(filepath.Join(wt, nfc), []byte("nfc\n"), 0o644)
			s := shadow(t, base, "S", wt)
			if _, _, err := s.CaptureTree(); err != nil {
				t.Fatal(err)
			}
			t.Logf("insensitive filesystem: %v", probeInsensitive(wt))

			renameVia(t, wt, "README.md", "readme.md")
			renameVia(t, wt, "dir", "Dir")
			renameVia(t, wt, nfc, nfd)
			var tree string
			var err error
			if scoped {
				// What a watcher reports: both spellings of each rename.
				tree, _, err = s.CaptureTreeScoped([]string{"README.md", "readme.md", "dir", "Dir", nfc, nfd}, nil)
			} else {
				tree, _, err = s.CaptureTree()
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"Dir/file.txt", nfd, "readme.md"}
			if got := treePaths(t, s, tree); !reflect.DeepEqual(got, want) {
				t.Fatalf("captured %q, want %q", got, want)
			}
		})
	}
}

// TestDropFoldedStale exercises the index reconciliation on any filesystem
// by planting the stale entry an insensitive filesystem would leave behind.
func TestDropFoldedStale(t *testing.T) {
	withForceFold(t)
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	os.MkdirAll(filepath.Join(wt, "sub"), 0o755)
	os.WriteFile(filepath.Join(wt, "readme.md"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "sub", "a.txt"), []byte("y\n"), 0o644)
	s := shadow(t, base, "S", wt)
	if _, _, err := s.CaptureTree(); err != nil {
		t.Fatal(err)
	}
	plant := func(paths ...string) {
		oid := BlobOid([]byte("x\n"))
		for _, p := range paths {
			if _, err := s.Git(nil, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+p); err != nil {
				t.Fatal(err)
			}
		}
	}
	ls := func() string {
		out, err := s.Git(nil, "ls-files")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	plant("README.md", "SUB/a.txt", "sub/A.txt")
	// Scoped to sub: only the stale entry inside sub goes. SUB/a.txt is
	// outside the "sub" pathspec (byte-exact matching) and stays.
	if err := s.dropFoldedStale("sub"); err != nil {
		t.Fatal(err)
	}
	if got, want := ls(), "README.md\nSUB/a.txt\nreadme.md\nsub/a.txt"; got != want {
		t.Fatalf("after scoped drop:\n%s\nwant:\n%s", got, want)
	}
	if err := s.dropFoldedStale(); err != nil {
		t.Fatal(err)
	}
	if got, want := ls(), "readme.md\nsub/a.txt"; got != want {
		t.Fatalf("after full drop:\n%s\nwant:\n%s", got, want)
	}
}

// TestApplyRefusesCollisions covers ⟨P-21⟩: a checkpoint holding paths that
// name the same file on the replica is refused before anything is written,
// with a collision error rather than ErrDiverged.
func TestApplyRefusesCollisions(t *testing.T) {
	withForceFold(t)
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	os.MkdirAll(a, 0o755)
	git(t, a, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(a, "keep.txt"), []byte("keep\n"), 0o644)
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "one")
	git(t, base, "clone", "-q", a, b)
	A, B := shadow(t, base, "A", a), shadow(t, base, "B", b)
	ref := "refs/checkpoints/current"
	c0, err := A.Checkpoint(ref, "", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Build the colliding tree directly, as a case-sensitive writer would
	// capture it: an insensitive test filesystem cannot hold both names.
	blob := func(s string) string {
		out, err := A.Git([]byte(s), "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	keep, upper, lower := blob("keep\n"), blob("upper\n"), blob("lower\n")
	out, err := A.Git([]byte("100644 blob "+upper+"\tMakefile\n100644 blob "+keep+"\tkeep.txt\n100644 blob "+lower+"\tmakefile\n"), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := A.CommitState(State{Tree: strings.TrimSpace(string(out)), EmptyDirs: []string{}, Staged: []StagedEntry{}}, ref, c0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TransferPack(A, B, ref, c0, ""); err != nil {
		t.Fatal(err)
	}
	if err := B.Seed(c0); err != nil {
		t.Fatal(err)
	}
	if _, err := TransferPack(A, B, ref, c1, c0); err != nil {
		t.Fatal(err)
	}

	for name, apply := range map[string]func() error{
		"apply": func() error { return B.Apply(c0, c1, ApplyOptions{}) },
		"seed":  func() error { return B.Seed(c1) },
	} {
		err := apply()
		var ce *CollisionError
		if !errors.As(err, &ce) {
			t.Fatalf("%s: want *CollisionError, got %v", name, err)
		}
		if errors.Is(err, ErrDiverged) || strings.Contains(err.Error(), "diverged") {
			t.Fatalf("%s: a collision must not read as divergence: %v", name, err)
		}
		if want := [][]string{{"Makefile", "makefile"}}; !reflect.DeepEqual(ce.Groups, want) {
			t.Fatalf("%s: groups %q, want %q", name, ce.Groups, want)
		}
		ents, _ := os.ReadDir(b)
		if len(ents) != 2 || ents[1].Name() != "keep.txt" { // .git, keep.txt
			t.Fatalf("%s: replica was modified: %v", name, ents)
		}
	}
}
