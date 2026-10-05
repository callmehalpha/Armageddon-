package components

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	dir              bool
}

func tarball(t *testing.T, entries []entry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755}
		switch {
		case e.dir:
			h.Typeflag = tar.TypeDir
		case e.link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func serve(t *testing.T, body []byte) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write(body) }))
	t.Cleanup(srv.Close)
	old := downloadBase
	downloadBase = srv.URL
	t.Cleanup(func() { downloadBase = old })
}

func TestInstallCodeServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	top := "code-server-9.0.0-" + platform() + "/"
	good := tarball(t, []entry{{name: top, dir: true}, {name: top + "bin/", dir: true},
		{name: top + "bin/code-server", body: "#!/bin/sh\necho fake\n"}, {name: top + "lib/x.js", body: "x"},
		{name: top + "node_modules/.bin/x", link: "../../lib/x.js"}})
	sum := sha256.Sum256(good)
	codeServerSHA256["9.0.0/"+platform()] = hex.EncodeToString(sum[:])
	defer delete(codeServerSHA256, "9.0.0/"+platform())
	serve(t, good)
	data := t.TempDir()
	exe, err := InstallCodeServer(data, "v9.0.0", "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(exe); err != nil || !strings.Contains(string(b), "fake") {
		t.Fatalf("installed executable: %q %v", b, err)
	}
	if got, err := ResolveCodeServer("", data); err != nil || got != exe {
		// PATH may contain a real code-server; only check when it doesn't.
		if _, lookErr := os.Stat("/usr/bin/code-server"); lookErr != nil {
			t.Fatalf("resolve = %q, %v; want %q", got, err, exe)
		}
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o005 != 0o005 {
		t.Fatalf("executable mode %v: workspace users must be able to run it", fi.Mode())
	}

	// A tampered download is refused and installs nothing.
	codeServerSHA256["9.0.1/"+platform()] = strings.Repeat("0", 64)
	defer delete(codeServerSHA256, "9.0.1/"+platform())
	if _, err := InstallCodeServer(data, "9.0.1", "", io.Discard); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered download: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(CodeServerDir(data), "9.0.1")); err == nil {
		t.Fatal("tampered version was installed")
	}
	// Unpinned versions need an explicit checksum.
	if _, err := InstallCodeServer(data, "9.9.9", "", io.Discard); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("unpinned: err = %v", err)
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	for _, es := range [][]entry{
		{{name: "top/", dir: true}, {name: "top/../../evil", body: "x"}},
		{{name: "top/", dir: true}, {name: "top/l", link: "/etc/passwd"}},
		{{name: "top/", dir: true}, {name: "top/a/l", link: "../../../x"}},
	} {
		if err := extractTarGz(bytes.NewReader(tarball(t, es)), t.TempDir()); err == nil {
			t.Fatalf("archive %v was accepted", es)
		}
	}
}
