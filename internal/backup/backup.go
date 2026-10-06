// Package backup implements `armageddon server backup` and `server restore`
// (contract §9.3, plan M5.5).
//
// A backup is a directory:
//
//	armageddon-backup-<UTC timestamp>/
//	├── backup.json                  what is inside, with a sha256 per file
//	├── armageddon.db                VACUUM INTO snapshot (the source of truth)
//	├── server.json                  configuration
//	├── keys.tar.enc                 optional: keys/, encrypted with a passphrase
//	└── workspaces/<id>/
//	    ├── repo.bundle              git bundle --all of repo.git (branches, tags, trash refs)
//	    └── checkpoints.bundle       git bundle --all of checkpoints.git
//
// Consistency. The database snapshot is taken first. Each workspace is then
// quiesced on its own: the backup takes the workspace's commit lock (package
// wslock), which pauses checkpoint commits for that workspace, and bundles
// both repositories while holding it. A checkpoint committed between the
// database snapshot and the bundle is in the bundle but not in the
// database; restore drops it, because the database is truth (§8.3) and
// writers retry anything unacknowledged. Every object a snapshotted
// checkpoint references is in the bundles, since bundles are taken after
// the snapshot and checkpoints and trash refs are never rewritten.
package backup

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/release"
	"github.com/callmehalpha/Armageddon-/internal/wsgit"
	"github.com/callmehalpha/Armageddon-/internal/wslock"

	_ "modernc.org/sqlite"
)

const Format = 1

// Workspace is one workspace in a backup.
type Workspace struct {
	ID                string `json:"id"`
	Slug              string `json:"slug"`
	State             string `json:"state"`
	OSUser            string `json:"os_user"`
	CurrentCheckpoint string `json:"current_checkpoint"`
	CheckpointSeq     int64  `json:"checkpoint_seq"`
	HeadRef           string `json:"head_ref,omitempty"` // repo.git HEAD
	RepoBundle        string `json:"repo_bundle,omitempty"`
	CheckpointsBundle string `json:"checkpoints_bundle,omitempty"`
	Skipped           string `json:"skipped,omitempty"`
}

// Manifest is backup.json.
type Manifest struct {
	Format        int               `json:"format"`
	Created       string            `json:"created"`
	ServerVersion string            `json:"server_version"`
	SchemaVersion int               `json:"schema_version"`
	DataDir       string            `json:"data_dir"`
	Keys          string            `json:"keys,omitempty"`
	Workspaces    []Workspace       `json:"workspaces"`
	SHA256        map[string]string `json:"sha256"`
}

