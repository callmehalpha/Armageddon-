package server

import (
	"fmt"
	"log"
	"strings"
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
	killed := o.rt.acct.KillAll(3 * time.Second)
	o.rt.seatMu.Unlock()
	o.s.event(o.rt.id, "server", "", "seat.quiesced", map[string]any{"sessions_closed": n, "processes_stopped": killed, "reason": reason})
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
	seed := base == ""
	if !seed {
		st, err := rt.seat.CaptureState()
		if err != nil {
			return err
		}
		prev, err := rt.seat.StateOf(base)
		if err != nil {
			seed = true
		} else if st.Tree != prev.Tree {
			if err := s.quarantineSeat(rt, st, base); err != nil {
				return fmt.Errorf("server drift found but not quarantined (%v); not overwriting it", err)
			}
			seed = true
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
	if seed {
		err = rt.seat.Seed(cur)
	} else {
		err = rt.seat.Apply(base, cur, gitshadow.ApplyOptions{})
		if err == gitshadow.ErrDiverged {
			// Changed between the check and the apply.
			st, cerr := rt.seat.CaptureState()
			if cerr == nil {
				cerr = s.quarantineSeat(rt, st, base)
			}
			if cerr != nil {
				return cerr
			}
			err = rt.seat.Seed(cur)
		}
	}
	if err != nil {
		return err
	}
	s.setSeatBase(rt, cur)
	return nil
}

// quarantineSeat keeps the server seat's diverged state under
// refs/quarantine/server/<id> in checkpoints.git (I4, F14).
func (s *Server) quarantineSeat(rt *runtime, st gitshadow.State, base string) error {
	qcp, err := rt.seat.CommitState(st, "refs/seat/quarantine", base, 0)
	if err != nil {
		return err
	}
	pack, err := rt.seat.PackSince(qcp, base)
	if err != nil {
		return err
	}
	qid := ids.New()
	if err := rt.cps.ReceivePack(pack, "refs/quarantine/server/"+qid, qcp); err != nil {
		return err
	}
	if err := s.store.InsertQuarantine(&store.Quarantine{ID: qid, WorkspaceID: rt.id, SourceKind: "server",
		CheckpointID: qcp, BaseCheckpointID: base, Reason: "server_drift", CreatedAt: store.Now()}); err != nil {
		return err
	}
	s.event(rt.id, "server", "", "quarantine.created", map[string]string{"id": qid, "reason": "server_drift", "source": "server"})
	log.Printf("workspace %s: server drift kept as quarantine %s", rt.id, qid)
	return nil
}
