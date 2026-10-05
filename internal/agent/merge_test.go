package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshot(t *testing.T, sh *gitshadow.Shadow) string {
	t.Helper()
	tree, st, err := sh.CaptureTree()
	if err != nil {
		t.Fatal(err)
	}
	cp, err := sh.CommitState(gitshadow.State{Tree: tree, EmptyDirs: st.EmptyDirs}, "refs/test/x", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// quarantine apply is a 3-way merge: base = the quarantine's parent,
// ours = the working tree, theirs = the quarantine (§6.7, M7.7).
func TestQuarantineMerge3(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	os.MkdirAll(wt, 0o755)
	sh := &gitshadow.Shadow{GitDir: filepath.Join(root, "shadow.git"), WorkTree: wt, IndexFile: filepath.Join(root, "idx")}
	if err := gitshadow.Init(sh.GitDir, nil); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"overlap.txt": "one\ntwo\nthree\n",
		"clean.txt":   "a\nb\nc\nd\ne\nf\ng\n",
		"gone.txt":    "deleted in the quarantine, edited here\n",
		"only-q.txt":  "edited only in the quarantine\n",
	}
	writeTree(t, wt, base)
	baseCP := snapshot(t, sh)
	theirs := map[string]string{
		"overlap.txt": "ONE (quarantine)\ntwo\nthree\n",
		"clean.txt":   "a\nb\nc\nd\ne\nf\nG (quarantine)\n",
		"only-q.txt":  "changed in the quarantine\n",
		"new.txt":     "added in the quarantine\n",
	}
	writeTree(t, wt, theirs)
	theirsCP := snapshot(t, sh)
	ours := map[string]string{
		"overlap.txt": "ONE (local)\ntwo\nthree\n",
		"clean.txt":   "A (local)\nb\nc\nd\ne\nf\ng\n",
		"gone.txt":    "edited here\n",
		"only-q.txt":  "edited only in the quarantine\n",
	}
	writeTree(t, wt, ours)
	res, err := merge3(sh, wt, baseCP+":worktree", theirsCP+":worktree", "Q1")
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(wt, p)); return string(b) }
	if o := read("overlap.txt"); !strings.Contains(o, "<<<<<<< local") || !strings.Contains(o, "ONE (local)") ||
		!strings.Contains(o, "ONE (quarantine)") || !strings.Contains(o, ">>>>>>> quarantine Q1") {
		t.Fatalf("overlap not marked as a conflict:\n%s", o)
	}
	if c := read("clean.txt"); c != "A (local)\nb\nc\nd\ne\nf\nG (quarantine)\n" {
		t.Fatalf("clean merge = %q", c)
	}
	if read("only-q.txt") != "changed in the quarantine\n" || read("new.txt") != "added in the quarantine\n" {
		t.Fatal("one-sided quarantine changes not taken")
	}
	if read("gone.txt") != "edited here\n" {
		t.Fatal("a local edit was dropped because the quarantine deleted the file")
	}
	got := strings.Join(res.Conflicts, ",") + "|" + strings.Join(res.Merged, ",") + "|" + strings.Join(res.Kept, ",")
	if !strings.HasPrefix(got, "overlap.txt|clean.txt|gone.txt") {
		t.Fatalf("result = %s (taken %v)", got, res.Taken)
	}
}