// Options configure a backup.
type Options struct {
	DataDir string
	// To is the directory the backup directory is created in; default
	// <data>/backups.
	To string
	// Passphrase, when set, includes keys/ encrypted with it.
	Passphrase []byte
	// LockTimeout bounds how long to wait for a workspace's commit lock.
	LockTimeout   time.Duration
	ServerVersion string
	// WorkspaceGit builds Git commands that run as a workspace's OS user
	// (§2.5). Default: wsgit.New(DataDir, HelperSocket).
	WorkspaceGit wsgit.Func
	// HelperSocket is the privileged helper's socket (default
	// /run/armageddon/helper.sock).
	HelperSocket string
	Log          io.Writer
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// Run writes a backup and returns its directory.
func Run(o Options) (string, error) {
	if o.To == "" {
		o.To = filepath.Join(o.DataDir, "backups")
	}
	if o.LockTimeout == 0 {
		o.LockTimeout = 2 * time.Minute
	}
	if o.WorkspaceGit == nil {
		o.WorkspaceGit = wsgit.New(o.DataDir, o.HelperSocket)
	}
	dbPath := filepath.Join(o.DataDir, "armageddon.db")
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("no database at %s: %w", dbPath, err)
	}
	now := time.Now().UTC()
	out := filepath.Join(o.To, "armageddon-backup-"+now.Format("20060102T150405Z"))
	if err := os.MkdirAll(o.To, 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(out)
		}
	}()
	m := &Manifest{Format: Format, Created: now.Format(time.RFC3339), ServerVersion: o.ServerVersion,
		DataDir: o.DataDir, SHA256: map[string]string{}, Workspaces: []Workspace{}}

	// 1. The database, first: it decides what the backup contains.
	if err := VacuumInto(dbPath, filepath.Join(out, "armageddon.db")); err != nil {
		return "", fmt.Errorf("database snapshot: %w", err)
	}
	o.logf("database snapshot written")
	snap, err := openDB(filepath.Join(out, "armageddon.db"))
	if err != nil {
		return "", err
	}
	m.SchemaVersion, _ = appliedSchema(snap)
	wss, err := listWorkspaces(snap)
	snap.Close()
	if err != nil {
		return "", err
	}

	// 2. Each workspace, quiesced briefly.
	for _, w := range wss {
		if w.State != "ready" && w.State != "degraded" {
			w.Skipped = "state " + w.State
			m.Workspaces = append(m.Workspaces, w)
			o.logf("workspace %s (%s): skipped, %s", w.Slug, w.ID, w.Skipped)
			continue
		}
		if err := backupWorkspace(&o, out, &w); err != nil {
			return "", fmt.Errorf("workspace %s (%s): %w", w.Slug, w.ID, err)
		}
		m.Workspaces = append(m.Workspaces, w)
		o.logf("workspace %s (%s): checkpoint #%d", w.Slug, w.ID, w.CheckpointSeq)
	}

	// 3. Configuration and, optionally, keys.
	if err := copyFile(config.Path(o.DataDir), filepath.Join(out, "server.json"), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if len(o.Passphrase) > 0 {
		keysDir := filepath.Join(o.DataDir, "keys")
		if _, err := os.Stat(keysDir); err == nil {
			plain, err := tarDir(keysDir)
			if err != nil {
				return "", err
			}
			enc, err := Encrypt(plain, o.Passphrase)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(out, "keys.tar.enc"), enc, 0o600); err != nil {
				return "", err
			}
			m.Keys = "keys.tar.enc"
			o.logf("keys included (encrypted)")
		} else {
			o.logf("no keys directory; nothing to include")
		}
	}

	// 4. Checksums and the manifest, last: a backup without backup.json is
	// incomplete.
	err = filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		sum, _, err := release.SHA256File(p)
		m.SHA256[filepath.ToSlash(rel)] = sum
		return err
	})
	if err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "backup.json"), append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	ok = true
	return out, nil
}

