// P2: capture → apply round-trips byte-exactly.
//
// Property-based fuzz. Two user clones A and B of the same repository each
// get a git-shadow. Random edit sequences are applied to the current writer,
// captured as a checkpoint, transferred, and applied to the other side. After
// every round an independent oracle (a plain filesystem walk, no git) compares
// both trees, and `git ls-files -s` compares both indexes.
//
// Also exercised: writer swaps (handoff), injected crashes mid-apply followed
// by resume, divergence detection (I7), and ignored-file tolerance.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"armageddon/prototypes/internal/gitshadow"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	nSeq    = flag.Int("seqs", 200, "number of independent sequences")
	nRounds = flag.Int("rounds", 25, "checkpoint rounds per sequence")
	nOps    = flag.Int("ops", 8, "max edit operations per round")
	seed    = flag.Int64("seed", 1, "base random seed")
	workdir = flag.String("workdir", "", "scratch directory")
	keep    = flag.Bool("keep", false, "keep failing sequence directories")
	mode    = flag.String("mode", "fuzz", "fuzz | export | import | xplat-local (see xplat.go)")
	xdir    = flag.String("dir", "", "export/import directory (cross-platform modes)")
	strict  = flag.Bool("strict", false, "cross-platform modes: exit 1 on a silent mismatch")
)

type stats struct {
	sequences, rounds, ops, applies, crashes, resumes, swaps   int
	divergenceTests, divergenceCaught, ignoredTests, displaced int
	stagedRounds, unmergedRounds, portabilityCollisionsFound   int
	opCounts                                                   map[string]int
	failures                                                   []string
}

func main() {
	flag.Parse()
	if *mode != "fuzz" {
		os.Exit(runXplat(*mode, *xdir, *strict))
	}
	if *workdir == "" {
		d, _ := os.MkdirTemp("", "p2-")
		*workdir = d
	}
	*workdir, _ = filepath.Abs(*workdir)
	st := &stats{opCounts: map[string]int{}}
	start := time.Now()
	portabilitySelfTest(st)
	for i := 0; i < *nSeq; i++ {
		if err := runSequence(st, *seed+int64(i), filepath.Join(*workdir, fmt.Sprintf("seq-%04d", i))); err != nil {
			st.failures = append(st.failures, fmt.Sprintf("seed %d: %v", *seed+int64(i), err))
			fmt.Fprintf(os.Stderr, "FAIL seed %d: %v\n", *seed+int64(i), err)
		} else if !*keep {
			os.RemoveAll(filepath.Join(*workdir, fmt.Sprintf("seq-%04d", i)))
		}
		if (i+1)%20 == 0 {
			fmt.Fprintf(os.Stderr, "... %d/%d sequences, %d failures\n", i+1, *nSeq, len(st.failures))
		}
	}
	fmt.Printf("P2 round-trip fuzz (%s)\n", time.Since(start).Round(time.Second))
	fmt.Printf("sequences=%d rounds=%d edit_ops=%d applies=%d\n", st.sequences, st.rounds, st.ops, st.applies)
	fmt.Printf("writer_swaps=%d injected_crashes=%d resumes_ok=%d\n", st.swaps, st.crashes, st.resumes)
	fmt.Printf("divergence_tests=%d caught=%d ignored_file_tolerance_tests=%d dirs_displaced=%d\n", st.divergenceTests, st.divergenceCaught, st.ignoredTests, st.displaced)
	fmt.Printf("rounds_with_staged_changes=%d rounds_with_unmerged_entries=%d\n", st.stagedRounds, st.unmergedRounds)
	fmt.Printf("portability_collisions_detected=%d\n", st.portabilityCollisionsFound)
	var keys []string
	for k := range st.opCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Print("op mix:")
	for _, k := range keys {
		fmt.Printf(" %s=%d", k, st.opCounts[k])
	}
	fmt.Println()
	fmt.Printf("FAILURES=%d\n", len(st.failures))
	for _, f := range st.failures {
		fmt.Println("  ", f)
	}
	if len(st.failures) > 0 {
		os.Exit(1)
	}
}

type side struct {
	name string
	sh   *gitshadow.Shadow
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("git %v: %v %s", args, err, stderr.String())
	}
	return string(out), nil
}

