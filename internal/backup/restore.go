package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/release"
	"github.com/callmehalpha/Armageddon-/internal/server"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// RestoreOptions configure a restore.
type RestoreOptions struct {
	Backup  string // backup directory
	DataDir string // must be fresh: no database, no workspaces
	// Passphrase decrypts keys.tar.enc when the backup has one. Without it
	// the keys are not restored (a new server identity is generated).
	Passphrase []byte
	Log        io.Writer
	// Helper creates workspace users and directories and runs workspace
	// Git (§2.5). Nil means the in-process development helper (no
	// isolation), which is only right for tests and --dev setups.
	Helper helper.Client
}

func (o *RestoreOptions) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// RestoreReport summarises a restore.
type RestoreReport struct {
	Restored []string          // workspace IDs back in READY
	Failed   map[string]string // workspace ID → reason
	Inexact  []string          // restored, but the seat differs from the checkpoint
}

// ReadManifest loads and checks backup.json and every file's checksum.
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "backup.json"))
	if err != nil {
		return nil, fmt.Errorf("%s is not a complete backup (no backup.json): %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("backup.json: %w", err)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("backup format %d is not supported by this binary (want %d)", m.Format, Format)
	}
	for rel, want := range m.SHA256 {
		got, _, err := release.SHA256File(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("backup file %s: %w", rel, err)
		}
		if got != want {
			return nil, fmt.Errorf("backup file %s is damaged (sha256 %s, expected %s)", rel, got, want)
		}
	}
	return &m, nil
}

// Restore rebuilds a server from a backup onto a fresh data directory.
// Workspaces come back READY with the lease held by the server at a new
// epoch, so writes from devices that held it before are fenced (§4).
func Restore(o RestoreOptions) (*RestoreReport, error) {
	m, err := ReadManifest(o.Backup)
	if err != nil {
		return nil, err
	}
	if m.SchemaVersion > store.SchemaVersion() {
		return nil, fmt.Errorf("the backup has database schema %d, newer than this binary supports (%d): restore with version %s or later", m.SchemaVersion, store.SchemaVersion(), m.ServerVersion)
	}
	dbPath := filepath.Join(o.DataDir, "armageddon.db")
	if _, err := os.Stat(dbPath); err == nil {
		return nil, fmt.Errorf("%s already has a database: restore needs a fresh data directory", o.DataDir)
	}
	if ents, _ := os.ReadDir(filepath.Join(o.DataDir, "workspaces")); len(ents) > 0 {
		return nil, fmt.Errorf("%s already has workspaces: restore needs a fresh data directory", o.DataDir)
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(o.DataDir, 0o755); err != nil {
		return nil, err
	}

	// Configuration: keep a configuration the new install already has
	// (its listen address and TLS mode belong to the new machine).
	cfgPath := config.Path(o.DataDir)
	if _, err := os.Stat(filepath.Join(o.Backup, "server.json")); err == nil {
		if _, err := os.Stat(cfgPath); err == nil {
			copyFile(filepath.Join(o.Backup, "server.json"), cfgPath+".from-backup", 0o600)
			o.logf("kept the existing %s; the backed-up configuration is in %s.from-backup", cfgPath, cfgPath)
		} else {
			old, err := loadConfigFile(filepath.Join(o.Backup, "server.json"))
			if err != nil {
				return nil, err
			}
			// Paths under the old data directory move to the new one.
			for _, p := range []*string{&old.TLSCert, &old.TLSKey} {
				if m.DataDir != "" && strings.HasPrefix(*p, m.DataDir+string(filepath.Separator)) {
					*p = filepath.Join(o.DataDir, strings.TrimPrefix(*p, m.DataDir))
				}
			}
			old.DataDir = o.DataDir
			if err := old.Save(); err != nil {
				return nil, err
			}
			o.logf("configuration restored")
		}
	}
	if m.Keys != "" {
		if len(o.Passphrase) == 0 {
			o.logf("WARNING: the backup includes encrypted keys but no passphrase was given; keys were not restored")
		} else {
			sealed, err := os.ReadFile(filepath.Join(o.Backup, m.Keys))
			if err != nil {
				return nil, err
			}
			plain, err := Decrypt(sealed, o.Passphrase)
			if err != nil {
				return nil, err
			}
			keys := filepath.Join(o.DataDir, "keys")
			if err := os.MkdirAll(keys, 0o700); err != nil {
				return nil, err
			}
			if err := untar(plain, keys); err != nil {
				return nil, err
			}
			o.logf("keys restored")
		}
	}

	if err := copyFile(filepath.Join(o.Backup, "armageddon.db"), dbPath, 0o600); err != nil {
		return nil, err
	}
	cfg, err := config.Load(o.DataDir)
	if err != nil {
		cfg = config.Default(o.DataDir)
	}
	var opts []server.Option
	if o.Helper != nil {
		opts = append(opts, server.WithHelper(o.Helper))
	}
	s, err := server.New(cfg, opts...) // migrates the restored database if this binary is newer
	if err != nil {
		return nil, err
	}
	defer s.Close()
	db := s.Store().DB()
	now := store.Now()
	// The server holds every lease at a new epoch (§4.5 forced takeover).
	if _, err := db.Exec(`UPDATE leases SET holder_kind = 'server', holder_device_id = NULL, epoch = epoch + 1, state = 'held', acquired_at = ?, heartbeat_at = ?`, now, now); err != nil {
		return nil, err
	}
	rep := &RestoreReport{Failed: map[string]string{}}
	for _, bw := range m.Workspaces {
		fail := func(reason string) {
			rep.Failed[bw.ID] = reason
			db.Exec(`UPDATE workspaces SET state = 'failed', state_reason = ?, updated_at = ?, row_version = row_version + 1 WHERE id = ?`, "restore: "+reason, now, bw.ID)
			o.logf("workspace %s (%s): NOT restored: %s", bw.Slug, bw.ID, reason)
		}
		if bw.Skipped != "" {
			fail("not in the backup (" + bw.Skipped + " at backup time)")
			continue
		}
		w, err := s.Store().WorkspaceByID(bw.ID)
		if err != nil {
			fail(err.Error())
			continue
		}
		bundle := func(rel string) string {
			if rel == "" {
				return ""
			}
			return filepath.Join(o.Backup, filepath.FromSlash(rel))
		}
		res, err := s.RestoreWorkspace(w, bundle(bw.RepoBundle), bundle(bw.CheckpointsBundle), bw.HeadRef)
		if err != nil {
			fail(err.Error())
			continue
		}
		if _, err := db.Exec(`UPDATE workspaces SET state = 'ready', state_reason = '', updated_at = ?, row_version = row_version + 1 WHERE id = ?`, now, bw.ID); err != nil {
			return nil, err
		}
		rep.Restored = append(rep.Restored, bw.ID)
		if !res.Exact {
			rep.Inexact = append(rep.Inexact, bw.ID)
			o.logf("workspace %s (%s): restored at checkpoint #%d; the rebuilt worktree differs from it, the server will record a new checkpoint", w.Slug, w.ID, w.CheckpointSeq)
		} else {
			o.logf("workspace %s (%s): restored at checkpoint #%d", w.Slug, w.ID, w.CheckpointSeq)
		}
	}
	if len(rep.Failed) > 0 {
		return rep, errors.New("some workspaces could not be restored (they are marked failed)")
	}
	return rep, nil
}

func loadConfigFile(path string) (*config.Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := config.Default("")
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
