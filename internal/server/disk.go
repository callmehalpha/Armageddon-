package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"syscall"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Disk full (contract §10 F8). Below the free-space floor every READY
// workspace goes DEGRADED with reason disk_full: checkpoint uploads,
// commits and pushes are refused with code disk_full, writers keep their
// queue and retry, and reads, follows and lease traffic keep working.
// Nothing is dropped. Once space is freed, `armageddon doctor --repair`
// returns the workspaces to READY (it re-checks the disk first).

// ReasonDiskFull is the state_reason of a workspace degraded by F8.
const ReasonDiskFull = "disk_full"

// ErrDiskFull is returned by writes refused under F8.
var ErrDiskFull = errors.New(ReasonDiskFull)

// statfsFree is replaced by tests.
var statfsFree = func(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// diskLow reports whether the data directory's filesystem is below the
// free-space floor, with a description for logs and events.
func (s *Server) diskLow() (bool, string) {
	free, total, err := statfsFree(s.cfg.DataDir)
	if err != nil || total == 0 {
		return false, ""
	}
	pct := 100 * float64(free) / float64(total)
	if pct >= s.cfg.MinFreePercent() {
		return false, ""
	}
	return true, fmt.Sprintf("%.1f%% free on the filesystem holding %s (floor %.1f%%)", pct, s.cfg.DataDir, s.cfg.MinFreePercent())
}

// checkDisk degrades every READY workspace when the disk is below the
// floor, and reports whether it is. The janitor calls it periodically and
// every write path calls it before storing anything.
func (s *Server) checkDisk() bool {
	low, why := s.diskLow()
	if !low {
		return false
	}
	ws, err := s.store.AllWorkspaces()
	if err != nil {
		return true
	}
	for _, w := range ws {
		if w.State != StateReady {
			continue
		}
		if err := s.store.SetWorkspaceState(w.ID, StateReady, StateDegraded, ReasonDiskFull, store.Now()); err == nil {
			log.Printf("workspace %s: DEGRADED (disk_full): %s; commits are refused until space is freed and `armageddon doctor --repair` runs", w.ID, why)
			s.event(w.ID, "server", "", "workspace.degraded", map[string]string{"reason": ReasonDiskFull, "detail": why})
		}
	}
	return true
}

// diskFull reports whether the disk is below the floor (degrading every
// READY workspace if it just went there), or w is degraded by F8.
func (s *Server) diskFull(w *store.Workspace) bool {
	if w.State == StateDegraded && strings.HasPrefix(w.StateReason, ReasonDiskFull) {
		return true
	}
	return s.checkDisk()
}

// refuseIfDiskFull writes the F8 rejection and returns true when the disk
// is full. Recovery writes (repair from a device) only check this.
func (s *Server) refuseIfDiskFull(rw http.ResponseWriter, w *store.Workspace) bool {
	if !s.diskFull(w) {
		return false
	}
	writeJSON(rw, http.StatusInsufficientStorage, map[string]string{"code": ReasonDiskFull,
		"error": "the server's disk is almost full: checkpoints are refused (nothing is lost; this device keeps its changes queued and retries). The server's administrator must free space and run `armageddon doctor --repair`."})
	return true
}

// writesRefused reports why commits and ref updates to w are refused
// (contract §3.1: a DEGRADED workspace is readable, not writable), or "".
func (s *Server) writesRefused(w *store.Workspace) string {
	if s.diskFull(w) {
		return ReasonDiskFull
	}
	if w.State == StateDegraded {
		reason, _, _ := strings.Cut(w.StateReason, ":")
		return reason
	}
	return ""
}

// refuseWrites writes the rejection for a DEGRADED workspace (or a full
// disk) and returns true when writes to w are refused.
func (s *Server) refuseWrites(rw http.ResponseWriter, w *store.Workspace) bool {
	switch reason := s.writesRefused(w); reason {
	case "":
		return false
	case ReasonDiskFull:
		return s.refuseIfDiskFull(rw, w)
	default:
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"code": reason,
			"error": "the workspace is DEGRADED (" + w.StateReason + "): it is readable but refuses writes until it is repaired. Nothing is lost; this device keeps its changes queued and retries."})
		return true
	}
}

// running reports whether a workspace has a runtime: READY, or DEGRADED
// (still readable, followable and leasable; writes may be refused).
func running(w *store.Workspace) bool {
	return w.State == StateReady || w.State == StateDegraded
}
