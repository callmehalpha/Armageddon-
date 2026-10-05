// Package components resolves and installs optional third-party components
// the server runs on behalf of workspaces. Today that is code-server (plan
// M4.3). The release manifest (Phase 4) will pin versions centrally; until
// then the pins live here.
package components

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CodeServerVersion is the version `components install code-server` picks
// when none is given.
const CodeServerVersion = "4.118.0"

// codeServerSHA256 pins the upstream release tarballs by version and
// platform. Computed from the GitHub release downloads; a version that is
// not listed here cannot be installed without an explicit --sha256.
var codeServerSHA256 = map[string]string{
	"4.118.0/linux-amd64": "ab4dee01cacc20eb500c96660477d8ba755f69f402cc9cbab3a8496b4690f2fd",
	"4.118.0/linux-arm64": "70dd29a9bffa1ca7a9578e24106e612ee041192bf6aa5ece964b1af1e3d27c08",
}

// downloadBase is the release download URL prefix (a variable for tests).
var downloadBase = "https://github.com/coder/code-server/releases/download"

// CodeServerDir is where installed versions live.
func CodeServerDir(dataDir string) string { return filepath.Join(dataDir, "components", "code-server") }

// ResolveCodeServer finds the code-server executable: the configured path,
// else code-server in PATH, else the installed component.
func ResolveCodeServer(configured, dataDir string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("code_server.path: %w", err)
		}
		return configured, nil
	}
	if p, err := exec.LookPath("code-server"); err == nil {
		return filepath.Abs(p)
	}
	p := filepath.Join(CodeServerDir(dataDir), "current", "bin", "code-server")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", errors.New("code-server is not installed: run `armageddon server components install code-server`, put code-server in PATH, or set code_server.path in server.json")
}

func platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// InstallCodeServer downloads the upstream release tarball of version,
// verifies it against the pinned SHA-256 (or wantSHA when given, for
// versions not pinned here), unpacks it under the data directory and points
// "current" at it. It returns the executable's path.
func InstallCodeServer(dataDir, version, wantSHA string, out io.Writer) (string, error) {
	if version == "" {
		version = CodeServerVersion
	}
	version = strings.TrimPrefix(version, "v")
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("code-server components are available for Linux servers only (this is %s)", runtime.GOOS)
	}
	if strings.ContainsAny(version, "/\\ ") || version == "" {
		return "", fmt.Errorf("invalid version %q", version)
	}
	if wantSHA == "" {
		wantSHA = codeServerSHA256[version+"/"+platform()]
	}
	if wantSHA == "" {
		return "", fmt.Errorf("code-server %s for %s is not pinned in this build; pass --sha256 to install it anyway", version, platform())
	}
	base := CodeServerDir(dataDir)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	for d := base; d != filepath.Dir(dataDir) && d != "/" && d != "."; d = filepath.Dir(d) {
		// Workspace users execute code-server: the path must be traversable.
		os.Chmod(d, 0o755)
	}
	url := fmt.Sprintf("%s/v%s/code-server-%s-%s.tar.gz", downloadBase, version, version, platform())
	fmt.Fprintf(out, "Downloading %s\n", url)
	tmp, err := os.CreateTemp(base, "download-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hc := &http.Client{Timeout: 30 * time.Minute}
	resp, err := hc.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA) {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, wantSHA)
	}
	fmt.Fprintf(out, "Checksum OK (sha256 %s)\n", wantSHA)
	if _, err := tmp.Seek(0, 0); err != nil {
		return "", err
	}
	dst := filepath.Join(base, version)
	stage := dst + ".partial"
	os.RemoveAll(stage)
	if err := extractTarGz(tmp, stage); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	exe := filepath.Join(stage, "bin", "code-server")
	if _, err := os.Stat(exe); err != nil {
		os.RemoveAll(stage)
		return "", fmt.Errorf("unexpected archive layout: %w", err)
	}
	os.RemoveAll(dst)
	if err := os.Rename(stage, dst); err != nil {
		return "", err
	}
	cur := filepath.Join(base, "current")
	link := cur + ".new"
	os.Remove(link)
	if err := os.Symlink(version, link); err != nil {
		return "", err
	}
	if err := os.Rename(link, cur); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "Installed code-server %s in %s\n", version, dst)
	return filepath.Join(cur, "bin", "code-server"), nil
}

// extractTarGz unpacks an archive whose entries share one top-level
// directory into dst, stripping that directory. Entries escaping dst, and
// symlinks pointing outside it, are rejected.
func extractTarGz(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		for _, part := range strings.Split(filepath.ToSlash(hdr.Name), "/") {
			if part == ".." {
				return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
			}
		}
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		} else {
			continue // the top-level directory itself
		}
		if name == "" || name == "." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		mode := os.FileMode(hdr.Mode).Perm()&0o755 | 0o444
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			os.Chmod(target, 0o755)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
			os.Chmod(target, mode)
		case tar.TypeSymlink:
			resolved := filepath.Join(filepath.Dir(target), hdr.Linkname)
			if filepath.IsAbs(hdr.Linkname) || !strings.HasPrefix(resolved, filepath.Clean(dst)+string(filepath.Separator)) {
				return fmt.Errorf("unsafe symlink in archive: %s -> %s", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Hard links, devices and FIFOs are not expected in a release.
			return fmt.Errorf("unsupported entry %s (type %c)", hdr.Name, hdr.Typeflag)
		}
	}
}
