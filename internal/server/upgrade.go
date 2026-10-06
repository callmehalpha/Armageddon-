package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/callmehalpha/Armageddon-/internal/helper"
)

// UpgradeDataDir prepares a data directory written by an MVP server, which
// ran as root, for an unprivileged server: if anything the server must own
// is still root-owned, it asks the helper to hand it over
// (RepairDataOwnership, limited to the data directory). It runs before the
// configuration and database are opened, since an MVP server.json and
// armageddon.db are root-only.
func UpgradeDataDir(ctx context.Context, dataDir string, c helper.Client) error {
	if !c.Isolated() || os.Geteuid() == 0 {
		return nil
	}
	stale := staleOwnership(dataDir)
	if len(stale) == 0 {
		return nil
	}
	log.Printf("data directory %s has %d root-owned entries from a root server (e.g. %s); asking the helper to hand them to this user", dataDir, len(stale), stale[0])
	if err := c.RepairDataOwnership(ctx); err != nil {
		return fmt.Errorf("upgrade data directory ownership: %w", err)
	}
	if left := staleOwnership(dataDir); len(left) > 0 {
		return fmt.Errorf("upgrade data directory ownership: %s is still owned by root", left[0])
	}
	return nil
}

// staleOwnership lists server-side paths that are owned by root: the data
// directory, its top-level entries, and each workspace directory with its
// server-owned entries (checkpoints.git, hooks, ...). Workspace-owned
// entries are not the server's concern.
func staleOwnership(dataDir string) []string {
	var out []string
	rootOwned := func(p string) bool {
		fi, err := os.Lstat(p)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		return ok && st.Uid == 0
	}
	check := func(dir string, entries bool) {
		if rootOwned(dir) {
			out = append(out, dir)
		}
		if !entries {
			return
		}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if p := filepath.Join(dir, e.Name()); rootOwned(p) {
				out = append(out, p)
			}
		}
	}
	check(dataDir, true)
	ws := filepath.Join(dataDir, "workspaces")
	ents, _ := os.ReadDir(ws)
	for _, e := range ents {
		check(filepath.Join(ws, e.Name()), true)
	}
	return out
}