func runSequence(st *stats, sd int64, dir string) error {
	rng := rand.New(rand.NewSource(sd))
	st.sequences++
	os.RemoveAll(dir)
	a := filepath.Join(dir, "A")
	b := filepath.Join(dir, "B")
	if err := os.MkdirAll(a, 0o755); err != nil {
		return err
	}
	for _, c := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "p2@x"}, {"config", "user.name", "p2"}, {"config", "core.autocrlf", "false"}} {
		if _, err := git(a, c...); err != nil {
			return err
		}
	}
	// Base content: a .gitignore, an eol-normalising .gitattributes (to prove
	// capture ignores it), and a few files.
	must(os.WriteFile(filepath.Join(a, ".gitignore"), []byte("ignored/\n*.log\n.env*\n"), 0o644))
	must(os.WriteFile(filepath.Join(a, ".gitattributes"), []byte("*.txt text eol=lf\n"), 0o644))
	for i := 0; i < 6; i++ {
		createFile(rng, a, st)
	}
	if _, err := git(a, "add", "-A"); err != nil {
		return err
	}
	if _, err := git(a, "commit", "-q", "-m", "base"); err != nil {
		return err
	}
	if _, err := git(dir, "clone", "-q", a, b); err != nil {
		return err
	}
	if _, err := git(b, "config", "core.autocrlf", "false"); err != nil {
		return err
	}
	mk := func(name, wt string) (*side, error) {
		gd := filepath.Join(dir, name+".shadow")
		if err := gitshadow.Init(gd); err != nil {
			return nil, err
		}
		return &side{name, &gitshadow.Shadow{GitDir: gd, WorkTree: wt,
			IndexFile: filepath.Join(dir, name+".capture-index"), Preserve: []string{".env*"}}}, nil
	}
	A, err := mk("A", a)
	if err != nil {
		return err
	}
	B, err := mk("B", b)
	if err != nil {
		return err
	}
	writer, reader := A, B
	ref := "refs/checkpoints/current"
	// Initial checkpoint; the fresh clone must already match it.
	prev, err := writer.sh.Checkpoint(ref, "", 0)
	if err != nil {
		return err
	}
	if err := gitshadow.Transfer(writer.sh, reader.sh, ref); err != nil {
		return err
	}
	if err := reader.sh.Seed(prev); err != nil {
		return fmt.Errorf("initial seed: %w", err)
	}
	if err := compareTrees(writer.sh.WorkTree, reader.sh.WorkTree); err != nil {
		return fmt.Errorf("initial seed oracle: %w", err)
	}

	for r := 1; r <= *nRounds; r++ {
		st.rounds++
		if rng.Float64() < 0.2 { // handoff: the other side becomes writer
			writer, reader = reader, writer
			st.swaps++
		}
		k := 1 + rng.Intn(*nOps)
		for i := 0; i < k; i++ {
			op := randomOp(rng, writer.sh.WorkTree, st)
			st.opCounts[op]++
			st.ops++
		}
		if rng.Float64() < 0.03 {
			if makeConflict(rng, writer.sh.WorkTree) {
				st.unmergedRounds++
			}
		}
		cp, err := writer.sh.Checkpoint(ref, prev, r)
		if err != nil {
			return fmt.Errorf("round %d capture: %w", r, err)
		}
		m, _ := writer.sh.ReadMeta(cp)
		if len(m.Staged) > 0 {
			st.stagedRounds++
		}
		if _, err := gitshadow.TransferPack(writer.sh, reader.sh, ref, cp, prev); err != nil {
			return fmt.Errorf("round %d transfer: %w", r, err)
		}
		portabilityCheck(st, reader.sh, cp)

		// I7: a modified destination must be refused, not overwritten.
		if rng.Float64() < 0.1 {
			if undo := mutateSynced(rng, reader.sh.WorkTree); undo != nil {
				st.divergenceTests++
				err := reader.sh.Apply(prev, cp, gitshadow.ApplyOptions{})
				if !errors.Is(err, gitshadow.ErrDiverged) {
					return fmt.Errorf("round %d: divergence not detected (err=%v)", r, err)
				}
				st.divergenceCaught++
				undo()
			}
		}
		// Ignored files on the destination must not block apply.
		if rng.Float64() < 0.1 {
			st.ignoredTests++
			d := filepath.Join(reader.sh.WorkTree, "ignored")
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, fmt.Sprintf("r%d.log", r)), []byte("local only"), 0o644)
		}

		opt := gitshadow.ApplyOptions{}
		if rng.Float64() < 0.3 {
			opt.CrashAfter = 1 + rng.Intn(4)
		}
		st.applies++
		err = reader.sh.Apply(prev, cp, opt)
		if errors.Is(err, gitshadow.ErrSimulatedCrash) {
			st.crashes++
			if err = reader.sh.Apply(prev, cp, gitshadow.ApplyOptions{Resume: true}); err == nil {
				st.resumes++
			}
		}
		if err != nil {
			return fmt.Errorf("round %d apply %s→%s: %w", r, writer.name, reader.name, err)
		}
		st.displaced += len(reader.sh.Displaced)
		reader.sh.Displaced = nil
		if err := compareTrees(writer.sh.WorkTree, reader.sh.WorkTree); err != nil {
			return fmt.Errorf("round %d oracle (%s→%s): %w", r, writer.name, reader.name, err)
		}
		ia, _ := git(writer.sh.WorkTree, "ls-files", "-s")
		ib, _ := git(reader.sh.WorkTree, "ls-files", "-s")
		if ia != ib {
			return fmt.Errorf("round %d index mismatch %s→%s:\n--- writer\n%s--- reader\n%s", r, writer.name, reader.name, ia, ib)
		}
		prev = cp
	}
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// ---------------------------------------------------------------------------
// Edit generator

