package server

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// serverSeat implements seatOps for a real workspace (contract §4.3).
type serverSeat struct {
	s  *Server
	rt *runtime
}

func (o *serverSeat) GitOpInProgress() string {
	return gitshadow.OpInProgress(gitshadow.GitDirOf(o.rt.p.Tree))
}

// Quiesce closes every interactive server-seat session and stops the
// workspace's dev processes (decision Q1).
func (o *serverSeat) Quiesce(reason string) {
	n := o.s.sessions.CloseAll(o.rt.id, reason)
	o.rt.seatMu.Lock()
	stopped := o.s.stopWorkspaceProcesses(o.rt.id, 3*time.Second)
	o.rt.seatMu.Unlock()
	o.s.event(o.rt.id, "server", "", "seat.quiesced", map[string]any{"sessions_closed": n, "processes_stopped": stopped, "reason": reason})
}

func (o *serverSeat) Flush() error {
	_, err := o.s.captureOnce(o.rt)
	return err
}

// BecameFollower: tree/ was flushed as current, so it is the follower's
// base. Later server-side edits show up as drift at the next apply (F14).
func (o *serverSeat) BecameFollower() {
	o.rt.seatMu.Lock()
	defer o.rt.seatMu.Unlock()
	if w, err := o.s.store.WorkspaceByID(o.rt.id); err == nil && w.CurrentCheckpoint != "" {
		o.s.setSeatBase(o.rt, w.CurrentCheckpoint)
	}
}

// BecameHolder brings tree/ to current before the server seat writes again
// (the grant reconcile that P4's "grant keeps a stale tree" mutant breaks).
func (o *serverSeat) BecameHolder() {
	o.rt.seatMu.Lock()
	defer o.rt.seatMu.Unlock()
	if err := o.s.syncSeatLocked(o.rt); err != nil {
		log.Printf("workspace %s: bring server seat to current: %v", o.rt.id, err)
	}
}

