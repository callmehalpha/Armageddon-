// P1: is git-shadow capture fast enough for continuous checkpoints?
//
// Builds synthetic repositories (committed source tree + an ignored
// node_modules), then measures cold capture, no-op capture, incremental
// capture after k modified files, full checkpoint creation, apply latency,
// per-phase cost, config variants, and shadow growth over many checkpoints.
// Optionally measures a real repository given with -real.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"armageddon/prototypes/internal/gitshadow"
)

var (
	sizes     = flag.String("sizes", "5000,50000,200000", "tracked file counts to test")
	nodeMods  = flag.Int("node-modules", 100000, "ignored node_modules files")
	reps      = flag.Int("reps", 30, "repetitions per incremental measurement")
	growthN   = flag.Int("growth", 1000, "checkpoints for the growth test (at the middle size)")
	workdir   = flag.String("workdir", "", "scratch directory")
	realRepo  = flag.String("real", "", "optional path to a real repository working tree to measure")
	skipBuild = flag.Bool("reuse", false, "reuse previously generated trees")
	scoped    = flag.Bool("scoped", false, "only measure watcher-scoped capture vs full capture")
)

func main() {
	flag.Parse()
	if *workdir == "" {
		*workdir, _ = os.MkdirTemp("", "p1-")
	}
	*workdir, _ = filepath.Abs(*workdir)
	fmt.Printf("P1 capture performance — git %s, %s\n", gitVersion(), time.Now().Format(time.RFC3339))
	fmt.Printf("host: %d CPUs; page cache warm (cold-disk not measured)\n\n", numCPU())

	var ns []int
	for _, s := range strings.Split(*sizes, ",") {
		var n int
		fmt.Sscanf(s, "%d", &n)
		ns = append(ns, n)
	}
	for i, n := range ns {
		dir := filepath.Join(*workdir, fmt.Sprintf("tree-%d", n))
		if !*skipBuild || !exists(dir) {
			t := time.Now()
			build(dir, n, *nodeMods)
			fmt.Printf("[built %d tracked + %d ignored files in %s]\n", n, *nodeMods, time.Since(t).Round(time.Second))
		}
		if *scoped {
			measureScoped(fmt.Sprintf("synthetic %d files + %d ignored", n, *nodeMods), dir)
			continue
		}
		measure(fmt.Sprintf("synthetic %d files + %d ignored", n, *nodeMods), dir, i == len(ns)/2)
	}
	if *realRepo != "" {
		measure("real: "+filepath.Base(*realRepo), *realRepo, false)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func numCPU() int {
	out, _ := exec.Command("nproc").Output()
	var n int
	fmt.Sscanf(string(out), "%d", &n)
	return n
}

func gitVersion() string {
	out, _ := exec.Command("git", "version").Output()
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "git version ")
}

func sh(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=p1", "GIT_AUTHOR_EMAIL=p1@x", "GIT_COMMITTER_NAME=p1", "GIT_COMMITTER_EMAIL=p1@x")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("git %v: %v\n%s", args, err, out))
	}
}

// build writes a source-like tree: ~20 files per directory, depth up to 5,
// sizes mostly 0.5–8 KiB with a long tail, text-like content.
func build(root string, n, ignored int) {
	os.RemoveAll(root)
	rng := rand.New(rand.NewSource(int64(n)))
	words := strings.Fields("func return if else for range var const type struct interface package import nil err ctx string int byte map chan go defer select case switch")
	text := func(size int) []byte {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(words[rng.Intn(len(words))])
			if rng.Intn(8) == 0 {
				b.WriteString("\n\t")
			} else {
				b.WriteByte(' ')
			}
		}
		return []byte(b.String())
	}
	size := func() int {
		switch r := rng.Float64(); {
		case r < 0.6:
			return 500 + rng.Intn(3500)
		case r < 0.95:
			return 4000 + rng.Intn(12000)
		default:
			return 16000 + rng.Intn(100000)
		}
	}
	write := func(base string, count int, sz func() int) {
		dirs := []string{base}
		for i := 0; i < count; i++ {
			if i%20 == 0 {
				parent := dirs[rng.Intn(len(dirs))]
				if strings.Count(parent, "/") > 6 {
					parent = base
				}
				d := filepath.Join(parent, fmt.Sprintf("d%d", i/20))
				os.MkdirAll(d, 0o755)
				dirs = append(dirs, d)
			}
			d := dirs[len(dirs)-1]
			os.WriteFile(filepath.Join(d, fmt.Sprintf("f%d.go", i)), text(sz()), 0o644)
		}
	}
	os.MkdirAll(root, 0o755)
	write(filepath.Join(root, "src"), n, size)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n*.log\n.env*\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o600)
	sh(root, "init", "-q", "-b", "main")
	sh(root, "add", "-A")
	sh(root, "commit", "-q", "-m", "base")
	write(filepath.Join(root, "node_modules"), ignored, func() int { return 200 + rng.Intn(1500) })
}

