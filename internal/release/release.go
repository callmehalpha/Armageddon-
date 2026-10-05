// Package release is the signed release manifest (contract §9.1): building
// it in the release pipeline, signing it with a minisign-compatible Ed25519
// key, and verifying a manifest and the artefacts it lists before anything
// is installed.
//
// The private signing key never lives in this repository. The release
// workflow reads it from a GitHub secret; tests generate throwaway keys. The
// public key is embedded at build time:
//
//	go build -ldflags "-X github.com/callmehalpha/Armageddon-/internal/release.PublicKey=RWQ…"
package release

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aead.dev/minisign"
)

// PublicKey is the minisign public key (the base64 "RW…" line) that release
// manifests must be signed with. Empty in development builds; set with
// -ldflags -X by the release workflow. Owner decision D1 decides who holds
// the matching private key.
var PublicKey = ""

// ManifestFormat is the version of the manifest document itself.
const ManifestFormat = 1

// File is one artefact listed in the manifest.
type File struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
}

// CodeServerFile is one pinned upstream code-server tarball (Phase 5 uses it).
type CodeServerFile struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// CodeServer pins the code-server version shipped with a release.
type CodeServer struct {
	Version string           `json:"version"`
	Files   []CodeServerFile `json:"files"`
}

// Manifest describes one release.
type Manifest struct {
	Format         int         `json:"format"`
	Version        string      `json:"version"`
	Commit         string      `json:"commit,omitempty"`
	Created        string      `json:"created"`
	MinUpgradeFrom string      `json:"min_upgrade_from"`
	SchemaVersion  int         `json:"schema_version"`
	Files          []File      `json:"files"`
	CodeServer     *CodeServer `json:"code_server,omitempty"`
}

// File returns the entry called name.
func (m *Manifest) File(name string) (*File, bool) {
	for i := range m.Files {
		if m.Files[i].Name == name {
			return &m.Files[i], true
		}
	}
	return nil, false
}

// BinaryName is the artefact name of the binary for an OS and architecture.
func BinaryName(goos, goarch string) string { return "armageddon-" + goos + "-" + goarch }

var (
	ErrBadSignature = errors.New("release manifest signature is invalid")
	ErrNoPublicKey  = errors.New("no release public key is configured: this build cannot verify release signatures (pass --pubkey, or --allow-unsigned to skip verification)")
	ErrChecksum     = errors.New("artefact checksum mismatch")
)

// ParsePublicKey accepts either the bare base64 key or the contents of a
// minisign .pub file (an "untrusted comment:" line followed by the key).
func ParsePublicKey(text string) (minisign.PublicKey, error) {
	var pk minisign.PublicKey
	line := ""
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "untrusted comment:") {
			line = l
		}
	}
	if line == "" {
		return pk, ErrNoPublicKey
	}
	if err := pk.UnmarshalText([]byte(line)); err != nil {
		return pk, fmt.Errorf("invalid minisign public key: %w", err)
	}
	return pk, nil
}

// Verify checks the minisign signature of a manifest and parses it. pubkey
// empty means the embedded PublicKey.
func Verify(manifest, signature []byte, pubkey string) (*Manifest, error) {
	if pubkey == "" {
		pubkey = PublicKey
	}
	if strings.TrimSpace(pubkey) == "" {
		return nil, ErrNoPublicKey
	}
	pk, err := ParsePublicKey(pubkey)
	if err != nil {
		return nil, err
	}
	if !minisign.Verify(pk, manifest, signature) {
		return nil, ErrBadSignature
	}
	return Parse(manifest)
}

// Parse decodes a manifest without checking any signature.
func Parse(manifest []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("manifest format %d is not supported (want %d)", m.Format, ManifestFormat)
	}
	if m.Version == "" {
		return nil, errors.New("manifest has no version")
	}
	return &m, nil
}

// SHA256File returns the hex sha256 and size of a file.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyFile checks that the file at path is the artefact `name` listed in
// the manifest.
func (m *Manifest) VerifyFile(name, path string) error {
	f, ok := m.File(name)
	if !ok {
		return fmt.Errorf("%s is not listed in the manifest for %s", name, m.Version)
	}
	sum, size, err := SHA256File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, f.SHA256) || (f.Size > 0 && size != f.Size) {
		return fmt.Errorf("%w: %s has sha256 %s, the manifest says %s", ErrChecksum, name, sum, f.SHA256)
	}
	return nil
}

