package gitshadow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafePath reports a path in a checkpoint or quarantine that apply
// refuses to write: it would land in a Git directory or outside the
// working tree (M9.2 security review, finding 1). A checkpoint comes from
// another seat, so its paths are checked like any untrusted input, on top
// of `index-pack --strict` refusing such trees on arrival.
var ErrUnsafePath = errors.New("unsafe path")

// CheckPath validates rel (slash-separated, relative to root) before a
// write or delete: no absolute path, no empty, "." or ".." component, no
// component that names a Git directory (".git", case-insensitively, with
// the trailing dots and spaces Windows and macOS ignore, or its 8.3 name
// "git~1"), and no parent directory that is a symbolic link on disk.
func CheckPath(root, rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) || strings.Contains(rel, "\\") {
		return fmt.Errorf("%w: %q", ErrUnsafePath, rel)
	}
	parts := strings.Split(rel, "/")
	for _, c := range parts {
		if c == "" || c == "." || c == ".." || isGitDirName(c) {
			return fmt.Errorf("%w: %q", ErrUnsafePath, rel)
		}
	}
	p := root
	for _, c := range parts[:len(parts)-1] {
		p = filepath.Join(p, c)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil // created by the apply as a real directory
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %q goes through the symbolic link %q", ErrUnsafePath, rel, strings.TrimPrefix(p, root+string(filepath.Separator)))
		}
	}
	return nil
}

func isGitDirName(c string) bool {
	c = strings.ToLower(strings.TrimRight(c, ". "))
	// HFS+ ignores some zero-width code points in names.
	for _, z := range []string{"\u200c", "\u200d", "\u200e", "\u200f", "\u202a", "\u202b", "\u202c", "\u202d", "\u202e", "\u206a", "\u206b", "\u206c", "\u206d", "\u206e", "\u206f", "\ufeff"} {
		c = strings.ReplaceAll(c, z, "")
	}
	return c == ".git" || c == "git~1"
}
