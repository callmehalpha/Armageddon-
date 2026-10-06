package gitshadow

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Case- and normalisation-insensitive filesystems (macOS APFS by default).
//
// The capture index persists between captures and the shadow runs with
// core.ignorecase=false and core.precomposeunicode=false, so names are kept
// byte-exact. `git add -A` decides that an indexed path was deleted by
// lstat()ing it. On an insensitive filesystem lstat("README.md") still
// succeeds after a rename to "readme.md", and lstat(<NFC name>) succeeds when
// only the NFD name exists. The stale entry is therefore never removed, while
// the real on-disk name is added as untracked: the captured tree holds both
// names (P2 macOS, Phase 1). That breaks post-apply verification on the Mac
// and gives case-sensitive replicas a duplicate file.
//
// dropFoldedStale removes every index entry whose path, component by
// component, is not spelled exactly as readdir() reports it. It runs only
// when the work tree's filesystem is insensitive, so Linux pays nothing.

var forceFold = os.Getenv("GITSHADOW_FORCE_FOLD_CHECK") == "1" // tests: run on any FS

func (s *Shadow) foldInsensitive() bool {
	if forceFold {
		return true
	}
	return probeInsensitive(s.WorkTree)
}

var probeCache sync.Map // worktree -> bool

// probeInsensitive creates a probe file and checks whether a case-changed or
// normalisation-changed spelling resolves to it.
func probeInsensitive(dir string) bool {
	if v, ok := probeCache.Load(dir); ok {
		return v.(bool)
	}
	name := ".armageddon-tmp-probe-Aé"
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	defer os.Remove(p)
	insensitive := false
	for _, alt := range []string{".armageddon-tmp-probe-aé", ".armageddon-tmp-probe-Aé"} {
		if _, err := os.Lstat(filepath.Join(dir, alt)); err == nil {
			insensitive = true
		}
	}
	probeCache.Store(dir, insensitive)
	return insensitive
}

func (s *Shadow) dropFoldedStale() error {
	if !s.foldInsensitive() {
		return nil
	}
	out, err := s.Git(nil, "ls-files", "-z")
	if err != nil {
		return err
	}
	listings := map[string]map[string]bool{}
	exact := func(dir, name string) bool {
		l, ok := listings[dir]
		if !ok {
			l = map[string]bool{}
			if ents, err := os.ReadDir(filepath.Join(s.WorkTree, filepath.FromSlash(dir))); err == nil {
				for _, e := range ents {
					l[e.Name()] = true
				}
			}
			listings[dir] = l
		}
		return l[name]
	}
	var stale []string
	for _, p := range splitZ(out) {
		parts := strings.Split(p, "/")
		dir := "."
		for _, c := range parts {
			if !exact(dir, c) {
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