// seatBase is the checkpoint tree/ was last captured as or applied to.
func (s *Server) seatBase(rt *runtime) string {
	out, err := rt.seat.Git(nil, "rev-parse", "-q", "--verify", "refs/seat/base")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (s *Server) setSeatBase(rt *runtime, cp string) {
	if _, err := rt.seat.Git(nil, "update-ref", "refs/seat/base", cp); err != nil {
		log.Printf("workspace %s: record seat base: %v", rt.id, err)
	}
}

// syncSeat applies current to tree/ as the workspace user, like any
// follower (§4.3 item 3, §6.5). Local changes on the server seat that never
// became a checkpoint are server drift: quarantined as source=server, then
// overwritten (F14). Called with rt.seatMu held.
func (s *Server) syncSeatLocked(rt *runtime) error {
	w, err := s.store.WorkspaceByID(rt.id)
	if err != nil || w.CurrentCheckpoint == "" {
		return err
	}
	cur, base := w.CurrentCheckpoint, s.seatBase(rt)
	if base == cur {
		return nil
	}
	// checkpoints.git (server) → seat shadow (workspace user): a thin pack
	// against the seat's base, never shared object directories (§2.5, P-1).
	thinBase := base
	if thinBase != "" {
		if _, err := rt.cps.Git(nil, "cat-file", "-e", thinBase+"^{tree}"); err != nil {
			thinBase = ""
		}
	}
	pack, err := rt.cps.PackSince(cur, thinBase)
	if err != nil {
		return err
	}
	if err := rt.seat.ReceivePack(pack, "refs/seat/applied", cur); err != nil {
		return err
	}
	from := base // "" seeds (legacy workspaces without a recorded base)
	if base != "" {
		st, err := s.seatState(rt)
		if err != nil {
			return err
		}
		if prev, err := rt.seat.StateOf(base); err != nil {
			from = ""
		} else if st.Tree != prev.Tree {
			q, err := s.quarantineSeat(rt, st, base)
			if err != nil {
				return fmt.Errorf("server drift found but not quarantined (%v); not overwriting it", err)
			}
			from = q
		}
	}
	meta, err := rt.seat.ReadMeta(cur)
	if err != nil {
		return err
	}
	if meta.HeadRef != "" {
		_, err = rt.seat.UserGit(nil, "symbolic-ref", "HEAD", meta.HeadRef)
	} else if meta.HeadOid != "" {
		_, err = rt.seat.UserGit(nil, "update-ref", "--no-deref", "HEAD", meta.HeadOid)
	}
	if err != nil {
		return fmt.Errorf("point HEAD: %w", err)
	}
	// Apply from the quarantined capture, verified, never a blind seed: an
	// edit made after the quarantine's capture must not be overwritten.
	err = s.seatApply(rt, from, cur)
	for i := 0; i < 5 && err == gitshadow.ErrDiverged; i++ {
		st, cerr := s.seatState(rt)
		if cerr != nil {
			return cerr
		}
		q, cerr := s.quarantineSeat(rt, st, base)
		if cerr != nil {
			return cerr
		}
		err = s.seatApply(rt, q, cur)
	}
	if err != nil {
		return err
	}
	s.setSeatBase(rt, cur)
	return nil
}

// quarantineSeat keeps the server seat's diverged state under
// refs/quarantine/server/<id> in checkpoints.git (I4, F14).
func (s *Server) quarantineSeat(rt *runtime, st gitshadow.State, base string) (string, error) {
	qcp, err := rt.seat.CommitState(st, "refs/seat/quarantine", base, 0)
	if err != nil {
		return "", err
	}
	pack, err := rt.seat.PackSince(qcp, base)
	if err != nil {
		return "", err
	}
	qid := ids.New()
	if err := rt.cps.ReceivePack(pack, "refs/quarantine/server/"+qid, qcp); err != nil {
		return "", err
	}
	if err := s.store.InsertQuarantine(&store.Quarantine{ID: qid, WorkspaceID: rt.id, SourceKind: "server",
		CheckpointID: qcp, BaseCheckpointID: base, Reason: "server_drift", CreatedAt: store.Now()}); err != nil {
		return "", err
	}
	s.event(rt.id, "server", "", "quarantine.created", map[string]string{"id": qid, "reason": "server_drift", "source": "server"})
	log.Printf("workspace %s: server drift kept as quarantine %s", rt.id, qid)
	return qcp, nil
}

// seatState captures the server worktree for drift detection. While a
// device writes, tree/'s HEAD can name a branch the device just deleted
// (the next checkpoint moves it), so the index delta may be unavailable;
// the drift check needs only the files.
func (s *Server) seatState(rt *runtime) (gitshadow.State, error) {
	if st, err := rt.seat.CaptureState(); err == nil {
		return st, nil
	}
	tree, stats, err := rt.seat.CaptureTree()
	if err != nil {
		return gitshadow.State{}, err
	}
	empty := stats.EmptyDirs
	if empty == nil {
		empty = []string{}
	}
	return gitshadow.State{Tree: tree, EmptyDirs: empty, Staged: []gitshadow.StagedEntry{}}, nil
}

// seatApply runs the apply (or, with from == "", a seed) as the workspace
// user through `armageddon hook seat-apply`: apply writes files in a tree
// the workspace user controls, so it must never run as the server (§2.5).
// With the privilege split this becomes a helper operation.
func (s *Server) seatApply(rt *runtime, from, to string) error {
	if from == "" {
		from = "-"
	}
	cmd := rt.acct.Command(rt.p.Tree, s.hookBin, "hook", "seat-apply", rt.seat.GitDir, rt.seat.WorkTree, rt.seat.IndexFile, from, to)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		return gitshadow.ErrDiverged
	}
	if err != nil {
		return fmt.Errorf("seat apply: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopWorkspaceProcesses implements decision Q1 through the helper's
// SignalWorkspace(all): SIGTERM, a grace period, then SIGKILL. The server
// itself has no kill capability over workspace processes (§2.5). Docker
// services are not the workspace user's processes and keep running. It
// reports whether the signals were delivered.
func (s *Server) stopWorkspaceProcesses(wsID string, grace time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), grace+10*time.Second)
	defer cancel()
	if err := s.helper.SignalWorkspace(ctx, wsID, "all", syscall.SIGTERM); err != nil {
		log.Printf("workspace %s: stop processes: %v", wsID, err)
		return false
	}
	time.Sleep(grace)
	if err := s.helper.SignalWorkspace(ctx, wsID, "all", syscall.SIGKILL); err != nil {
		log.Printf("workspace %s: stop processes: %v", wsID, err)
		return false
	}
	return true
}