type sample []time.Duration

func (s sample) pct(p float64) time.Duration {
	c := append(sample{}, s...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[int(float64(len(c)-1)*p)]
}

func (s sample) String() string {
	return fmt.Sprintf("p50 %-8s p95 %-8s max %-8s (n=%d)", ms(s.pct(0.5)), ms(s.pct(0.95)), ms(s.pct(1)), len(s))
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0fms", float64(d.Microseconds())/1000) }

func childCPU() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func tracked(root string) []string {
	out, _ := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
}

func modify(rng *rand.Rand, root string, files []string, k int) int {
	bytes := 0
	for i := 0; i < k; i++ {
		p := filepath.Join(root, files[rng.Intn(len(files))])
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		edit := fmt.Sprintf("// edit %d\n", rng.Int())
		f.WriteString(edit)
		f.Close()
		bytes += len(edit)
	}
	return bytes
}

// measureScoped compares full capture with capture scoped to the changed
// paths (what a filesystem watcher reports), and cross-checks that both give
// the same tree.
func measureScoped(label, root string) {
	base := filepath.Dir(root) + "/" + filepath.Base(root) + ".p1"
	os.MkdirAll(base, 0o755)
	files := tracked(root)
	rng := rand.New(rand.NewSource(7))
	fmt.Printf("=== %s (%d tracked files) — scoped vs full capture\n", label, len(files))
	s := newShadow(base, root, "scoped")
	_, st, err := s.CaptureTree()
	if err != nil {
		panic(err)
	}
	prevEmpty := st.EmptyDirs
	for _, k := range []int{1, 20, 200} {
		var full, sc sample
		mismatch := 0
		for i := 0; i < *reps; i++ {
			// full
			modify(rng, root, files, k)
			full = append(full, timeIt(func() { must2(s.CaptureTree()) }))
			// scoped: modify, also create one new file and one empty dir
			var changed []string
			for j := 0; j < k; j++ {
				f := files[rng.Intn(len(files))]
				fh, _ := os.OpenFile(filepath.Join(root, f), os.O_APPEND|os.O_WRONLY, 0)
				fh.WriteString(fmt.Sprintf("// s %d\n", rng.Int()))
				fh.Close()
				changed = append(changed, f)
			}
			nf := fmt.Sprintf("src/new-%d-%d.go", k, i)
			os.WriteFile(filepath.Join(root, nf), []byte("package x\n"), 0o644)
			changed = append(changed, nf)
			var tree string
			sc = append(sc, timeIt(func() {
				t, st, err := s.CaptureTreeScoped(changed, prevEmpty)
				if err != nil {
					panic(err)
				}
				tree, prevEmpty = t, st.EmptyDirs
			}))
			if ft, _, _ := s.CaptureTree(); ft != tree {
				mismatch++
			}
		}
		fmt.Printf("%3d files changed: full %s\n", k, full)
		fmt.Printf("                   scoped %s  cross-check mismatches=%d\n", sc, mismatch)
	}
	os.RemoveAll(s.GitDir)
	fmt.Println()
}

func newShadow(base, root, name string) *gitshadow.Shadow {
	gd := filepath.Join(base, name+".shadow")
	os.RemoveAll(gd)
	os.Remove(filepath.Join(base, name+".index"))
	if err := gitshadow.Init(gd); err != nil {
		panic(err)
	}
	return &gitshadow.Shadow{GitDir: gd, WorkTree: root, IndexFile: filepath.Join(base, name+".index"), Preserve: []string{".env*"}}
}

func dirSize(p string) int64 {
	var n int64
	filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func timeIt(f func()) time.Duration { t := time.Now(); f(); return time.Since(t) }

func measure(label, root string, growth bool) {
	base := filepath.Dir(root) + "/" + filepath.Base(root) + ".p1"
	os.MkdirAll(base, 0o755)
	files := tracked(root)
	rng := rand.New(rand.NewSource(42))
	fmt.Printf("=== %s (%d tracked files)\n", label, len(files))

	s := newShadow(base, root, "main")
	cpu0 := childCPU()
	cold := timeIt(func() { must2(s.CaptureTree()) })
	fmt.Printf("cold capture (empty shadow, no stat cache):   %s  [child CPU %s]\n", ms(cold), ms(childCPU()-cpu0))

	// Lost index, objects present (e.g. agent state reset).
	os.Remove(s.IndexFile)
	warmCold := timeIt(func() { must2(s.CaptureTree()) })
	fmt.Printf("re-capture after losing the capture index:     %s\n", ms(warmCold))

	var noop sample
	cpu0 = childCPU()
	for i := 0; i < *reps; i++ {
		noop = append(noop, timeIt(func() { must2(s.CaptureTree()) }))
	}
	fmt.Printf("no-op capture (nothing changed):               %s  [child CPU/op %s]\n", noop, ms((childCPU()-cpu0)/time.Duration(*reps)))

	for _, k := range []int{1, 20, 200} {
		var inc, cp sample
		for i := 0; i < *reps; i++ {
			modify(rng, root, files, k)
			inc = append(inc, timeIt(func() { must2(s.CaptureTree()) }))
			modify(rng, root, files, k)
			cp = append(cp, timeIt(func() { mustS(s.Checkpoint("refs/checkpoints/bench", "", i)) }))
		}
		fmt.Printf("incremental capture, %3d files changed:       %s\n", k, inc)
		fmt.Printf("full checkpoint,     %3d files changed:       %s\n", k, cp)
	}

	// Phase breakdown for a 20-file change.
	phases := map[string]sample{}
	order := []string{"add -A", "ls-files -o -i (preserve)", "ls-files -o (empty dirs)", "write-tree", "diff-index --cached HEAD"}
	for i := 0; i < *reps; i++ {
		modify(rng, root, files, 20)
		phases[order[0]] = append(phases[order[0]], timeIt(func() { mustB(s.Git(nil, "add", "-A", "--", ".")) }))
		phases[order[1]] = append(phases[order[1]], timeIt(func() { mustB(s.Git(nil, "ls-files", "-z", "-o", "-i", "--exclude-standard", "--directory")) }))
		phases[order[2]] = append(phases[order[2]], timeIt(func() { mustB(s.Git(nil, "ls-files", "-z", "-o", "--exclude-standard", "--directory")) }))
		phases[order[3]] = append(phases[order[3]], timeIt(func() { mustB(s.Git(nil, "write-tree")) }))
		phases[order[4]] = append(phases[order[4]], timeIt(func() { mustB(gitshadow.UserGit(root, nil, "diff-index", "--cached", "--raw", "-z", "HEAD")) }))
	}
	fmt.Println("phase breakdown (20 files changed):")
	for _, p := range order {
		fmt.Printf("    %-28s %s\n", p, phases[p])
	}

	// Config variants on the add -A step.
	for _, v := range []struct{ name, key, val string }{
		{"core.untrackedCache=false", "core.untrackedcache", "false"},
		{"index.version=2", "index.version", "2"},
	} {
		vs := newShadow(base, root, "variant")
		mustB(vs.Git(nil, "config", v.key, v.val))
		must2(vs.CaptureTree())
		var sm sample
		for i := 0; i < *reps; i++ {
			modify(rng, root, files, 20)
			sm = append(sm, timeIt(func() { must2(vs.CaptureTree()) }))
		}
		fmt.Printf("variant %-27s 20 files: %s\n", v.name, sm)
		os.RemoveAll(vs.GitDir)
	}

	// Apply latency on a replica of the same tree (20-file change).
	measureApply(base, root, files, rng)

	if growth {
		measureGrowth(base, root, files, rng)
	}
	os.RemoveAll(s.GitDir)
	fmt.Println()
}

func measureApply(base, root string, files []string, rng *rand.Rand) {
	replica := filepath.Join(base, "replica")
	os.RemoveAll(replica)
	if out, err := exec.Command("git", "clone", "-q", root, replica).CombinedOutput(); err != nil {
		panic(string(out))
	}
	w := newShadow(base, root, "apply-writer")
	r := newShadow(base, replica, "apply-reader")
	ref := "refs/checkpoints/current"
	prev, err := w.Checkpoint(ref, "", 0)
	if err != nil {
		panic(err)
	}
	if err := gitshadow.Transfer(w, r, ref); err != nil {
		panic(err)
	}
	seed := timeIt(func() {
		if err := r.Seed(prev); err != nil {
			panic(err)
		}
	})
	var ap, xfer, fetchXfer sample
	var packBytes int
	n := *reps
	if n > 15 {
		n = 15
	}
	// Stock git fetch for comparison (3 samples; it resends the whole tree).
	for i := 0; i < 3; i++ {
		modify(rng, root, files, 20)
		cp, err := w.Checkpoint(ref, prev, i)
		if err != nil {
			panic(err)
		}
		fetchXfer = append(fetchXfer, timeIt(func() {
			if err := gitshadow.Transfer(w, r, ref); err != nil {
				panic(err)
			}
		}))
		if err := r.Apply(prev, cp, gitshadow.ApplyOptions{}); err != nil {
			panic(err)
		}
		prev = cp
	}
	for i := 1; i <= n; i++ {
		modify(rng, root, files, 20)
		cp, err := w.Checkpoint(ref, prev, i)
		if err != nil {
			panic(err)
		}
		xfer = append(xfer, timeIt(func() {
			b, err := gitshadow.TransferPack(w, r, ref, cp, prev)
			if err != nil {
				panic(err)
			}
			packBytes += b
		}))
		ap = append(ap, timeIt(func() {
			if err := r.Apply(prev, cp, gitshadow.ApplyOptions{}); err != nil {
				panic(err)
			}
		}))
		prev = cp
	}
	fmt.Printf("replica seed (clone → first checkpoint):       %s\n", ms(seed))
	fmt.Printf("transfer, stock git fetch, 20 files:           %s  (parentless commits: whole tree resent)\n", fetchXfer)
	fmt.Printf("transfer, thin pack vs base tree, 20 files:    %s  avg pack %d bytes\n", xfer, packBytes/n)
	fmt.Printf("apply incl. verify + post-verify, 20 files:    %s\n", ap)
	os.RemoveAll(replica)
	os.RemoveAll(w.GitDir)
	os.RemoveAll(r.GitDir)
}

func measureGrowth(base, root string, files []string, rng *rand.Rand) {
	g := newShadow(base, root, "growth")
	prev, err := g.Checkpoint("refs/checkpoints/0", "", 0)
	if err != nil {
		panic(err)
	}
	mustB(g.Git(nil, "gc", "-q"))
	before := dirSize(g.GitDir)
	changed := 0
	t := time.Now()
	for i := 1; i <= *growthN; i++ {
		changed += modify(rng, root, files, 20)
		cp, err := g.Checkpoint(fmt.Sprintf("refs/checkpoints/%d", i), prev, i)
		if err != nil {
			panic(err)
		}
		prev = cp
	}
	elapsed := time.Since(t)
	loose := dirSize(g.GitDir) - before
	gcT := timeIt(func() { mustB(g.Git(nil, "gc", "-q")) })
	after := dirSize(g.GitDir) - before
	fmt.Printf("growth: %d checkpoints × 20 appended edits (%d bytes of edits total) in %s\n", *growthN, changed, elapsed.Round(time.Second))
	fmt.Printf("    shadow growth before gc: %.1f MiB (%.1f KiB/checkpoint)\n", float64(loose)/(1<<20), float64(loose)/1024/float64(*growthN))
	fmt.Printf("    shadow growth after gc:  %.1f MiB (%.1f KiB/checkpoint), ratio to edit bytes %.1f×, gc took %s\n",
		float64(after)/(1<<20), float64(after)/1024/float64(*growthN), float64(after)/float64(changed), ms(gcT))
	os.RemoveAll(g.GitDir)
}

func must2(_ string, _ gitshadow.CaptureStats, err error) {
	if err != nil {
		panic(err)
	}
}
func mustS(_ string, err error) {
	if err != nil {
		panic(err)
	}
}
func mustB(_ []byte, err error) {
	if err != nil {
		panic(err)
	}
}