func backupWorkspace(o *Options, out string, w *Workspace) error {
	root := filepath.Join(o.DataDir, "workspaces", w.ID)
	repo, cps := filepath.Join(root, "repo.git"), filepath.Join(root, "checkpoints.git")
	name := w.OSUser
	if name == "" {
		name = helper.UserName(w.ID)
	}
	if _, err := o.WorkspaceGit(w.ID, name, repo, "--version"); err != nil {
		return fmt.Errorf("workspace user: %w", err)
	}
	dir := filepath.Join(out, "workspaces", w.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := wslock.TryLock(root, o.LockTimeout)
	if err != nil {
		return fmt.Errorf("pausing commits: %w", err)
	}
	defer unlock()
	// repo.git is workspace-writable: Git runs as the workspace user (§2.5).
	asWS := func(args ...string) *exec.Cmd {
		c, _ := o.WorkspaceGit(w.ID, name, repo, args...)
		c.Env = append(c.Env, "GIT_DIR="+repo, "GIT_CONFIG_NOSYSTEM=1")
		return c
	}
	if head, err := asWS("symbolic-ref", "-q", "HEAD").Output(); err == nil {
		w.HeadRef = strings.TrimSpace(string(head))
	}
	if err := bundle(asWS, filepath.Join(dir, "repo.bundle")); err != nil {
		return fmt.Errorf("repo.git: %w", err)
	}
	if fileExists(filepath.Join(dir, "repo.bundle")) {
		w.RepoBundle = "workspaces/" + w.ID + "/repo.bundle"
	}
	// checkpoints.git belongs to the server and is never workspace-accessible.
	asServer := func(args ...string) *exec.Cmd {
		c := exec.Command("git", append([]string{"--git-dir=" + cps}, args...)...)
		c.Env = append(cleanEnv(), "GIT_CONFIG_NOSYSTEM=1")
		return c
	}
	if err := bundle(asServer, filepath.Join(dir, "checkpoints.bundle")); err != nil {
		return fmt.Errorf("checkpoints.git: %w", err)
	}
	if fileExists(filepath.Join(dir, "checkpoints.bundle")) {
		w.CheckpointsBundle = "workspaces/" + w.ID + "/checkpoints.bundle"
	}
	return nil
}

// bundle writes `git bundle create - --all` to dst. The bundle goes to
// stdout so the file is created by this process, not by the workspace user.
// A repository without refs gets no bundle.
func bundle(git func(...string) *exec.Cmd, dst string) error {
	refs, err := git("for-each-ref", "--count=1", "--format=%(refname)").Output()
	if err != nil {
		return fmt.Errorf("list refs: %w", err)
	}
	if strings.TrimSpace(string(refs)) == "" {
		return nil
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := git("bundle", "create", "-q", "-", "--all")
	cmd.Stdout = f
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("git bundle: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// VacuumInto writes a consistent copy of a live SQLite database.
func VacuumInto(src, dst string) error {
	db, err := openDB(src)
	if err != nil {
		return err
	}
	defer db.Close()
	os.Remove(dst)
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, db.Ping()
}

func appliedSchema(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func listWorkspaces(db *sql.DB) ([]Workspace, error) {
	rows, err := db.Query(`SELECT id, slug, state, os_user, COALESCE(current_checkpoint_id, ''), checkpoint_seq FROM workspaces WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Slug, &w.State, &w.OSUser, &w.CurrentCheckpoint, &w.CheckpointSeq); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- keys: tar + scrypt + XChaCha20-Poly1305 ----

const encMagic = "ARMKEYS1"

// Encrypt seals plaintext with a key derived from passphrase by scrypt
// (N=2^15, r=8, p=1). Layout: magic | salt(16) | nonce(24) | ciphertext.
func Encrypt(plaintext, passphrase []byte) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := keyAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(encMagic), salt...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(encMagic)), nil
}

// ErrPassphrase means the keys archive could not be decrypted.
var ErrPassphrase = errors.New("wrong passphrase, or the keys archive is damaged")

// Decrypt opens what Encrypt sealed.
func Decrypt(sealed, passphrase []byte) ([]byte, error) {
	hdr := len(encMagic) + 16 + chacha20poly1305.NonceSizeX
	if len(sealed) < hdr || string(sealed[:len(encMagic)]) != encMagic {
		return nil, errors.New("not an Armageddon keys archive")
	}
	salt := sealed[len(encMagic) : len(encMagic)+16]
	nonce := sealed[len(encMagic)+16 : hdr]
	aead, err := keyAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, sealed[hdr:], []byte(encMagic))
	if err != nil {
		return nil, ErrPassphrase
	}
	return plain, nil
}

func keyAEAD(passphrase, salt []byte) (interface {
	NonceSize() int
	Seal(dst, nonce, plaintext, ad []byte) []byte
	Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
}, error) {
	key, err := scrypt.Key(passphrase, salt, 1<<15, 8, 1, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(key)
}

func tarDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var paths []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." || !(fi.Mode().IsRegular() || fi.IsDir()) {
			continue
		}
		hdr, _ := tar.FileInfoHeader(fi, "")
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			tw.Write(b)
		}
	}
	return buf.Bytes(), tw.Close()
}

func untar(data []byte, dir string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q in keys archive", hdr.Name)
		}
		p := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, b, os.FileMode(hdr.Mode).Perm()&0o700); err != nil {
				return err
			}
		}
	}
}