var nameParts = []string{
	"a", "B", "readme", "Makefile", "src", "lib", "café", "cafe\u0301", "straße", "STRASSE",
	"日本", "emoji😀", "with space", "-dash", "#hash", "x.y.z", "UPPER", "upper", "über",
	strings.Repeat("long", 40), "back\\slash", "tab\tname", "new\nline", "quote\"d", "ünïcödé",
}

func randName(rng *rand.Rand) string {
	n := nameParts[rng.Intn(len(nameParts))]
	if rng.Float64() < 0.5 {
		n += fmt.Sprintf("%d", rng.Intn(50))
	}
	if rng.Float64() < 0.4 {
		n += []string{".txt", ".go", ".bin", ".sh", ".md"}[rng.Intn(5)]
	}
	return n
}

func randContent(rng *rand.Rand) []byte {
	switch rng.Intn(7) {
	case 0:
		return nil
	case 1: // CRLF text
		return []byte(strings.Repeat("line\r\n", 1+rng.Intn(20)))
	case 2: // mixed endings
		return []byte("a\nb\r\nc\rd")
	case 3: // binary
		b := make([]byte, rng.Intn(4096))
		rng.Read(b)
		return b
	case 4: // unicode
		return []byte("héllo wörld ✓ 日本語\n")
	case 5: // large
		b := bytes.Repeat([]byte("0123456789abcdef"), 1+rng.Intn(20000))
		return b
	default:
		return []byte(fmt.Sprintf("content %d\n", rng.Int()))
	}
}

// walk lists synced (non-ignored) regular files, symlinks and dirs.
func listPaths(root string) (files, dirs []string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if rel == ".git" || ignored(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(filepath.Base(rel), ".git") && rel != ".gitignore" && rel != ".gitattributes" {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
		} else if rel != ".gitignore" && rel != ".gitattributes" {
			files = append(files, rel)
		}
		return nil
	})
	return
}

// ignored mirrors the .gitignore written above, independently of git.
// Preserve (".env*") rescues files only: a directory matching .env* stays
// ignored with everything inside it (it is often a Python virtualenv).
func ignored(rel string) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	for i, c := range parts {
		if c == "ignored" {
			return true
		}
		if i < len(parts)-1 && strings.HasPrefix(c, ".env") {
			return true
		}
	}
	base := filepath.Base(rel)
	return strings.HasSuffix(base, ".log")
}

func randomDir(rng *rand.Rand, root string) string {
	_, dirs := listPaths(root)
	if len(dirs) == 0 || rng.Float64() < 0.3 {
		return root
	}
	return filepath.Join(root, dirs[rng.Intn(len(dirs))])
}

