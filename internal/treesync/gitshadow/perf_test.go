package gitshadow

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestPerfReference is the M9.3 performance pass against the P1 targets
// (architecture §14, P1): a reference repository of 50k tracked files with
// an ignored node_modules of 100k files, then rounds of 20 changed files
// captured on a writer, transferred and applied on a follower.
//
//	P1 targets: incremental capture with ≤ 20 changed files p95 < 300 ms at
//	50k files; cold capture < 15 s at 50k files.
//
// It runs only with ARMAGEDDON_PERF=1 (the nightly workflow), takes a few
// minutes, and writes its numbers as JSON to ARMAGEDDON_PERF_OUT if set.
// ARMAGEDDON_PERF_FILES and ARMAGEDDON_PERF_IGNORED change the sizes;
// ARMAGEDDON_PERF_NOLIMIT=1 turns the size policy (F17) off for comparison.
func TestPerfReference(t *testing.T) {
	if os.Getenv("ARMAGEDDON_PERF") != "1" {
		t.Skip("set ARMAGEDDON_PERF=1 to run the performance pass")
	}
	files := envInt("ARMAGEDDON_PERF_FILES", 50000)
	ignored := envInt("ARMAGEDDON_PERF_IGNORED", 100000)
	const rounds, changed = 50, 20

	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	rng := rand.New(rand.NewSource(1))
	start := time.Now()
	paths := generateTree(t, a, files, ignored, rng)
	git(t, a, "init", "-q", "-b", "main")
	git(t, a, "add", "-A")
	git(t, a, "commit", "-q", "-m", "reference")
	git(t, base, "clone", "-q", a, b)
	t.Logf("generated %d tracked + %d ignored files in %s", files, ignored, time.Since(start).Round(time.Millisecond))

	A, B := shadow(t, base, "A", a), shadow(t, base, "B", b)
	if os.Getenv("ARMAGEDDON_PERF_NOLIMIT") == "1" { // measure without the size policy's scan
		A.MaxFileSize, B.MaxFileSize = -1, -1
	}
	ref := "refs/checkpoints/current"
	t0 := time.Now()
	prev, err := A.Checkpoint(ref, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cold := time.Since(t0)
	t0 = time.Now()
	if _, err := TransferPack(A, B, ref, prev, ""); err != nil {
		t.Fatal(err)
	}
	if err := B.Seed(prev); err != nil {
		t.Fatal(err)
	}
	seed := time.Since(t0)

	var capture, transfer, apply []time.Duration
	var bytes []int
	for i := 1; i <= rounds; i++ {
		for _, j := range rng.Perm(len(paths))[:changed] {
			if err := os.WriteFile(filepath.Join(a, paths[j]), content(rng, fmt.Sprintf("round %d\n", i)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t0 := time.Now()
		cp, err := A.Checkpoint(ref, prev, i)
		if err != nil {
			t.Fatal(err)
		}
		capture = append(capture, time.Since(t0))
		t0 = time.Now()
		n, err := TransferPack(A, B, ref, cp, prev)
		if err != nil {
			t.Fatal(err)
		}
		transfer, bytes = append(transfer, time.Since(t0)), append(bytes, n)
		t0 = time.Now()
		if err := B.Apply(prev, cp, ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		apply = append(apply, time.Since(t0))
		prev = cp
	}

	res := map[string]any{
		"files": files, "ignored": ignored, "rounds": rounds, "changed_per_round": changed,
		"cold_capture_ms":       cold.Milliseconds(),
		"follower_seed_ms":      seed.Milliseconds(),
		"capture_p50_ms":        pct(capture, 50),
		"capture_p95_ms":        pct(capture, 95),
		"transfer_p95_ms":       pct(transfer, 95),
		"apply_p50_ms":          pct(apply, 50),
		"apply_p95_ms":          pct(apply, 95),
		"end_to_end_p95_ms":     pct(sum(capture, transfer, apply), 95),
		"pack_bytes_p95":        pctInt(bytes, 95),
		"target_capture_p95_ms": 300,
		"target_cold_ms":        15000,
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("results:\n%s", out)
	if p := os.Getenv("ARMAGEDDON_PERF_OUT"); p != "" {
		if err := os.WriteFile(p, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if files == 50000 {
		if p := pct(capture, 95); p >= 300 {
			t.Errorf("incremental capture p95 %d ms, target < 300 ms (P1)", p)
		}
		if cold >= 15*time.Second {
			t.Errorf("cold capture %s, target < 15 s (P1)", cold)
		}
	}
}

// generateTree writes n tracked files spread over nested directories
// (sizes 200 B to 8 KiB) and ignored files under node_modules.
func generateTree(t *testing.T, root string, n, ignored int, rng *rand.Rand) []string {
	t.Helper()
	write := func(rel string, data []byte) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", []byte("node_modules/\n.env*\n"))
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("src/m%02d/p%03d/f%05d.go", i%40, (i/40)%60, i)
		write(rel, content(rng, rel))
		paths = append(paths, rel)
	}
	small := []byte("module.exports = {}\n")
	for i := 0; i < ignored; i++ {
		write(fmt.Sprintf("node_modules/pkg%04d/lib/f%05d.js", i%3000, i), small)
	}
	return paths
}

func content(rng *rand.Rand, seed string) []byte {
	n := 200 + rng.Intn(8000)
	b := make([]byte, n)
	copy(b, seed)
	for i := len(seed); i < n; i++ {
		b[i] = "abcdefghijklmnopqrstuvwxyz \n"[rng.Intn(28)]
	}
	return b
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func pct(ds []time.Duration, p int) int64 {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)*p+99)/100-1].Milliseconds()
}

func pctInt(xs []int, p int) int {
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[(len(s)*p+99)/100-1]
}

func sum(parts ...[]time.Duration) []time.Duration {
	out := make([]time.Duration, len(parts[0]))
	for _, p := range parts {
		for i, d := range p {
			out[i] += d
		}
	}
	return out
}
