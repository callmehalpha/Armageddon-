package release

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aead.dev/minisign"
)

// plainKey generates an unencrypted throwaway key: encrypting with the
// minisign scrypt parameters takes seconds per operation.
func plainKey(t *testing.T) (secret []byte, pub string) {
	t.Helper()
	pk, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret, err = priv.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pk.MarshalText()
	return secret, string(b)
}

// fixture builds a release directory with two artefacts, a manifest and a
// signature made with a key generated for this test only.
func fixture(t *testing.T) (dir, pub string, manifest, sig []byte) {
	t.Helper()
	dir = t.TempDir()
	for _, n := range []string{"armageddon-linux-amd64", "armageddon-linux-arm64", "install.sh", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("contents of "+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Build(dir, "v0.2.0", "abc123", "0.1.0", 1, &CodeServer{Version: "4.0.0", Files: []CodeServerFile{{OS: "linux", Arch: "amd64", URL: "https://example.invalid/cs.tgz", SHA256: "00"}}})
	if err != nil {
		t.Fatal(err)
	}
	secret, pub := plainKey(t)
	manifest = m.Marshal()
	sig, err = Sign(manifest, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	return dir, pub, manifest, sig
}

func TestVerifyGoodManifestAndArtefacts(t *testing.T) {
	dir, pub, manifest, sig := fixture(t)
	m, err := Verify(manifest, sig, pub)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.2.0" || m.SchemaVersion != 1 || m.MinUpgradeFrom != "0.1.0" || m.CodeServer == nil {
		t.Fatalf("unexpected manifest %+v", m)
	}
	if len(m.Files) != 3 {
		t.Fatalf("want 3 files (two binaries and install.sh), got %+v", m.Files)
	}
	for _, f := range m.Files {
		if err := m.VerifyFile(f.Name, filepath.Join(dir, f.Name)); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ := m.File("armageddon-linux-arm64"); f.OS != "linux" || f.Arch != "arm64" {
		t.Fatalf("os/arch not parsed: %+v", f)
	}
}

func TestTamperedManifestRefused(t *testing.T) {
	_, pub, manifest, sig := fixture(t)
	tampered := bytes.Replace(manifest, []byte(`"version": "0.2.0"`), []byte(`"version": "9.9.9"`), 1)
	if bytes.Equal(tampered, manifest) {
		t.Fatal("fixture did not change")
	}
	if _, err := Verify(tampered, sig, pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered manifest: want ErrBadSignature, got %v", err)
	}
	// A different key's signature is refused too.
	_, otherPub := plainKey(t)
	if _, err := Verify(manifest, sig, otherPub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: want ErrBadSignature, got %v", err)
	}
}

func TestTamperedArtefactRefused(t *testing.T) {
	dir, pub, manifest, sig := fixture(t)
	m, err := Verify(manifest, sig, pub)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "armageddon-linux-amd64")
	if err := os.WriteFile(p, []byte("contents of armageddon-linux-amd64 plus a backdoor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyFile("armageddon-linux-amd64", p); !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if err := m.VerifyFile("not-listed", p); err == nil {
		t.Fatal("unlisted artefact accepted")
	}
}

func TestNoPublicKey(t *testing.T) {
	_, _, manifest, sig := fixture(t)
	old := PublicKey
	PublicKey = ""
	defer func() { PublicKey = old }()
	if _, err := Verify(manifest, sig, ""); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("want ErrNoPublicKey, got %v", err)
	}
}

func TestPublicKeyFileFormatAndDerivation(t *testing.T) {
	if testing.Short() {
		t.Skip("encrypted minisign keys use expensive scrypt parameters")
	}
	secret, pub, err := GenerateKey("pw")
	if err != nil {
		t.Fatal(err)
	}
	derived, err := PublicKeyOf(secret, "pw")
	if err != nil || derived != pub {
		t.Fatalf("derived %q, want %q (%v)", derived, pub, err)
	}
	if strings.ContainsAny(pub, " \n") || !strings.HasPrefix(pub, "RW") {
		t.Fatalf("public key should be the bare base64 line, got %q", pub)
	}
	if _, err := ParsePublicKey("untrusted comment: minisign public key\n" + pub + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := PublicKeyOf(secret, "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0}, {"v0.1.0", "0.1.0", 0}, {"0.1.0", "0.2.0", -1}, {"0.10.0", "0.9.9", 1},
		{"0.2.0-rc1", "0.2.0", -1}, {"0.1.0-mvp", "0.1.0", -1}, {"1.0.0", "0.99.99", 1},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	m := &Manifest{Version: "0.3.0", MinUpgradeFrom: "0.2.0"}
	if m.CanUpgrade("0.1.5") == nil || m.CanUpgrade("0.2.0") != nil {
		t.Fatal("min_upgrade_from not enforced")
	}
}