func createFile(rng *rand.Rand, root string, st *stats) string {
	d := randomDir(rng, root)
	if rng.Float64() < 0.3 {
		d = filepath.Join(d, randName(rng))
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "create(mkdir-fail)"
		}
	}
	p := filepath.Join(d, randName(rng))
	if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
		return "create(skip)"
	}
	os.WriteFile(p, randContent(rng), 0o644)
	return "create"
}

func randomOp(rng *rand.Rand, root string, st *stats) string {
	files, dirs := listPaths(root)
	pick := func() string {
		if len(files) == 0 {
			return ""
		}
		return filepath.Join(root, files[rng.Intn(len(files))])
	}
	switch rng.Intn(19) {
	case 0, 1, 2:
		return createFile(rng, root, st)
	case 3, 4:
		if p := pick(); p != "" {
			if fi, _ := os.Lstat(p); fi != nil && fi.Mode().IsRegular() {
				f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
				if f != nil {
					c := randContent(rng)
					f.Write(c[:min(64, len(c))])
					f.Write([]byte{byte(rng.Intn(256))})
					f.Close()
				}
			}
			return "modify"
		}
	case 5:
		if p := pick(); p != "" {
			os.Remove(p)
			return "delete"
		}
	case 6:
		if p := pick(); p != "" {
			dst := filepath.Join(randomDir(rng, root), randName(rng))
			if _, err := os.Lstat(dst); err == nil {
				return "rename(skip)"
			}
			os.Rename(p, dst)
			return "rename"
		}
	case 7: // case-only rename (a collision on macOS/Windows)
		if p := pick(); p != "" {
			base := filepath.Base(p)
			nb := strings.ToUpper(base)
			if nb == base {
				nb = strings.ToLower(base)
			}
			if nb != base {
				dst := filepath.Join(filepath.Dir(p), nb)
				if _, err := os.Lstat(dst); err != nil {
					os.Rename(p, dst)
					return "case-rename"
				}
			}
		}
	case 8:
		if p := pick(); p != "" {
			if fi, _ := os.Lstat(p); fi != nil && fi.Mode().IsRegular() {
				os.Chmod(p, fi.Mode().Perm()^0o111)
				return "chmod"
			}
		}
	case 9: // new symlink: relative, dangling, or absolute
		d := randomDir(rng, root)
		p := filepath.Join(d, "link-"+randName(rng))
		var target string
		switch rng.Intn(3) {
		case 0:
			if f := pick(); f != "" {
				target, _ = filepath.Rel(d, f)
			}
		case 1:
			target = "does/not/exist"
		default:
			target = "/etc/hostname"
		}
		if target != "" {
			os.Symlink(target, p)
			return "symlink"
		}
	case 10: // file ↔ symlink
		if p := pick(); p != "" {
			fi, _ := os.Lstat(p)
			os.Remove(p)
			if fi != nil && fi.Mode()&os.ModeSymlink != 0 {
				os.WriteFile(p, []byte("was a link\n"), 0o644)
			} else {
				os.Symlink("target-"+randName(rng), p)
			}
			return "file<->symlink"
		}
	case 11: // file → directory
		if p := pick(); p != "" {
			os.Remove(p)
			os.MkdirAll(p, 0o755)
			os.WriteFile(filepath.Join(p, "child"), []byte("child\n"), 0o644)
			return "file->dir"
		}
	case 12: // directory → file
		if len(dirs) > 0 {
			d := filepath.Join(root, dirs[rng.Intn(len(dirs))])
			os.RemoveAll(d)
			os.WriteFile(d, []byte("now a file\n"), 0o644)
			return "dir->file"
		}
	case 13:
		d := filepath.Join(randomDir(rng, root), "empty-"+randName(rng))
		if rng.Float64() < 0.3 {
			d = filepath.Join(d, "nested-empty")
		}
		os.MkdirAll(d, 0o755)
		return "mkdir-empty"
	case 14: // ignored noise and preserved .env files
		if rng.Float64() < 0.5 {
			d := filepath.Join(root, "ignored", randName(rng))
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, "x"), randContent(rng), 0o644)
			os.WriteFile(filepath.Join(randomDir(rng, root), "debug.log"), []byte("noise"), 0o644)
			return "ignored-noise"
		}
		os.WriteFile(filepath.Join(randomDir(rng, root), []string{".env", ".env.local", ".env.test"}[rng.Intn(3)]),
			[]byte(fmt.Sprintf("SECRET=%d\n", rng.Int())), 0o600)
		return "env-file"
	case 15, 16: // stage a change (index differs from HEAD and worktree)
		if p := pick(); p != "" {
			rel, _ := filepath.Rel(root, p)
			if fi, _ := os.Lstat(p); fi != nil && fi.Mode().IsRegular() && rng.Float64() < 0.5 {
				os.WriteFile(p, randContent(rng), fi.Mode().Perm())
				git(root, "add", "--", rel)
				f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
				if f != nil {
					f.WriteString("unstaged tail\n")
					f.Close()
				}
				return "stage-partial"
			}
			git(root, "add", "-f", "--", rel)
			return "stage"
		}
	case 17:
		if p := pick(); p != "" {
			rel, _ := filepath.Rel(root, p)
			if rng.Float64() < 0.5 {
				git(root, "reset", "-q", "--", rel)
				return "unstage"
			}
			git(root, "rm", "-q", "--cached", "--ignore-unmatch", "--", rel)
			return "stage-delete"
		}
	case 18: // remove a directory tree
		if len(dirs) > 0 {
			os.RemoveAll(filepath.Join(root, dirs[rng.Intn(len(dirs))]))
			return "rmdir"
		}
	}
	return "noop"
}

