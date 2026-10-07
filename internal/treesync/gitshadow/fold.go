package gitshadow

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Case- and normalisation-insensitive filesystems (macOS APFS by default).
// Ported from prototypes/internal/gitshadow/fold.go (P2 on macOS, ⟨P-20⟩,
// ⟨P-21⟩).
//
// The capture index persists between captures and the shadow runs with
// core.ignorecase=false and core.precomposeunicode=false, so names are kept
// byte-exact. `git add -A` decides that an indexed path was deleted by
// lstat()ing it. On an insensitive filesystem lstat("README.md") still
// succeeds after a rename to "readme.md", and lstat(<NFC name>) succeeds when
// only the NFD name exists. The stale entry is therefore never removed, while
// the real on-disk name is added as untracked: the captured tree holds both
// names. That breaks post-apply verification on the Mac and gives
// case-sensitive replicas a duplicate file.

// forceFold makes the insensitive-filesystem paths run on any filesystem
// (tests on Linux).
var forceFold = false

func (s *Shadow) foldInsensitive() bool {
	if forceFold {
		return true
	}
	return probeInsensitive(s.WorkTree)
}

var probeCache sync.Map // worktree -> bool

// probeInsensitive creates a probe file and checks whether a case-changed or
// normalisation-changed spelling resolves to it. A worktree the process
// cannot write to is reported as sensitive (the Linux server, where the
// seat belongs to the workspace user).
func probeInsensitive(dir string) bool {
	if v, ok := probeCache.Load(dir); ok {
		return v.(bool)
	}
	// A unique name: a probe left behind by a crash must not make the
	// filesystem read as sensitive (O_EXCL fails on an existing name).
	prefix := fmt.Sprintf(".armageddon-tmp-probe-%d-%d-", os.Getpid(), rand.Int63())
	p := filepath.Join(dir, prefix+"A\u00e9") // NFC é
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	defer os.Remove(p)
	insensitive := false
	for _, alt := range []string{prefix + "a\u00e9", prefix + "Ae\u0301"} { // case, NFD
		if _, err := os.Lstat(filepath.Join(dir, alt)); err == nil {
			insensitive = true
		}
	}
	probeCache.Store(dir, insensitive)
	return insensitive
}

// dropFoldedStale removes every index entry under scope (slash-form
// directories, "." for all) whose path, component by component, is not
// spelled exactly as readdir() reports it (⟨P-20⟩). It runs only when the
// work tree's filesystem is insensitive, so Linux pays nothing.
func (s *Shadow) dropFoldedStale(scope ...string) error {
	if !s.foldInsensitive() {
		return nil
	}
	if len(scope) == 0 {
		scope = []string{"."}
	}
	out, err := s.Git(nil, append([]string{"--literal-pathspecs", "ls-files", "-z", "--"}, scope...)...)
	if err != nil {
		return err
	}
	listings := map[string]map[string]bool{}
	exact := func(dir, name string) (bool, error) {
		l, ok := listings[dir]
		if !ok {
			l = map[string]bool{}
			ents, err := os.ReadDir(filepath.Join(s.WorkTree, filepath.FromSlash(dir)))
			// A missing directory means every entry under it is stale; any
			// other error must not be read as "empty", or the whole
			// directory would be dropped from the capture.
			if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				return false, err
			}
			for _, e := range ents {
				l[e.Name()] = true
			}
			listings[dir] = l
		}
		return l[name], nil
	}
	var stale []string
	for _, p := range splitZ(out) {
		dir := "."
		for _, c := range strings.Split(p, "/") {
			ok, err := exact(dir, c)
			if err != nil {
				return err
			}
			if !ok {
				stale = append(stale, p)
				break
			}
			if dir == "." {
				dir = c
			} else {
				dir += "/" + c
			}
		}
	}
	if len(stale) == 0 {
		return nil
	}
	_, err = s.Git([]byte(strings.Join(stale, "\x00")+"\x00"), "update-index", "-z", "--force-remove", "--stdin")
	return err
}

// CollisionError reports a checkpoint whose paths collide under the
// replica's filesystem rules (⟨P-21⟩). Apply refuses it before touching the
// working tree; it is not a divergence, so callers must not quarantine and
// retry.
type CollisionError struct {
	Groups [][]string // each group: distinct paths that name the same file here
}

func (e *CollisionError) Error() string {
	var g []string
	for _, grp := range e.Groups {
		g = append(g, strings.Join(grp, " / "))
	}
	return fmt.Sprintf("checkpoint has paths that collide on this case-insensitive filesystem: %s", strings.Join(g, "; "))
}

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
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// checkCollisions returns a *CollisionError when the work tree's filesystem
// is insensitive and tree holds colliding paths.
func (s *Shadow) checkCollisions(tree string, emptyDirs []string) error {
	if tree == EmptyTree || !s.foldInsensitive() {
		return nil
	}
	out, err := s.Git(nil, "ls-tree", "-r", "-z", "--name-only", tree)
	if err != nil {
		return err
	}
	if g := Collisions(append(splitZ(out), emptyDirs...)); len(g) > 0 {
		return &CollisionError{Groups: g}
	}
	return nil
}
