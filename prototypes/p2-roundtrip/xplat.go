// Cross-platform mode for P2 (Phase 1): a fixed scenario of portability
// focus cases (case-only renames, names differing only in case, NFC vs NFD,
// the executable bit, symlinks), exported on one OS and applied on another.
//
//	p2-roundtrip -mode export -dir OUT   capture each step, write bundles + manifest
//	p2-roundtrip -mode import -dir OUT   seed/apply every step, compare, report
//	p2-roundtrip -mode xplat-local       both, on this machine (same-OS baseline)
//
// The import side REPORTS what happened per step. It does not encode an
// expectation of how APFS behaves: that is what the run is for. Each step is
// classified as one of:
//
//	match                apply succeeded and the tree equals the exporter's
//	refused              apply returned an error (nothing silently merged)
//	mismatch-detected    tree differs, and the checkpoint has a case/NFC
//	                     collision that Collisions() flags (detectable)
//	mismatch-SILENT      tree differs and nothing flagged it (the bad case)
//
// -strict makes mismatch-SILENT a non-zero exit.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"armageddon/prototypes/internal/gitshadow"
)

type xEntry struct {
	Kind string `json:"kind"`
	Exec bool   `json:"exec,omitempty"`
	Hash string `json:"hash,omitempty"` // sha256 of content or link target
}

type xStep struct {
	Name       string            `json:"name"`
	Checkpoint string            `json:"checkpoint"`
	Tree       map[string]xEntry `json:"tree"`
	Collisions [][]string        `json:"collisions,omitempty"`
	Note       string            `json:"note,omitempty"` // exporter-side observation
}

type xManifest struct {
	OS    string  `json:"os"`
	Steps []xStep `json:"steps"`
}

type xResult struct {
	Step, Outcome, Detail string
	Collisions            int
}

const nfc, nfd = "café.txt", "café.txt"

// xSteps mutates the exporter's tree; each step is followed by a checkpoint.
// Renames that differ only in case go through a temporary name so they also
// work on a case-insensitive exporter.
var xSteps = []struct {
	name string
	do   func(root string) string
}{
	{"base", func(r string) string {
		wf(r, "README.md", "readme v1\n", 0o644)
		wf(r, nfc, "nfc\n", 0o644)
		wf(r, "run.sh", "#!/bin/sh\necho hi\n", 0o755)
		wf(r, "dir/file.txt", "in dir\n", 0o644)
		must(os.Symlink("README.md", filepath.Join(r, "link")))
		return ""
	}},
	{"case-only rename (file and directory)", func(r string) string {
		caseRename(r, "README.md", "readme.md")
		caseRename(r, "dir", "Dir")
		return ""
	}},
	{"executable bit flip", func(r string) string {
		must(os.Chmod(filepath.Join(r, "run.sh"), 0o644))
		must(os.Chmod(filepath.Join(r, "Dir/file.txt"), 0o755))
		return ""
	}},
	{"NFC and NFD names side by side", func(r string) string {
		wf(r, nfd, "nfd\n", 0o644)
		return countNote(r, nfc, nfd)
	}},
	{"names differing only in case", func(r string) string {
		wf(r, "Makefile", "upper\n", 0o644)
		wf(r, "makefile", "lower\n", 0o644)
		return countNote(r, "Makefile", "makefile")
	}},
	{"symlink retarget, dangling, case-variant target", func(r string) string {
		must(os.Remove(filepath.Join(r, "link")))
		must(os.Symlink("Dir/file.txt", filepath.Join(r, "link")))
		must(os.Symlink("does-not-exist", filepath.Join(r, "dangling")))
		must(os.Symlink("README.MD", filepath.Join(r, "link-case")))
		return ""
	}},
	{"resolve collisions", func(r string) string {
		os.Remove(filepath.Join(r, "makefile"))
		os.Remove(filepath.Join(r, nfd))
		return ""
	}},
}

func wf(root, rel, content string, mode os.FileMode) {
	p := filepath.Join(root, rel)
	must(os.MkdirAll(filepath.Dir(p), 0o755))
	must(os.WriteFile(p, []byte(content), mode))
	must(os.Chmod(p, mode))
}

