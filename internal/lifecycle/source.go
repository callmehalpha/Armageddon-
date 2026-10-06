package lifecycle

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/release"
)

// DefaultReleaseBase is where releases are downloaded from (owner decision
// D2: the install URL; GitHub Releases by default).
const DefaultReleaseBase = "https://github.com/callmehalpha/Armageddon-/releases"

// Source provides a release: its manifest, signature and artefacts.
type Source struct {
	// Bundle is a local release bundle (.tar) for offline installs; when
	// empty the release is downloaded from Base.
	Bundle string
	// Base is the releases URL (default DefaultReleaseBase).
	Base string
	// Version to install ("" = latest).
	Version string
	// PublicKey verifies the manifest ("" = the key embedded at build time).
	PublicKey string
	// AllowUnsigned installs without a signature check (development only).
	AllowUnsigned bool
	HTTP          *http.Client
	GOOS, GOARCH  string
}

// Stage downloads (or unpacks) the release, verifies the manifest
// signature and the binary's checksum, checks min_upgrade_from against
// `from`, and installs it as versions/<v>/. It returns the version.
func (s *Source) Stage(l Layout, from string, logf func(string, ...any)) (string, error) {
	goos, goarch := s.GOOS, s.GOARCH
	if goos == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	tmp, err := os.MkdirTemp(l.Versions(), ".staging-")
	if err != nil {
		if err := os.MkdirAll(l.Versions(), 0o755); err != nil {
			return "", err
		}
		if tmp, err = os.MkdirTemp(l.Versions(), ".staging-"); err != nil {
			return "", err
		}
	}
	defer os.RemoveAll(tmp)
	bin := release.BinaryName(goos, goarch)
	if s.Bundle != "" {
		if err := untarBundle(s.Bundle, tmp); err != nil {
			return "", fmt.Errorf("bundle %s: %w", s.Bundle, err)
		}
	} else {
		base := s.Base
		if base == "" {
			base = DefaultReleaseBase
		}
		dir := base + "/latest/download"
		if s.Version != "" {
			dir = base + "/download/v" + strings.TrimPrefix(s.Version, "v")
		}
		for _, n := range []string{"manifest.json", "manifest.json.minisig"} {
			if err := s.download(dir+"/"+n, filepath.Join(tmp, n)); err != nil {
				if n == "manifest.json.minisig" && s.AllowUnsigned {
					continue
				}
				return "", err
			}
		}
		// The binary is fetched after the manifest so its name is known to be listed.
		if err := s.download(dir+"/"+bin, filepath.Join(tmp, bin)); err != nil {
			return "", err
		}
	}
	mb, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil {
		return "", fmt.Errorf("release has no manifest.json: %w", err)
	}
	var m *release.Manifest
	sig, sigErr := os.ReadFile(filepath.Join(tmp, "manifest.json.minisig"))
	switch {
	case sigErr == nil:
		m, err = release.Verify(mb, sig, s.PublicKey)
		if errors.Is(err, release.ErrNoPublicKey) && s.AllowUnsigned {
			logf("WARNING: no release public key configured; signature NOT checked (--allow-unsigned)")
			m, err = release.Parse(mb)
		}
	case s.AllowUnsigned:
		logf("WARNING: the release is unsigned; installing anyway (--allow-unsigned)")
		m, err = release.Parse(mb)
	default:
		err = fmt.Errorf("the release has no signature (manifest.json.minisig); refusing it (use --allow-unsigned only for development builds)")
	}
	if err != nil {
		return "", err
	}
	if s.Version != "" && release.CompareVersions(m.Version, s.Version) != 0 {
		return "", fmt.Errorf("asked for %s but the release manifest is for %s", s.Version, m.Version)
	}
	if from != "" {
		if err := m.CanUpgrade(from); err != nil {
			return "", err
		}
	}
	if err := m.VerifyFile(bin, filepath.Join(tmp, bin)); err != nil {
		return "", err
	}
	dst := l.VersionDir(m.Version)
	if _, err := os.Stat(filepath.Join(dst, "armageddon")); err == nil {
		if err := m.VerifyFile(bin, filepath.Join(dst, "armageddon")); err == nil {
			logf("version %s already staged and verified", m.Version)
			return m.Version, nil
		}
		return "", fmt.Errorf("%s exists but does not match the release manifest; remove it and retry", dst)
	}
	stage := filepath.Join(tmp, "v")
	if err := os.Mkdir(stage, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(filepath.Join(tmp, bin), filepath.Join(stage, "armageddon")); err != nil {
		return "", err
	}
	if err := os.Chmod(filepath.Join(stage, "armageddon"), 0o755); err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(stage, "manifest.json"), mb, 0o644)
	if sigErr == nil {
		os.WriteFile(filepath.Join(stage, "manifest.json.minisig"), sig, 0o644)
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(stage, dst); err != nil {
		return "", err
	}
	logf("version %s staged in %s (signature and sha256 verified)", m.Version, dst)
	return m.Version, nil
}

func (s *Source) download(url, dst string) error {
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// untarBundle extracts the flat files of a release bundle.
func untarBundle(path, dir string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Base(h.Name)
		if h.Typeflag != tar.TypeReg || name == "." || name == ".." {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}
