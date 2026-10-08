package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/callmehalpha/Armageddon-/internal/faults"
	"github.com/callmehalpha/Armageddon-/internal/wslock"
	"log"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// ---- server seat capture & authority commit ----

// startCaptureLoop runs the server seat: it captures and commits while the
// server holds the lease, and follows (applies each committed checkpoint)
// while a device holds it (§4.3).
func (s *Server) startCaptureLoop(ctx context.Context, rt *runtime) {
	cctx, cancel := context.WithCancel(ctx)
	rt.ctx, rt.cancel = cctx, cancel
	interval := time.Duration(s.cfg.CaptureIntervalMS) * time.Millisecond
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		failures := 0
		for {
			changed := rt.changed()
			select {
			case <-cctx.Done():
				return
			case <-t.C:
			case <-changed:
			}
			if _, err := s.captureOnce(rt); err != nil {
				failures++
				if failures == 1 || failures%30 == 0 {
					log.Printf("workspace %s: server seat: %v", rt.id, err)
				}
			} else {
				failures = 0
			}
		}
	}()
}

// captureOnce runs one server-seat step. As holder it captures the seat
// and commits a checkpoint if the working state changed, returning the new
// sequence number (0 if unchanged). As a follower it applies current.
func (s *Server) captureOnce(rt *runtime) (int64, error) {
	rt.seatMu.Lock()
	defer rt.seatMu.Unlock()
	w, err := s.store.WorkspaceByID(rt.id)
	if err != nil {
		return 0, err
	}
	if !running(w) {
		return 0, nil
	}
	lease, err := s.store.LeaseOf(nil, rt.id)
	if err != nil {
		return 0, err
	}
	if lease.HolderKind != "server" {
		// A device writes: the server worktree is a follower (§4.3).
		return 0, s.syncSeatLocked(rt)
	}
	if w.State != StateReady {
		// DEGRADED (F8, F10): the server seat's edits stay in tree/ and are
		// captured once the workspace is READY again.
		return 0, nil
	}
	base := s.seatBase(rt)
	if base == "" && w.CurrentCheckpoint != "" {
		// Workspaces from before seat tracking: the server always held the
		// lease, so its tree is at current.
		base = w.CurrentCheckpoint
		s.setSeatBase(rt, base)
	}
	if base != w.CurrentCheckpoint {
		// The lease came back from a device: never capture over a tree
		// that is behind current (I3).
		if err := s.syncSeatLocked(rt); err != nil {
			return 0, fmt.Errorf("bring server seat to current before capturing: %w", err)
		}
	}
	st, err := rt.seat.CaptureState()
	if err != nil {
		return 0, err
	}
	if w.CurrentCheckpoint != "" {
		prev, err := rt.cps.StateOf(w.CurrentCheckpoint)
		if err == nil && prev.Same(st) {
			return 0, nil
		}
	}
	cp, err := rt.seat.CommitState(st, "refs/seat/latest", w.CurrentCheckpoint, int(w.CheckpointSeq+1))
	if err != nil {
		return 0, err
	}
	// Seat shadow (workspace user) → checkpoints.git (server) as a thin pack
	// against current, never by sharing object directories (§2.5, P-1).
	pack, err := rt.seat.PackSince(cp, w.CurrentCheckpoint)
	if err != nil {
		return 0, err
	}
	if err := rt.cps.ReceivePack(pack, "refs/staging/server", cp); err != nil {
		return 0, err
	}
	seq, err := s.commitCheckpoint(rt, lease.Epoch, "server", "", w.CurrentCheckpoint, cp, "auto")
	if err == nil {
		s.setSeatBase(rt, cp)
	}
	return seq, err
}

var (
	ErrLeaseLost      = errors.New("lease_lost")
	ErrParentMismatch = errors.New("parent_mismatch")
	ErrHeadMissing    = errors.New("head_missing")
)

// commitCheckpoint is the authority's commit command (§6.4): epoch check,
// parent CAS, sequence assignment, then ref update and notification. A
// retry of an already committed checkpoint returns its original seq.
func (s *Server) commitCheckpoint(rt *runtime, epoch int64, authorKind, authorDevice, parent, cp, kind string) (int64, error) {
	rt.commitMu.Lock()
	defer rt.commitMu.Unlock()
	// The same lock across processes, so `server backup` can pause commits.
	unlock, err := wslock.Lock(rt.p.Root)
	if err != nil {
		return 0, fmt.Errorf("commit lock: %w", err)
	}
	defer unlock()
	if existing, err := s.store.CheckpointByID(rt.id, cp); err == nil {
		return existing.Seq, nil // idempotent retry (F5)
	}
	if s.checkDisk() {
		return 0, ErrDiskFull // F8: the writer keeps it queued and retries
	}
	meta, err := rt.cps.ReadMeta(cp)
	if err != nil {
		return 0, fmt.Errorf("checkpoint %s unreadable: %w", cp, err)
	}
	tree, err := rt.cps.Git(nil, "rev-parse", cp+":worktree")
	if err != nil {
		return 0, err
	}
	if authorKind == "device" && meta.HeadOid != "" {
		// §6.4 4c: HEAD must already be in repo.git (the writer pushes refs
		// before committing), or followers cannot check it out.
		if err := rt.acct.Command(rt.p.Repo, "git", "cat-file", "-e", meta.HeadOid+"^{commit}").Run(); err != nil {
			return 0, ErrHeadMissing
		}
	}
	faults.Point("commit-before-db") // F6: staged, not acknowledged
	var seq int64
	err = s.store.Tx(context.Background(), func(tx *sql.Tx) error {
		if existing, err := s.store.CheckpointByIDTx(tx, rt.id, cp); err == nil {
			seq = existing.Seq
			return nil
		}
		now := store.Now()
		// 4a: holder and epoch, as a conditional update (§4.1).
		ok, err := s.store.CheckHolder(tx, rt.id, epoch, authorKind, authorDevice, now)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLeaseLost
		}
		var cur sql.NullString
		var curSeq int64
		if err := tx.QueryRow(`SELECT current_checkpoint_id, checkpoint_seq FROM workspaces WHERE id = ?`, rt.id).Scan(&cur, &curSeq); err != nil {
			return err
		}
		if cur.String != parent {
			return ErrParentMismatch
		}
		seq = curSeq + 1
		if err := s.store.InsertCheckpoint(tx, &store.Checkpoint{ID: cp, WorkspaceID: rt.id, Seq: seq, Epoch: epoch, ParentID: parent,
			AuthorKind: authorKind, AuthorDevice: authorDevice, HeadRef: meta.HeadRef, HeadOid: meta.HeadOid,
			WorktreeTree: strings.TrimSpace(string(tree)), Kind: kind, CreatedAt: now}); err != nil {
			return err
		}
		// 4d: compare-and-swap on current.
		ok, err = s.store.AdvanceCurrent(tx, rt.id, parent, cp, seq, now)
		if err != nil {
			return err
		}
		if !ok {
			return ErrParentMismatch
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	faults.Point("commit-after-db") // F6: in the database, refs not updated, no reply sent
	rt.cps.Git(nil, "update-ref", fmt.Sprintf("refs/checkpoints/%d", seq), cp)
	rt.cps.Git(nil, "update-ref", "refs/checkpoints/current", cp)
	if authorKind == "device" {
		rt.cps.Git(nil, "update-ref", "-d", "refs/staging/"+authorDevice+"/"+cp)
	}
	rt.broadcast()
	return seq, nil
}