func caseRename(root, from, to string) {
	tmp := filepath.Join(root, ".tmp-rename")
	must(os.Rename(filepath.Join(root, from), tmp))
	must(os.Rename(tmp, filepath.Join(root, to)))
}

// countNote records how many of the given names the exporter's filesystem
// actually kept as separate entries.
func countNote(root string, names ...string) string {
	ents, _ := os.ReadDir(root)
	n := 0
	for _, e := range ents {
		for _, want := range names {
			if e.Name() == want {
				n++
			}
		}
	}
	return fmt.Sprintf("exporter kept %d of %d distinct names", n, len(names))
}

func xSnapshot(root string) (map[string]xEntry, error) {
	snap, err := snapshot(root)
	if err != nil {
		return nil, err
	}
	out := map[string]xEntry{}
	for k, e := range snap {
		x := xEntry{Kind: e.kind, Exec: e.exec}
		if e.kind == "file" || e.kind == "link" {
			h := sha256.Sum256([]byte(e.content))
			x.Hash = hex.EncodeToString(h[:8])
		}
		out[filepath.ToSlash(k)] = x
	}
	return out, nil
}

func cpPaths(sh *gitshadow.Shadow, cp string) []string {
	out, err := sh.Git(nil, "ls-tree", "-r", "-z", "--name-only", cp+":worktree")
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
}

func xExport(dir string) error {
	work := filepath.Join(dir, "work")
	os.RemoveAll(work)
	a := filepath.Join(work, "A")
	must(os.MkdirAll(a, 0o755))
	for _, c := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "p2@x"},
		{"config", "user.name", "p2"}, {"config", "core.autocrlf", "false"}} {
		if _, err := git(a, c...); err != nil {
			return err
		}
	}
	wf(a, ".gitignore", "ignored/\n*.log\n", 0o644)
	if _, err := git(a, "add", "-A"); err != nil {
		return err
	}
	if _, err := git(a, "commit", "-q", "-m", "base"); err != nil {
		return err
	}
	gd := filepath.Join(work, "A.shadow")
	if err := gitshadow.Init(gd); err != nil {
		return err
	}
	sh := &gitshadow.Shadow{GitDir: gd, WorkTree: a, IndexFile: filepath.Join(work, "A.capture-index")}
	m := xManifest{OS: runtime.GOOS + "/" + runtime.GOARCH}
	prev := ""
	for i, s := range xSteps {
		note := s.do(a)
		ref := fmt.Sprintf("refs/xplat/step-%02d", i)
		cp, err := sh.Checkpoint(ref, prev, i)
		if err != nil {
			return fmt.Errorf("step %q capture: %w", s.name, err)
		}
		tree, err := xSnapshot(a)
		if err != nil {
			return err
		}
		m.Steps = append(m.Steps, xStep{Name: s.name, Checkpoint: cp, Tree: tree,
			Collisions: Collisions(cpPaths(sh, cp)), Note: note})
		prev = cp
	}
	out := filepath.Join(dir, "out")
	must(os.MkdirAll(out, 0o755))
	if _, err := git(a, "bundle", "create", filepath.Join(out, "user.bundle"), "--all"); err != nil {
		return err
	}
	if _, err := sh.Git(nil, "bundle", "create", filepath.Join(out, "shadow.bundle"), "--all"); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), b, 0o644); err != nil {
		return err
	}
	fmt.Printf("exported %d steps from %s to %s\n", len(m.Steps), m.OS, out)
	for _, s := range m.Steps {
		if s.Note != "" {
			fmt.Printf("  %s: %s\n", s.Name, s.Note)
		}
	}
	return nil
}