// Build creates a manifest for every armageddon-* binary, install.sh and
// systemd unit (*.service) in dir. The signature and the manifest itself are skipped.
func Build(dir, version, commit, minUpgradeFrom string, schemaVersion int, cs *CodeServer) (*Manifest, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: ManifestFormat, Version: strings.TrimPrefix(version, "v"), Commit: commit,
		Created: time.Now().UTC().Format(time.RFC3339), MinUpgradeFrom: strings.TrimPrefix(minUpgradeFrom, "v"),
		SchemaVersion: schemaVersion, CodeServer: cs, Files: []File{}}
	for _, e := range ents {
		n := e.Name()
		listed := strings.HasPrefix(n, "armageddon-") || n == "install.sh" || strings.HasSuffix(n, ".service")
		if e.IsDir() || !listed || strings.HasSuffix(n, ".tar") || strings.HasSuffix(n, ".minisig") {
			continue
		}
		sum, size, err := SHA256File(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		f := File{Name: n, SHA256: sum, Size: size}
		if parts := strings.Split(n, "-"); len(parts) == 3 && parts[0] == "armageddon" {
			f.OS, f.Arch = parts[1], parts[2]
		}
		m.Files = append(m.Files, f)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("no release artefacts in %s", dir)
	}
	return m, nil
}

// Marshal renders the manifest as the exact bytes that get signed.
func (m *Manifest) Marshal() []byte {
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n')
}

// Sign signs a manifest with an encrypted minisign secret key (the contents
// of a minisign .key file) and its password.
func Sign(manifest, secretKey []byte, password string) ([]byte, error) {
	priv, err := DecryptKey(secretKey, password)
	if err != nil {
		return nil, err
	}
	m, err := Parse(manifest)
	if err != nil {
		return nil, err
	}
	trusted := fmt.Sprintf("armageddon %s manifest", m.Version)
	return minisign.SignWithComments(priv, manifest, trusted, "signature from the armageddon release key"), nil
}

// DecryptKey decodes a minisign secret key file (encrypted or not).
func DecryptKey(secretKey []byte, password string) (minisign.PrivateKey, error) {
	if minisign.IsEncrypted(secretKey) {
		return minisign.DecryptKey(password, secretKey)
	}
	var priv minisign.PrivateKey
	return priv, priv.UnmarshalText(secretKey)
}

// PublicKeyOf returns the base64 public key matching a secret key.
func PublicKeyOf(secretKey []byte, password string) (string, error) {
	priv, err := DecryptKey(secretKey, password)
	if err != nil {
		return "", err
	}
	b, err := priv.Public().(minisign.PublicKey).MarshalText()
	return string(b), err
}

// GenerateKey creates a key pair: the encrypted secret key file contents
// and the public key (base64 line).
func GenerateKey(password string) (secret []byte, public string, err error) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	secret, err = minisign.EncryptKey(password, priv)
	if err != nil {
		return nil, "", err
	}
	b, err := pub.MarshalText()
	return secret, string(b), err
}

// CompareVersions compares dotted versions ("0.1.0", "v0.2.0-rc1"). A
// pre-release suffix sorts before the release it precedes.
func CompareVersions(a, b string) int {
	pa, sa := splitVersion(a)
	pb, sb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case sa == sb:
		return 0
	case sa == "":
		return 1
	case sb == "":
		return -1
	case sa < sb:
		return -1
	}
	return 1
}

func splitVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, suffix, _ := strings.Cut(v, "-")
	var out [3]int
	for i, p := range strings.SplitN(core, ".", 3) {
		n, _ := strconv.Atoi(p)
		out[i] = n
	}
	return out, suffix
}

// CanUpgrade reports whether a server at version `from` may install m.
func (m *Manifest) CanUpgrade(from string) error {
	if m.MinUpgradeFrom != "" && CompareVersions(from, m.MinUpgradeFrom) < 0 {
		return fmt.Errorf("version %s cannot upgrade directly to %s: upgrade to %s first", from, m.Version, m.MinUpgradeFrom)
	}
	return nil
}
