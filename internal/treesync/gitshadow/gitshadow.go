// Package gitshadow implements the git-shadow TreeSync strategy (contract
// §6, with Phase 0 changes P-1…P-5). Ported from prototypes/internal/gitshadow.
//
// A Shadow captures a working tree into a separate bare repository using a
// persistent private index, so the user's own .git is never touched. Apply
// transforms a destination working tree from one checkpoint to another with
// per-file verification (invariant I7).
package gitshadow

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// ErrDiverged reports that the destination is not in the state the apply
// expected to start from. The caller must quarantine before retrying (I4).
var ErrDiverged = errors.New("destination diverged from expected base")

// BuiltinExcludes are class C defaults from §6.6.
var BuiltinExcludes = []string{
	"node_modules/", "vendor/", ".next/", "dist/", "build/", "target/",
	"__pycache__/", ".venv/",
}

type Shadow struct {
	GitDir    string   // bare shadow repository
	WorkTree  string   // working tree being captured or applied to
	IndexFile string   // persistent capture index (stat cache)
	Preserve  []string // filepath.Match patterns on the basename, e.g. ".env*"

	// Prepare, when set, configures every git process (OS user, base
	// environment). The server uses it to run as the workspace user (§2.5).
	Prepare func(*exec.Cmd)

	// Displaced records directories moved out of the working tree because
	// they held only local ignored files and blocked a put (I4: moved, not
	// deleted). They live under GitDir/displaced/.
	Displaced []string
}

// gitEnv is the fixed environment every shadow git invocation gets on top
// of its base environment.
func gitEnv() []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=armageddon", "GIT_AUTHOR_EMAIL=armageddon@localhost",
		"GIT_COMMITTER_NAME=armageddon", "GIT_COMMITTER_EMAIL=armageddon@localhost",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}
}

func (s *Shadow) env() []string {
	return append(gitEnv(),
		"GIT_DIR="+s.GitDir, "GIT_WORK_TREE="+s.WorkTree, "GIT_INDEX_FILE="+s.IndexFile)
}

// Git runs git against the shadow with the working tree as cwd.
func (s *Shadow) Git(stdin []byte, args ...string) ([]byte, error) {
	return run(s.WorkTree, s.env(), stdin, s.Prepare, args...)
}

// UserGit runs git against the user's own repository in the working tree.
func (s *Shadow) UserGit(stdin []byte, args ...string) ([]byte, error) {
	return run(s.WorkTree, gitEnv(), stdin, s.Prepare, args...)
}

// baseEnv is the server's environment without GIT_* variables, used when no
// Prepare hook supplies one.
func baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

func command(dir string, env []string, prepare func(*exec.Cmd), args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if prepare != nil {
		prepare(cmd)
		cmd.Env = append(cmd.Env, env...)
	} else {
		cmd.Env = append(baseEnv(), env...)
	}
	return cmd
}