func xImport(dir string) ([]xResult, xManifest, error) {
	var m xManifest
	out := filepath.Join(dir, "out")
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		return nil, m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, m, err
	}
	work := filepath.Join(dir, "import")
	os.RemoveAll(work)
	must(os.MkdirAll(work, 0o755))
	b := filepath.Join(work, "B")
	if _, err := git(work, "clone", "-q", filepath.Join(out, "user.bundle"), b); err != nil {
		return nil, m, err
	}
	if _, err := git(b, "config", "core.autocrlf", "false"); err != nil {
		return nil, m, err
	}
	gd := filepath.Join(work, "B.shadow")
	if err := gitshadow.Init(gd); err != nil {
		return nil, m, err
	}
	sh := &gitshadow.Shadow{GitDir: gd, WorkTree: b, IndexFile: filepath.Join(work, "B.capture-index")}
	if _, err := sh.Git(nil, "fetch", "-q", filepath.Join(out, "shadow.bundle"), "+refs/*:refs/*"); err != nil {
		return nil, m, err
	}
	var res []xResult
	prev := ""
	for i, s := range m.Steps {
		var err error
		if i == 0 || prev == "" {
			err = sh.Seed(s.Checkpoint)
		} else {
			err = sh.Apply(prev, s.Checkpoint, gitshadow.ApplyOptions{})
		}
		r := xResult{Step: s.Name, Collisions: len(s.Collisions)}
		if err != nil {
			r.Outcome, r.Detail = "refused", oneLine(err.Error())
			// Resynchronise from the replica's own capture for the next step.
			if serr := sh.Seed(s.Checkpoint); serr != nil {
				r.Detail += " | reseed: " + oneLine(serr.Error())
				prev = ""
			} else {
				prev = s.Checkpoint
			}
			res = append(res, r)
			continue
		}
		prev = s.Checkpoint
		got, err := xSnapshot(b)
		if err != nil {
			return nil, m, err
		}
		diffs := diffSnap(s.Tree, got)
		switch {
		case len(diffs) == 0:
			r.Outcome = "match"
		case len(s.Collisions) > 0:
			r.Outcome, r.Detail = "mismatch-detected", strings.Join(diffs, "; ")
		default:
			r.Outcome, r.Detail = "mismatch-SILENT", strings.Join(diffs, "; ")
		}
		res = append(res, r)
	}
	return res, m, nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func diffSnap(want, got map[string]xEntry) []string {
	var d []string
	for k, w := range want {
		g, ok := got[k]
		switch {
		case !ok && w.Kind == "ignoredonly":
		case !ok:
			d = append(d, fmt.Sprintf("missing %q", k))
		case g != w:
			d = append(d, fmt.Sprintf("differs %q (%s/x=%v vs %s/x=%v)", k, w.Kind, w.Exec, g.Kind, g.Exec))
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok && g.Kind != "ignoredonly" {
			d = append(d, fmt.Sprintf("extra %q", k))
		}
	}
	sort.Strings(d)
	return d
}

func xReport(res []xResult, m xManifest) (silent int) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### P2 cross-platform: exported on `%s`, applied on `%s/%s`\n\n", m.OS, runtime.GOOS, runtime.GOARCH)
	sb.WriteString("| Step | Outcome | Collisions flagged | Exporter note | Detail |\n|---|---|--:|---|---|\n")
	for i, r := range res {
		if r.Outcome == "mismatch-SILENT" {
			silent++
		}
		note := ""
		if i < len(m.Steps) {
			note = m.Steps[i].Note
		}
		fmt.Fprintf(&sb, "| %s | **%s** | %d | %s | %s |\n", r.Step, r.Outcome, r.Collisions, note,
			strings.ReplaceAll(r.Detail, "|", "\\|"))
	}
	fmt.Fprintf(&sb, "\nSILENT mismatches: %d\n", silent)
	fmt.Print(sb.String())
	if f := os.Getenv("GITHUB_STEP_SUMMARY"); f != "" {
		if fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			fh.WriteString(sb.String() + "\n")
			fh.Close()
		}
	}
	return silent
}

// runXplat dispatches the non-fuzz modes and returns the exit code.
func runXplat(mode, dir string, strict bool) int {
	if dir == "" {
		d, _ := os.MkdirTemp("", "p2x-")
		dir = d
	}
	dir, _ = filepath.Abs(dir)
	var err error
	switch mode {
	case "export":
		err = xExport(dir)
	case "import", "xplat-local":
		if mode == "xplat-local" {
			if err = xExport(dir); err != nil {
				break
			}
		}
		var res []xResult
		var m xManifest
		res, m, err = xImport(dir)
		if err == nil && xReport(res, m) > 0 && strict {
			return 1
		}
	default:
		err = errors.New("unknown -mode " + mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "p2:", err)
		return 2
	}
	return 0
}