// makeConflict writes unmerged stages for one path directly into the index,
// as an interrupted merge would leave them.
func makeConflict(rng *rand.Rand, root string) bool {
	files, _ := listPaths(root)
	if len(files) == 0 {
		return false
	}
	rel := files[rng.Intn(len(files))]
	if fi, _ := os.Lstat(filepath.Join(root, rel)); fi == nil || !fi.Mode().IsRegular() {
		return false
	}
	var oids []string
	for i := 0; i < 3; i++ {
		cmd := exec.Command("git", "hash-object", "-w", "--stdin")
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(fmt.Sprintf("stage %d %d\n", i+1, rng.Int()))
		out, err := cmd.Output()
		if err != nil {
			return false
		}
		oids = append(oids, strings.TrimSpace(string(out)))
	}
	info := fmt.Sprintf("0 %s\t%s\x00100644 %s 1\t%s\x00100644 %s 2\t%s\x00100644 %s 3\t%s\x00",
		strings.Repeat("0", 40), rel, oids[0], rel, oids[1], rel, oids[2], rel)
	cmd := exec.Command("git", "update-index", "-z", "--index-info")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(info)
	return cmd.Run() == nil
}

// mutateSynced changes one synced file on the destination and returns an
// undo function, or nil if there is nothing to mutate.
func mutateSynced(rng *rand.Rand, root string) func() {
	files, _ := listPaths(root)
	var regular []string
	for _, f := range files {
		if fi, _ := os.Lstat(filepath.Join(root, f)); fi != nil && fi.Mode().IsRegular() {
			regular = append(regular, f)
		}
	}
	if len(regular) == 0 {
		return nil
	}
	p := filepath.Join(root, regular[rng.Intn(len(regular))])
	orig, _ := os.ReadFile(p)
	fi, _ := os.Lstat(p)
	os.WriteFile(p, append(append([]byte{}, orig...), "DIVERGED"...), fi.Mode().Perm())
	return func() { os.WriteFile(p, orig, fi.Mode().Perm()) }
}

// ---------------------------------------------------------------------------
// Oracle: independent filesystem comparison (no git involved).

type entry struct {
	kind    string // file, link, emptydir
	exec    bool
	content string
}

