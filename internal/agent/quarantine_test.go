package agent

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func tarOf(t *testing.T, entries ...tar.Header) *bytes.Reader {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range entries {
		h := h
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	return bytes.NewReader(b.Bytes())
}

func TestUntarRefusesUnsafePaths(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]tar.Header{
		"dotdot":       {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"git dir":      {{Name: ".git/hooks/pre-commit", Typeflag: tar.TypeReg, Mode: 0o755}},
		"through link": {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: outside}, {Name: "l/evil", Typeflag: tar.TypeReg, Mode: 0o644}},
	} {
		dir := t.TempDir()
		if _, err := untar(tarOf(t, entries...), dir); err == nil {
			t.Errorf("%s: untar accepted an unsafe archive", name)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
		t.Fatal("a file was written through a symlink")
	}
	dir := t.TempDir()
	n, err := untar(tarOf(t, tar.Header{Name: "src/", Typeflag: tar.TypeDir, Mode: 0o755}, tar.Header{Name: "src/a.go", Typeflag: tar.TypeReg, Mode: 0o644}), dir)
	if err != nil || n != 1 {
		t.Fatalf("untar of a normal archive: n=%d err=%v", n, err)
	}
}
