package gitshadow

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultMaxFileSize is the class X size limit of §6.6: larger files are
// not captured, and are listed in the checkpoint's meta.excluded.oversize.
const DefaultMaxFileSize = 50 << 20

// PolicyFile is the project's sync policy, committed with the project.
const PolicyFile = ".armageddon/sync.yaml"

// maxFileSize resolves the size limit: the Shadow's override, else the
// policy file's max_file_size as of the previous capture, else the
// default. The policy is read from the shadow's index, not from disk, so
// it is read as the capturing user and a symlink can never point it at
// another file; a policy change applies from the capture after the one
// that picked it up.
func (s *Shadow) maxFileSize() int64 {
	if s.MaxFileSize != 0 {
		return s.MaxFileSize
	}
	b, err := s.Git(nil, "cat-file", "blob", ":"+PolicyFile)
	if err != nil {
		return DefaultMaxFileSize
	}
	if n, ok := ParseMaxFileSize(string(b)); ok {
		return n
	}
	return DefaultMaxFileSize
}

// ParseMaxFileSize reads sync.max_file_size from a sync.yaml. Only that
// key is understood; values are a byte count with an optional unit (B,
// KB, MB, GB; binary multiples), or 0 / "none" for no limit.
func ParseMaxFileSize(yaml string) (int64, bool) {
	inSync := false
	sc := bufio.NewScanner(strings.NewReader(yaml))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(val), `"'`)
		if !indented {
			inSync = key == "sync"
			continue
		}
		if inSync && key == "max_file_size" {
			return parseSize(val)
		}
	}
	return 0, false
}

