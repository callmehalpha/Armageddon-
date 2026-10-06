package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/backup"
	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/tlsedge"
)

// Updater runs `server update` and `server rollback` (contract §9.3, F16).
type Updater struct {
	Layout   Layout
	Services Services
	Source   *Source
	// Migrate applies the new binary's migrations to the data directory.
	// Default: `<binary> server migrate --data <dir>`, as the database owner.
	Migrate func(binary, dataDir string) error
	// Healthy polls the restarted server. Default: GET /healthz until 200
	// or HealthTimeout (60 s).
	Healthy       func(ctx context.Context) error
	HealthTimeout time.Duration
	Log           io.Writer
}

func (u *Updater) logf(format string, args ...any) {
	if u.Log != nil {
		fmt.Fprintf(u.Log, format+"\n", args...)
	}
}

func (u *Updater) dbPath() string { return filepath.Join(u.Layout.DataDir, "armageddon.db") }

// ErrRolledBack wraps an update failure after which the previous version
// and database were put back successfully.
var ErrRolledBack = errors.New("update failed and was rolled back")

// Update installs a new version: stage and verify → VACUUM INTO backup →
// stop → switch `current` → migrate → start → health check. If any step
// after the stop fails, the previous binary and the pre-migration
// database are put back and the old version is started again.
func (u *Updater) Update(ctx context.Context) (*Step, error) {
	from, err := u.Layout.Current()
	if err != nil {
		return nil, err
	}
	to, err := u.Source.Stage(u.Layout, from, u.logf)
	if err != nil {
		return nil, err
	}
	if to == from {
		u.logf("already running %s", to)
		return nil, nil
	}
	step := &Step{From: from, To: to, At: now()}
	if _, err := os.Stat(u.dbPath()); err == nil {
		dir := filepath.Join(u.Layout.DataDir, "backups")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		step.DBBackup = filepath.Join(dir, fmt.Sprintf("pre-update-%s-to-%s-%s.db", from, to, time.Now().UTC().Format("20060102T150405Z")))
		if err := backup.VacuumInto(u.dbPath(), step.DBBackup); err != nil {
			return nil, fmt.Errorf("database backup before update: %w", err)
		}
		u.logf("database backed up to %s", step.DBBackup)
	}
	u.logf("stopping %s", from)
	if err := u.Services.Stop(); err != nil {
		return nil, fmt.Errorf("stop: %w", err)
	}
	if err := u.apply(ctx, to); err != nil {
		u.logf("update to %s failed: %v", to, err)
		if rerr := u.revert(ctx, from, step.DBBackup); rerr != nil {
			return nil, fmt.Errorf("update to %s failed (%v) AND the automatic rollback failed: %v. "+
				"Restore by hand: point %s at versions/%s, copy %s over %s, start the services", to, err, rerr,
				u.Layout.CurrentLink(), from, step.DBBackup, u.dbPath())
		}
		return nil, fmt.Errorf("%w: %v (running %s again with the pre-update database)", ErrRolledBack, err, from)
	}
	st, err := u.Layout.loadState()
	if err == nil {
		st.History = append(st.History, *step)
		err = u.Layout.saveState(st)
	}
	if err != nil {
		u.logf("WARNING: could not record the update for rollback: %v", err)
	}
	u.logf("now running %s", to)
	return step, nil
}

func (u *Updater) apply(ctx context.Context, to string) error {
	if err := u.Layout.Switch(to); err != nil {
		return fmt.Errorf("switch: %w", err)
	}
	u.logf("migrating the database with %s", to)
	if err := u.migrate(u.Layout.Binary(to)); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	u.logf("starting %s", to)
	if err := u.Services.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := u.healthy(ctx); err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	return nil
}

// revert puts the previous version and database back and starts it.
func (u *Updater) revert(ctx context.Context, from, dbBackup string) error {
	u.logf("rolling back to %s", from)
	u.Services.Stop() // may already be stopped
	if err := u.Layout.Switch(from); err != nil {
		return err
	}
	if dbBackup != "" {
		if err := RestoreDB(dbBackup, u.dbPath()); err != nil {
			return fmt.Errorf("restore database: %w", err)
		}
		u.logf("database restored from %s", dbBackup)
	}
	if err := u.Services.Start(); err != nil {
		return err
	}
	return u.healthy(ctx)
}

// Rollback returns to the version before the last update, with the
// database backup taken before it (contract §9.3). Changes made since that
// update are lost unless keepDB is set (only safe when the update did not
// change the schema).
func (u *Updater) Rollback(ctx context.Context, keepDB bool) (*Step, error) {
	cur, err := u.Layout.Current()
	if err != nil {
		return nil, err
	}
	st, err := u.Layout.loadState()
	if err != nil {
		return nil, err
	}
	if len(st.History) == 0 {
		return nil, errors.New("no recorded update to roll back")
	}
	last := st.History[len(st.History)-1]
	if last.To != cur {
		return nil, fmt.Errorf("the last recorded update was %s → %s but %s is current; roll back by hand", last.From, last.To, cur)
	}
	db := last.DBBackup
	if keepDB {
		db = ""
	}
	if err := u.Services.Stop(); err != nil {
		return nil, fmt.Errorf("stop: %w", err)
	}
	if err := u.revert(ctx, last.From, db); err != nil {
		return nil, err
	}
	st.History = st.History[:len(st.History)-1]
	if err := u.Layout.saveState(st); err != nil {
		u.logf("WARNING: could not update the update history: %v", err)
	}
	return &last, nil
}

func (u *Updater) migrate(bin string) error {
	if u.Migrate != nil {
		return u.Migrate(bin, u.Layout.DataDir)
	}
	cmd := exec.Command(bin, "server", "migrate", "--data", u.Layout.DataDir)
	// Run as the database's owner (the armageddon user in the helper
	// layout), so new WAL files are not left owned by root.
	if fi, err := os.Stat(u.dbPath()); err == nil && os.Geteuid() == 0 {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: st.Uid, Gid: st.Gid}}
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (u *Updater) healthy(ctx context.Context) error {
	if u.Healthy != nil {
		return u.Healthy(ctx)
	}
	timeout := u.HealthTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	cfg, err := config.Load(u.Layout.DataDir)
	if err != nil {
		return err
	}
	return WaitHealthy(ctx, cfg, timeout)
}

// WaitHealthy polls the server's /healthz until it answers 200.
func WaitHealthy(ctx context.Context, cfg *config.Server, timeout time.Duration) error {
	url, sn := tlsedge.HealthURL(cfg)
	hc := tlsedge.HealthClient(sn, 5*time.Second)
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		resp, err := hc.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			err = fmt.Errorf("%s: %s", url, resp.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("no healthy answer from %s within %s: %v", url, timeout, last)
}

// RestoreDB replaces the database with a backup copy, dropping the WAL and
// shared-memory files of the replaced database, and keeps its owner.
func RestoreDB(backupPath, dbPath string) error {
	uid, gid := -1, -1
	if fi, err := os.Stat(dbPath); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dbPath + ".restore.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	out.Close()
	if uid >= 0 && os.Geteuid() == 0 {
		os.Chown(tmp, uid, gid)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(tmp, dbPath)
}