func run(dir string, env []string, stdin []byte, prepare func(*exec.Cmd), args ...string) ([]byte, error) {
	cmd := command(dir, env, prepare, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

// Init creates a shadow repository with byte-exact capture settings (§6.3).
func Init(gitDir string, prepare func(*exec.Cmd), extraExcludes ...string) error {
	if _, err := run("/", gitEnv(), nil, prepare, "init", "-q", "--bare", gitDir); err != nil {
		return err
	}
	for _, kv := range [][2]string{
		{"core.autocrlf", "false"}, {"core.safecrlf", "false"}, {"core.symlinks", "true"},
		{"core.filemode", "true"}, {"core.ignorecase", "false"}, {"core.precomposeunicode", "false"},
		{"core.quotepath", "false"}, {"gc.auto", "0"}, {"core.untrackedcache", "true"},
		{"index.version", "4"},
	} {
		if _, err := run("/", gitEnv(), nil, prepare, "--git-dir="+gitDir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	info := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	attrs := "* -text -eol -filter -ident -working-tree-encoding -diff -merge\n"
	if err := os.WriteFile(filepath.Join(info, "attributes"), []byte(attrs), 0o644); err != nil {
		return err
	}
	ex := strings.Join(append(append([]string{}, BuiltinExcludes...), extraExcludes...), "\n") + "\n"
	return os.WriteFile(filepath.Join(info, "exclude"), []byte(ex), 0o644)
}

// CaptureStats breaks capture time into phases for P1.
type CaptureStats struct {
	PreservedAdded int
	EmptyDirs      []string
}

// CaptureTree captures the working tree into the shadow and returns the tree
// oid for /worktree plus the list of non-ignored empty directories.
func (s *Shadow) CaptureTree() (string, CaptureStats, error) {
	var st CaptureStats
	if _, err := s.Git(nil, "add", "-A", "--", "."); err != nil {
		return "", st, err
	}
	// Preserve: ignored-but-wanted files (.env*). --directory collapses
	// ignored directories so node_modules is not descended into.
	if len(s.Preserve) > 0 {
		out, err := s.Git(nil, "ls-files", "-z", "-o", "-i", "--exclude-standard", "--directory")
		if err != nil {
			return "", st, err
		}
		var add []string
		for _, p := range splitZ(out) {
			if strings.HasSuffix(p, "/") {
				continue
			}
			base := filepath.Base(p)
			for _, pat := range s.Preserve {
				if ok, _ := filepath.Match(pat, base); ok {
					add = append(add, p)
					break
				}
			}
		}
		if len(add) > 0 {
			st.PreservedAdded = len(add)
			if _, err := s.Git([]byte(strings.Join(add, "\x00")), "--literal-pathspecs", "add", "-f",
				"--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
				return "", st, err
			}
		}
	}
	// After add -A the only remaining untracked, non-ignored entries are
	// empty directories.
	out, err := s.Git(nil, "ls-files", "-z", "-o", "--exclude-standard", "--directory")
	if err != nil {
		return "", st, err
	}
	// Git collapses a directory holding only empty directories into one
	// entry, so expand each reported directory to its empty leaves.
	for _, p := range splitZ(out) {
		if strings.HasSuffix(p, "/") {
			st.EmptyDirs = append(st.EmptyDirs, emptyLeaves(s.WorkTree, strings.TrimSuffix(p, "/"))...)
		}
	}
	sort.Strings(st.EmptyDirs)
	tree, err := s.Git(nil, "write-tree")
	if err != nil {
		return "", st, err
	}
	return strings.TrimSpace(string(tree)), st, nil
}

// CaptureTreeScoped is CaptureTree limited to the paths a filesystem
// watcher reported as changed (files or directories, relative, slash form).
// prevEmpty is the previous checkpoint's empty-directory list; entries that
// are still empty are carried over. Correctness depends on the watcher
// reporting every change (prototype P3); callers fall back to CaptureTree on
// any error or watcher overflow.
func (s *Shadow) CaptureTreeScoped(changed []string, prevEmpty []string) (string, CaptureStats, error) {
	var st CaptureStats
	if len(changed) == 0 {
		tree, err := s.Git(nil, "write-tree")
		st.EmptyDirs = prevEmpty
		return strings.TrimSpace(string(tree)), st, err
	}
	nul := []byte(strings.Join(changed, "\x00"))
	if _, err := s.Git(nul, "--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return "", st, err
	}
	dirSet := map[string]bool{}
	for _, c := range changed {
		dirSet[filepath.ToSlash(filepath.Dir(c))] = true
	}
	var dirs []string
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	if len(s.Preserve) > 0 {
		out, err := s.Git(nil, append([]string{"--literal-pathspecs", "ls-files", "-z", "-o", "-i", "--exclude-standard", "--directory", "--"}, dirs...)...)
		if err != nil {
			return "", st, err
		}
		var add []string
		for _, p := range splitZ(out) {
			if strings.HasSuffix(p, "/") {
				continue
			}
			for _, pat := range s.Preserve {
				if ok, _ := filepath.Match(pat, filepath.Base(p)); ok {
					add = append(add, p)
					break
				}
			}
		}
		if len(add) > 0 {
			if _, err := s.Git([]byte(strings.Join(add, "\x00")), "--literal-pathspecs", "add", "-f",
				"--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
				return "", st, err
			}
		}
	}
	empty := map[string]bool{}
	for _, d := range prevEmpty {
		if ents, err := os.ReadDir(filepath.Join(s.WorkTree, filepath.FromSlash(d))); err == nil && len(ents) == 0 {
			empty[d] = true
		}
	}
	out, err := s.Git(nil, append([]string{"--literal-pathspecs", "ls-files", "-z", "-o", "--exclude-standard", "--directory", "--"}, dirs...)...)
	if err != nil {
		return "", st, err
	}
	for _, p := range splitZ(out) {
		if strings.HasSuffix(p, "/") {
			for _, l := range emptyLeaves(s.WorkTree, strings.TrimSuffix(p, "/")) {
				empty[l] = true
			}
		}
	}
	for d := range empty {
		st.EmptyDirs = append(st.EmptyDirs, d)
	}
	sort.Strings(st.EmptyDirs)
	tree, err := s.Git(nil, "write-tree")
	if err != nil {
		return "", st, err
	}
	return strings.TrimSpace(string(tree)), st, nil
}

func emptyLeaves(root, rel string) []string {
	var out []string
	filepath.WalkDir(filepath.Join(root, filepath.FromSlash(rel)), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		ents, err := os.ReadDir(p)
		if err == nil && len(ents) == 0 {
			r, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	return out
}

// StagedEntry is one line of the index manifest: the staged state expressed
// as a delta against HEAD (stage 0) plus unmerged entries (stages 1-3).
type StagedEntry struct {
	Mode  string `json:"mode"` // "000000" means "not in index"
	Oid   string `json:"oid"`
	Stage int    `json:"stage"`
	Path  string `json:"path"`
}

// CaptureIndex reads the user's index as a delta against HEAD. Cost is
// proportional to staged changes, not repository size.
func (s *Shadow) CaptureIndex() ([]StagedEntry, error) {
	var out []StagedEntry
	raw, err := s.UserGit(nil, "diff-index", "--cached", "--raw", "-z", "--no-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	f := splitZ(raw)
	unmergedPaths := map[string]bool{}
	for i := 0; i+1 < len(f); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(f[i], ":"))
		path := f[i+1]
		status := meta[4]
		if status == "U" {
			unmergedPaths[path] = true
			continue
		}
		out = append(out, StagedEntry{Mode: meta[1], Oid: meta[3], Stage: 0, Path: path})
	}
	if len(unmergedPaths) > 0 {
		u, err := s.UserGit(nil, "ls-files", "-u", "-z")
		if err != nil {
			return nil, err
		}
		for _, line := range splitZ(u) {
			tab := strings.IndexByte(line, '\t')
			fs := strings.Fields(line[:tab])
			var stage int
			fmt.Sscanf(fs[2], "%d", &stage)
			out = append(out, StagedEntry{Mode: fs[0], Oid: fs[1], Stage: stage, Path: line[tab+1:]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Stage < out[j].Stage
	})
	return out, nil
}

type Meta struct {
	Format    int           `json:"format"`
	Parent    string        `json:"parent"`
	HeadRef   string        `json:"head_ref"`
	HeadOid   string        `json:"head_oid"`
	EmptyDirs []string      `json:"empty_dirs"`
	Staged    []StagedEntry `json:"staged"`
	Seq       int           `json:"seq"`
}

// Checkpoint captures tree + index and writes a checkpoint commit under ref.
// Staged blobs that differ from the worktree are copied into the shadow and
// made reachable through a /staged subtree so fetch transfers them.
func (s *Shadow) Checkpoint(ref string, parent string, seq int) (string, error) {
	st, err := s.CaptureState()
	if err != nil {
		return "", err
	}
	return s.CommitState(st, ref, parent, seq)
}

// State is a captured working state that has not been written as a
// checkpoint commit yet.
type State struct {
	Tree      string
	EmptyDirs []string
	Staged    []StagedEntry
	HeadRef   string
	HeadOid   string
}

// Same reports whether two states describe the same working state, so the
// caller can skip creating a checkpoint (§6.3: no-op captures create none).
func (a State) Same(b State) bool {
	if a.Tree != b.Tree || a.HeadRef != b.HeadRef || a.HeadOid != b.HeadOid ||
		len(a.EmptyDirs) != len(b.EmptyDirs) || len(a.Staged) != len(b.Staged) {
		return false
	}
	for i := range a.EmptyDirs {
		if a.EmptyDirs[i] != b.EmptyDirs[i] {
			return false
		}
	}
	for i := range a.Staged {
		if a.Staged[i] != b.Staged[i] {
			return false
		}
	}
	return true
}

// StateOf reads the State recorded in an existing checkpoint.
func (s *Shadow) StateOf(cp string) (State, error) {
	if cp == "" {
		return State{}, nil
	}
	m, err := s.ReadMeta(cp)
	if err != nil {
		return State{}, err
	}
	tree, err := s.worktreeOf(cp)
	if err != nil {
		return State{}, err
	}
	return State{Tree: tree, EmptyDirs: m.EmptyDirs, Staged: m.Staged, HeadRef: m.HeadRef, HeadOid: m.HeadOid}, nil
}

// CaptureState captures the working tree, the index delta and HEAD.
func (s *Shadow) CaptureState() (State, error) {
	wt, st, err := s.CaptureTree()
	if err != nil {
		return State{}, err
	}
	staged, err := s.CaptureIndex()
	if err != nil {
		return State{}, err
	}
	headOid, _ := s.UserGit(nil, "rev-parse", "-q", "--verify", "HEAD")
	headRef, _ := s.UserGit(nil, "symbolic-ref", "-q", "HEAD")
	out := State{Tree: wt, EmptyDirs: st.EmptyDirs, Staged: staged,
		HeadRef: strings.TrimSpace(string(headRef)), HeadOid: strings.TrimSpace(string(headOid))}
	if out.EmptyDirs == nil {
		out.EmptyDirs = []string{}
	}
	if out.Staged == nil {
		out.Staged = []StagedEntry{}
	}
	return out, nil
}

// CommitState writes a captured State as a checkpoint commit under ref.
func (s *Shadow) CommitState(state State, ref, parent string, seq int) (string, error) {
	wt, staged := state.Tree, state.Staged
	meta := Meta{Format: 1, Parent: parent, HeadRef: state.HeadRef, HeadOid: state.HeadOid,
		EmptyDirs: state.EmptyDirs, Staged: staged, Seq: seq}
	if meta.EmptyDirs == nil {
		meta.EmptyDirs = []string{}
	}
	if meta.Staged == nil {
		meta.Staged = []StagedEntry{}
	}
	// Copy staged blobs into the shadow (most are already there).
	var oids []string
	for _, e := range staged {
		if e.Mode != "000000" && e.Mode != "160000" {
			oids = append(oids, e.Oid)
		}
	}
	if err := copyObjects(oids, gitEnv(), s.env(), s.WorkTree, s.Prepare); err != nil {
		return "", fmt.Errorf("copy staged blobs: %w", err)
	}
	mj, _ := json.Marshal(meta)
	metaOid, err := s.Git(mj, "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		return "", err
	}
	var mk strings.Builder
	fmt.Fprintf(&mk, "040000 tree %s\tworktree\n", wt)
	fmt.Fprintf(&mk, "100644 blob %s\tmeta.json\n", strings.TrimSpace(string(metaOid)))
	if len(oids) > 0 {
		var sk strings.Builder
		seen := map[string]bool{}
		for _, o := range oids {
			if !seen[o] {
				seen[o] = true
				fmt.Fprintf(&sk, "100644 blob %s\t%s\n", o, o)
			}
		}
		stTree, err := s.Git([]byte(sk.String()), "mktree")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&mk, "040000 tree %s\tstaged\n", strings.TrimSpace(string(stTree)))
	}
	cpTree, err := s.Git([]byte(mk.String()), "mktree")
	if err != nil {
		return "", err
	}
	commit, err := s.Git([]byte(fmt.Sprintf("checkpoint %d\n", seq)), "commit-tree", strings.TrimSpace(string(cpTree)))
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(commit))
	if _, err := s.Git(nil, "update-ref", ref, oid); err != nil {
		return "", err
	}
	return oid, nil
}

// copyObjects moves the given objects from one repository to another with
// pack-objects | index-pack, skipping those already present at the target.
func copyObjects(oids []string, srcEnv, dstEnv []string, dir string, prepare func(*exec.Cmd)) error {
	if len(oids) == 0 {
		return nil
	}
	check, err := run(dir, dstEnv, []byte(strings.Join(oids, "\n")+"\n"), prepare, "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(string(check)), "\n") {
		if strings.HasSuffix(line, " missing") {
			missing = append(missing, strings.Fields(line)[0])
		}
	}
	if len(missing) == 0 {
		return nil
	}
	pack, err := run(dir, srcEnv, []byte(strings.Join(missing, "\n")+"\n"), prepare, "pack-objects", "--stdout", "-q")
	if err != nil {
		return err
	}
	_, err = run(dir, dstEnv, pack, prepare, "index-pack", "--stdin", "--fix-thin")
	return err
}

// ReadMeta loads a checkpoint's metadata.
func (s *Shadow) ReadMeta(cp string) (Meta, error) {
	var m Meta
	if cp == "" {
		return m, nil
	}
	b, err := s.Git(nil, "cat-file", "blob", cp+":meta.json")
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func (s *Shadow) worktreeOf(cp string) (string, error) {
	if cp == "" {
		return EmptyTree, nil
	}
	out, err := s.Git(nil, "rev-parse", cp+":worktree")
	return strings.TrimSpace(string(out)), err
}

// ---------------------------------------------------------------------------
// Apply

type change struct {
	oldMode, newMode, oldOid, newOid, status, path string
}

// ApplyOptions control failure injection for P2.
type ApplyOptions struct {
	Resume     bool // a previous apply of the same from→to was interrupted
	CrashAfter int  // >0: stop after this many file operations (simulated crash)
}

var ErrSimulatedCrash = errors.New("simulated crash")

// Apply transforms s.WorkTree from checkpoint `from` to `to`. Both must exist
// in s.GitDir. The user's index is updated from the staged manifests.
func (s *Shadow) Apply(from, to string, opt ApplyOptions) error {
	fromWT, err := s.worktreeOf(from)
	if err != nil {
		return err
	}
	toWT, err := s.worktreeOf(to)
	if err != nil {
		return err
	}
	fromMeta, err := s.ReadMeta(from)
	if err != nil {
		return err
	}
	toMeta, err := s.ReadMeta(to)
	if err != nil {
		return err
	}
	if !opt.Resume {
		cur, _, err := s.CaptureTree()
		if err != nil {
			return err
		}
		if cur != fromWT {
			return ErrDiverged
		}
	}
	return s.applyTrees(fromWT, fromMeta, toWT, toMeta, opt)
}

// Seed brings a newly created replica (a fresh clone) to checkpoint `to`.
// A fresh clone is not byte-identical to the writer's working tree when
// attributes normalise line endings or filters run on checkout, so the
// replica's own capture is used as the base instead of assuming equality.
func (s *Shadow) Seed(to string) error {
	cur, st, err := s.CaptureTree()
	if err != nil {
		return err
	}
	toWT, err := s.worktreeOf(to)
	if err != nil {
		return err
	}
	toMeta, err := s.ReadMeta(to)
	if err != nil {
		return err
	}
	return s.applyTrees(cur, Meta{EmptyDirs: st.EmptyDirs}, toWT, toMeta, ApplyOptions{})
}

func (s *Shadow) applyTrees(fromWT string, fromMeta Meta, toWT string, toMeta Meta, opt ApplyOptions) error {
	raw, err := s.Git(nil, "diff-tree", "-r", "-z", "--no-renames", fromWT, toWT)
	if err != nil {
		return err
	}
	var changes []change
	f := splitZ(raw)
	for i := 0; i+1 < len(f); i += 2 {
		m := strings.Fields(strings.TrimPrefix(f[i], ":"))
		changes = append(changes, change{m[0], m[1], m[2], m[3], m[4], f[i+1]})
	}
	var dels, puts []change
	for _, c := range changes {
		switch {
		case c.newMode == "160000" || c.oldMode == "160000":
			return fmt.Errorf("gitlink %q unsupported in v0.1", c.path)
		case c.status == "D":
			dels = append(dels, c)
		case c.status == "T":
			// Type change: delete then put, so a dir/file swap works.
			dels = append(dels, change{c.oldMode, "000000", c.oldOid, strings.Repeat("0", 40), "D", c.path})
			puts = append(puts, change{"000000", c.newMode, strings.Repeat("0", 40), c.newOid, "A", c.path})
		default:
			puts = append(puts, c)
		}
	}
	sort.Slice(dels, func(i, j int) bool {
		return depth(dels[i].path) > depth(dels[j].path) || (depth(dels[i].path) == depth(dels[j].path) && dels[i].path > dels[j].path)
	})
	sort.Slice(puts, func(i, j int) bool { return puts[i].path < puts[j].path })

	cat, err := newCatFile(s)
	if err != nil {
		return err
	}
	defer cat.Close()

	ops := 0
	tick := func() error {
		ops++
		if opt.CrashAfter > 0 && ops >= opt.CrashAfter {
			return ErrSimulatedCrash
		}
		return nil
	}
	root := s.WorkTree
	if opt.Resume {
		// A crash between write and rename leaves temp files behind.
		dirs := map[string]bool{}
		for _, c := range puts {
			dirs[filepath.Dir(c.path)] = true
		}
		for d := range dirs {
			tmps, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(d), ".armageddon-tmp-*"))
			for _, t := range tmps {
				os.Remove(t)
			}
		}
	}
	putNew := map[string][2]string{}
	for _, c := range puts {
		putNew[c.path] = [2]string{c.newMode, c.newOid}
	}
	// Directories that held deleted files; pruned if empty afterwards.
	touchedDirs := map[string]bool{}
	for _, c := range dels {
		p := filepath.Join(root, filepath.FromSlash(c.path))
		state, err := fileState(p, c.oldMode, c.oldOid, "000000", "")
		if err != nil {
			return err
		}
		// Record the parent even when the delete already happened before a
		// crash, so resume still prunes directories emptied by it.
		touchedDirs[filepath.Dir(c.path)] = true
		if state == stateOther && opt.Resume {
			// Already deleted and replaced before the crash, either by a
			// directory a later put created or by this path's own put (a
			// type change is a delete followed by a put).
			m, o, _ := DiskBlob(p)
			if pn, ok := putNew[c.path]; m == "040000" || (ok && pn == [2]string{m, o}) {
				continue
			}
		}
		switch state {
		case stateNew:
			continue // already deleted (resume)
		case stateOther:
			return fmt.Errorf("%w: %s changed before delete", ErrDiverged, c.path)
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		if err := tick(); err != nil {
			return err
		}
	}
	keepDirs := map[string]bool{}
	for _, d := range toMeta.EmptyDirs {
		keepDirs[d] = true
	}
	for _, d := range fromMeta.EmptyDirs {
		if !keepDirs[d] {
			_ = os.Remove(filepath.Join(root, filepath.FromSlash(d)))
			touchedDirs[filepath.Dir(d)] = true
		}
	}
	pruneEmptyDirs(root, touchedDirs, keepDirs)

	for _, c := range puts {
		p := filepath.Join(root, filepath.FromSlash(c.path))
		if m, _, _ := DiskBlob(p); m == "040000" {
			// Verification guarantees every synced child was deleted above,
			// so what remains is local ignored content. Move it aside.
			dst := filepath.Join(s.GitDir, "displaced", fmt.Sprintf("%d", time.Now().UnixNano()), filepath.FromSlash(c.path))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.Rename(p, dst); err != nil {
				return fmt.Errorf("displace %s: %w", c.path, err)
			}
			s.Displaced = append(s.Displaced, c.path)
		}
		state, err := fileState(p, c.oldMode, c.oldOid, c.newMode, c.newOid)
		if err != nil {
			return err
		}
		switch state {
		case stateNew:
			continue
		case stateOther:
			return fmt.Errorf("%w: %s changed before write", ErrDiverged, c.path)
		}
		if c.oldMode == c.newMode && c.oldOid == c.newOid {
			continue
		}
		content, err := cat.Blob(c.newOid)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(p, c.newMode, content); err != nil {
			return err
		}
		if err := tick(); err != nil {
			return err
		}
	}
	// Empty directories.
	for _, d := range toMeta.EmptyDirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			return err
		}
	}
	// Post-verify.
	cur, _, err := s.CaptureTree()
	if err != nil {
		return err
	}
	if cur != toWT {
		return fmt.Errorf("post-apply verify failed: have %s want %s", cur, toWT)
	}
	return s.applyIndex(fromMeta, toMeta)
}

// applyIndex moves the user's index from the `from` staged delta to the `to`
// one, touching only the affected paths so stat data elsewhere survives.
func (s *Shadow) applyIndex(fromMeta, toMeta Meta) error {
	if fromMeta.HeadOid != toMeta.HeadOid {
		// HEAD moved (a commit, checkout or reset happened on the writer).
		// The caller has already pointed HEAD at toMeta.HeadOid; rebuild the
		// index from it (mixed reset keeps stat data for unchanged entries)
		// and then apply the target's staged delta.
		if _, err := s.UserGit(nil, "reset", "-q"); err != nil {
			return err
		}
		fromMeta.Staged = nil
	}
	paths := map[string]bool{}
	for _, e := range fromMeta.Staged {
		paths[e.Path] = true
	}
	for _, e := range toMeta.Staged {
		paths[e.Path] = true
	}
	if len(paths) > 0 {
		var list []string
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		// Reset these paths to HEAD (removes them if absent in HEAD).
		if _, err := run(s.WorkTree, append(gitEnv(), "GIT_LITERAL_PATHSPECS=1"),
			[]byte(strings.Join(list, "\x00")), s.Prepare, "reset", "-q", "HEAD",
			"--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	if len(toMeta.Staged) > 0 {
		var oids []string
		var info bytes.Buffer
		zero := strings.Repeat("0", 40)
		// Remove existing entries first for deleted paths and for paths that
		// become unmerged (their stage-0 entry must go before stages 1-3).
		removed := map[string]bool{}
		for _, e := range toMeta.Staged {
			if (e.Stage > 0 || e.Mode == "000000") && !removed[e.Path] {
				removed[e.Path] = true
				fmt.Fprintf(&info, "0 %s\t%s\x00", zero, e.Path)
			}
		}
		for _, e := range toMeta.Staged {
			if e.Mode == "000000" {
				continue
			}
			oids = append(oids, e.Oid)
			fmt.Fprintf(&info, "%s %s %d\t%s\x00", e.Mode, e.Oid, e.Stage, e.Path)
		}
		if err := copyObjects(oids, s.env(), gitEnv(), s.WorkTree, s.Prepare); err != nil {
			return err
		}
		if _, err := s.UserGit(info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
			return err
		}
	}
	_, _ = s.UserGit(nil, "update-index", "-q", "--refresh")
	return nil
}

func depth(p string) int { return strings.Count(p, "/") }

func pruneEmptyDirs(root string, dirs map[string]bool, keep map[string]bool) {
	var list []string
	for d := range dirs {
		for d != "." && d != "" {
			list = append(list, d)
			d = filepath.Dir(d)
		}
	}
	sort.Slice(list, func(i, j int) bool { return depth(list[i]) > depth(list[j]) })
	for _, d := range list {
		if keep[filepath.ToSlash(d)] {
			continue
		}
		_ = os.Remove(filepath.Join(root, d)) // fails harmlessly when not empty
	}
}

const (
	stateOld = iota
	stateNew
	stateOther
)

// fileState classifies the on-disk path as matching the old version, the new
// version, or neither. Mode "000000" means "absent".
func fileState(p, oldMode, oldOid, newMode, newOid string) (int, error) {
	mode, oid, err := DiskBlob(p)
	if err != nil {
		return 0, err
	}
	if mode == oldMode && (mode == "000000" || oid == oldOid) {
		return stateOld, nil
	}
	if mode == newMode && (mode == "000000" || oid == newOid) {
		return stateNew, nil
	}
	return stateOther, nil
}

// DiskBlob returns the git mode and blob oid of a path as Git would record
// it, computed in-process (no subprocess per file).
func DiskBlob(p string) (string, string, error) {
	fi, err := os.Lstat(p)
	// ENOTDIR: a parent component is now a file (a directory became a file
	// before a crash), so this path does not exist either.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return "000000", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var content []byte
	mode := "100644"
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		t, err := os.Readlink(p)
		if err != nil {
			return "", "", err
		}
		content, mode = []byte(t), "120000"
	case fi.IsDir():
		return "040000", "", nil
	default:
		if fi.Mode()&0o100 != 0 {
			mode = "100755"
		}
		content, err = os.ReadFile(p)
		if err != nil {
			return "", "", err
		}
	}
	return mode, BlobOid(content), nil
}

func BlobOid(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func writeAtomic(p, mode string, content []byte) error {
	tmp := filepath.Join(filepath.Dir(p), fmt.Sprintf(".armageddon-tmp-%d", rand.Int63()))
	if mode == "120000" {
		if err := os.Symlink(string(content), tmp); err != nil {
			return err
		}
	} else {
		perm := os.FileMode(0o644)
		if mode == "100755" {
			perm = 0o755
		}
		if err := os.WriteFile(tmp, content, perm); err != nil {
			return err
		}
		if err := os.Chmod(tmp, perm); err != nil { // umask-independent
			return err
		}
	}
	return os.Rename(tmp, p)
}

type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func newCatFile(s *Shadow) (*catFile, error) {
	cmd := command(s.WorkTree, s.env(), s.Prepare, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd, in, bufio.NewReader(out)}, nil
}

func (c *catFile) Blob(oid string) ([]byte, error) {
	if _, err := fmt.Fprintln(c.in, oid); err != nil {
		return nil, err
	}
	hdr, err := c.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var o, typ string
	var size int
	if _, err := fmt.Sscanf(hdr, "%s %s %d", &o, &typ, &size); err != nil {
		return nil, fmt.Errorf("cat-file header %q: %w", hdr, err)
	}
	buf := make([]byte, size+1) // trailing LF
	if _, err := io.ReadFull(c.out, buf); err != nil {
		return nil, err
	}
	return buf[:size], nil
}

func (c *catFile) Close() { c.in.Close(); c.cmd.Wait() }

func splitZ(b []byte) []string {
	s := strings.Split(string(b), "\x00")
	if len(s) > 0 && s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

// PackSince returns a thin pack holding checkpoint cp and every object not
// reachable from base's tree (P-1). An empty base yields a full pack.
func (s *Shadow) PackSince(cp, base string) ([]byte, error) {
	revs := cp + "\n"
	if base != "" {
		revs += "^" + base + "^{tree}\n"
	}
	return s.Git([]byte(revs), "pack-objects", "--revs", "--thin", "--stdout", "-q")
}

// ReceivePack stores a pack from PackSince and points ref at cp. It fails if
// cp is not fully present afterwards.
func (s *Shadow) ReceivePack(pack []byte, ref, cp string) error {
	if len(pack) > 0 {
		if _, err := s.Git(pack, "index-pack", "--stdin", "--fix-thin"); err != nil {
			return err
		}
	}
	if _, err := s.Git(nil, "rev-list", "--objects", "--quiet", cp); err != nil {
		return fmt.Errorf("checkpoint %s incomplete after pack: %w", cp, err)
	}
	_, err := s.Git(nil, "update-ref", ref, cp)
	return err
}

// TransferPack moves cp between two local shadows (tests and tools).
func TransferPack(src, dst *Shadow, ref, cp, base string) (int, error) {
	pack, err := src.PackSince(cp, base)
	if err != nil {
		return 0, err
	}
	return len(pack), dst.ReceivePack(pack, ref, cp)
}