func parseSize(v string) (int64, bool) {
	v = strings.ToUpper(strings.ReplaceAll(v, " ", ""))
	if v == "0" || v == "NONE" {
		return -1, true
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, u.suffix) {
			v, mult = strings.TrimSuffix(v, u.suffix), u.m
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n * mult, true
}

// sizePolicy is the outcome of the pre-capture size scan.
type sizePolicy struct {
	active  bool
	limit   int64
	big     []string        // over the limit: excluded from this capture
	scanned map[string]bool // every path the scan looked at
	changed []string        // every changed path except big: what to add
}

// sizeScan lists the changed or new files (modified per the index's stat
// cache, or untracked and not ignored) larger than the size limit, so
// capture can leave them out without reading them.
func (s *Shadow) sizeScan() (sizePolicy, error) {
	// The policy, the changed tracked files and the untracked files come
	// from three Git processes run at once: this scan runs before every
	// capture, and each process mostly waits on reading the index or the
	// directory tree (M9.3: about 150 ms less at 50k files than one
	// `ls-files -m -o`, which stats every entry on one thread).
	if s.MaxFileSize < 0 {
		return sizePolicy{limit: -1, scanned: map[string]bool{}}, nil
	}
	var (
		wg              sync.WaitGroup
		limit           int64
		mod, untracked  []byte
		errMod, errUntr error
	)
	wg.Add(3)
	go func() { defer wg.Done(); limit = s.maxFileSize() }()
	go func() { defer wg.Done(); mod, errMod = s.Git(nil, "diff-files", "--name-only", "-z") }()
	go func() {
		defer wg.Done()
		untracked, errUntr = s.Git(nil, "ls-files", "-z", "-o", "--exclude-standard")
	}()
	wg.Wait()
	pol := sizePolicy{limit: limit, scanned: map[string]bool{}}
	if pol.limit < 0 {
		return pol, nil
	}
	pol.active = true
	if errMod != nil {
		return pol, errMod
	}
	if errUntr != nil {
		return pol, errUntr
	}
	var paths []string
	for _, p := range append(splitZ(mod), splitZ(untracked)...) {
		if pol.scanned[p] { // a path can be listed twice
			continue
		}
		pol.scanned[p] = true
		if s.sizeExempt[p] {
			pol.changed = append(pol.changed, p)
		} else {
			paths = append(paths, p)
		}
	}
	big := map[string]bool{}
	err := s.regularSizes(paths, func(p string, size int64) {
		if size > pol.limit {
			pol.big = append(pol.big, p)
			big[p] = true
		}
	})
	for _, p := range paths {
		if !big[p] {
			pol.changed = append(pol.changed, p)
		}
	}
	sort.Strings(pol.big)
	return pol, err
}

// dropLateOversize undoes the add of files that appeared or grew past the
// limit between the scan and `git add` (their paths are in add -v's output
// but were not scanned): each goes back to its entry in base, the previous
// capture's tree, or out of the index if it is new. The next scan then
// sees it, and leaves it out without reading it.
func (s *Shadow) dropLateOversize(addOut []byte, pol sizePolicy, base string) ([]string, error) {
	var cand []string
	for _, line := range strings.Split(string(addOut), "\n") {
		if p, ok := strings.CutPrefix(line, "add '"); ok && strings.HasSuffix(p, "'") {
			if p = strings.TrimSuffix(p, "'"); !pol.scanned[p] && !s.sizeExempt[p] {
				cand = append(cand, p)
			}
		}
	}
	if len(cand) == 0 {
		return nil, nil
	}
	var q strings.Builder
	for _, p := range cand {
		fmt.Fprintf(&q, ":%s\n", p)
	}
	out, err := s.Git([]byte(q.String()), "cat-file", "--batch-check=%(objectsize)")
	if err != nil {
		return nil, err
	}
	sizes := strings.Split(strings.TrimSpace(string(out)), "\n")
	var late []string
	for i, p := range cand {
		if i >= len(sizes) {
			break
		}
		if n, err := strconv.ParseInt(sizes[i], 10, 64); err != nil || n <= pol.limit {
			continue
		}
		late = append(late, p)
		prev, _ := s.Git(nil, "--literal-pathspecs", "ls-tree", "-z", base, "--", p)
		if meta, _, ok := strings.Cut(strings.TrimSuffix(string(prev), "\x00"), "\t"); ok && base != "" {
			f := strings.Fields(meta) // mode type oid
			if _, err := s.Git(nil, "update-index", "--cacheinfo", f[0]+","+f[2]+","+p); err != nil {
				return nil, err
			}
		} else if _, err := s.Git(nil, "update-index", "--force-remove", "--", p); err != nil {
			return nil, err
		}
	}
	sort.Strings(late)
	return late, nil
}

// regularSizes reports the size of each path (relative to the working
// tree) that is a regular file. Without Prepare the files are stat-ed
// in-process; with it (the server seat, whose tree only the workspace
// user can read) through stat(1) run as that user.
func (s *Shadow) regularSizes(paths []string, fn func(string, int64)) error {
	if s.Prepare == nil {
		for _, p := range paths {
			if fi, err := os.Lstat(filepath.Join(s.WorkTree, filepath.FromSlash(p))); err == nil && fi.Mode().IsRegular() {
				fn(p, fi.Size())
			}
		}
		return nil
	}
	for len(paths) > 0 {
		n := min(len(paths), 1000)
		chunk := paths[:n]
		paths = paths[n:]
		cmd := exec.Command("stat", append([]string{"--printf=%s\t%F\t%n\\0", "--"}, chunk...)...)
		cmd.Dir = s.WorkTree
		s.Prepare(cmd)
		cmd.Env = append(cmd.Env, "LC_ALL=C") // %F unlocalised
		// A file removed meanwhile makes stat fail; the others still print.
		out, _ := cmd.Output()
		for _, rec := range splitZ(out) {
			f := strings.SplitN(rec, "\t", 3)
			if len(f) != 3 || f[1] != "regular file" && f[1] != "regular empty file" {
				continue
			}
			if size, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				fn(f[2], size)
			}
		}
	}
	return nil
}

// HumanSize renders a byte count the way the policy file writes it.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKB", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}
