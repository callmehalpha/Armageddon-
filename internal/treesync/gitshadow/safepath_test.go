package gitshadow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"a.txt", "real/b.txt", "src/.gitignore", "x/.github/ci.yml", "link"} {
		if err := CheckPath(root, ok); err != nil {
			t.Errorf("CheckPath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../b", "./a", "a//b", ".git/config",
		"sub/.git/hooks/post-checkout", ".GIT/config", ".git./config", ".git /config", "git~1/config",
		".g‌it/config", "link/passwd", "a\\b"} {
		if err := CheckPath(root, bad); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("CheckPath(%q) = %v, want ErrUnsafePath", bad, err)
		}
	}
}