func snapshot(root string) (map[string]entry, error) {
	out := map[string]entry{}
	var nonEmpty []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		// A directory named .env* is ignored as a whole (preserve is
		// file-only), so it is neither content nor an empty directory.
		if rel == ".git" || ignored(rel) || (d.IsDir() && strings.HasPrefix(d.Name(), ".env")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(filepath.Base(rel), ".armageddon-tmp-") {
			return fmt.Errorf("leftover temp file %q", rel)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			out[rel] = entry{kind: "link", content: t}
		case fi.IsDir():
			// A truly empty directory must replicate. A directory holding
			// only ignored content is "don't care": Git treats it as ignored,
			// and whatever recreates the ignored files recreates it.
			ents, _ := os.ReadDir(p)
			if len(ents) == 0 {
				out[rel] = entry{kind: "emptydir"}
			} else {
				nonEmpty = append(nonEmpty, rel)
			}
		default:
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = entry{kind: "file", exec: fi.Mode()&0o100 != 0, content: string(c)}
		}
		return nil
	})
	// Non-empty directories with no synced descendant hold only ignored
	// content.
	hasSynced := map[string]bool{}
	for k := range out {
		for d := filepath.Dir(k); d != "." && !hasSynced[d]; d = filepath.Dir(d) {
			hasSynced[d] = true
		}
	}
	for _, d := range nonEmpty {
		if !hasSynced[d] {
			out[d] = entry{kind: "ignoredonly"}
		}
	}
	return out, err
}

func compareTrees(a, b string) error {
	sa, err := snapshot(a)
	if err != nil {
		return err
	}
	sb, err := snapshot(b)
	if err != nil {
		return err
	}
	var diffs []string
	dirLike := func(e entry) bool { return e.kind == "emptydir" || e.kind == "ignoredonly" }
	for k, va := range sa {
		vb, ok := sb[k]
		switch {
		case !ok && va.kind == "ignoredonly":
		case ok && dirLike(va) && dirLike(vb):
		case !ok:
			diffs = append(diffs, fmt.Sprintf("missing on dest: %q (%s)", k, va.kind))
		case va != vb:
			diffs = append(diffs, fmt.Sprintf("differs: %q (%s/%v vs %s/%v)", k, va.kind, va.exec, vb.kind, vb.exec))
		}
	}
	for k, vb := range sb {
		if _, ok := sa[k]; !ok && vb.kind != "ignoredonly" {
			diffs = append(diffs, fmt.Sprintf("extra on dest: %q (%s)", k, vb.kind))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		if len(diffs) > 10 {
			diffs = append(diffs[:10], fmt.Sprintf("... %d more", len(diffs)-10))
		}
		return errors.New(strings.Join(diffs, "\n  "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Portability: paths that collide on case-insensitive or normalising
// filesystems (macOS APFS default, Windows). v0.1 policy: detect and report.

var folder = cases.Fold()

func collisionKey(p string) string { return folder.String(norm.NFC.String(p)) }

// Collisions groups distinct paths (including every directory prefix, since
// "A/x" and "a/y" also collide) that map to the same name on a
// case-insensitive, normalisation-insensitive filesystem.
func Collisions(paths []string) [][]string {
	all := map[string]bool{}
	for _, p := range paths {
		parts := strings.Split(p, "/")
		for i := 1; i <= len(parts); i++ {
			all[strings.Join(parts[:i], "/")] = true
		}
	}
	groups := map[string][]string{}
	for p := range all {
		k := collisionKey(p)
		groups[k] = append(groups[k], p)
	}
	var out [][]string
	for _, g := range groups {
		if len(g) > 1 {
			sort.Strings(g)
			out = append(out, g)
		}
	}
	return out
}

func portabilityCheck(st *stats, sh *gitshadow.Shadow, cp string) {
	out, err := sh.Git(nil, "ls-tree", "-r", "-z", "--name-only", cp+":worktree")
	if err != nil {
		return
	}
	paths := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	st.portabilityCollisionsFound += len(Collisions(paths))
}

func portabilitySelfTest(st *stats) {
	cases := []struct {
		paths []string
		want  int
	}{
		{[]string{"README.md", "readme.md"}, 1},
		{[]string{"café", "cafe\u0301"}, 1}, // NFC vs NFD
		{[]string{"straße", "STRASSE"}, 1},  // full case folding
		{[]string{"a/b", "A/b"}, 2},         // the dirs "a"/"A" and the files collide
		{[]string{"A/x", "a/y"}, 1},         // only the directory collides
		{[]string{"a", "b", "c"}, 0},
	}
	for _, c := range cases {
		if got := len(Collisions(c.paths)); got != c.want {
			st.failures = append(st.failures, fmt.Sprintf("portability self-test %v: got %d want %d", c.paths, got, c.want))
		}
	}
}
