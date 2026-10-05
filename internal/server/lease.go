package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// The lease authority (contract §3.2, §4.1–§4.5, P-7/P-8/P-9).
//
//	HELD(h, e) ──acquire(R)──▶ HANDOFF(h → R, e) ──flush+release──▶ HELD(R, e+1)
//	HANDOFF ──refused by holder (Git op in progress)──▶ HELD(h, e)
//	HANDOFF ──holder unresponsive > T_handoff──▶ STALE(h, e)
//	HELD(device) ──no heartbeat > T_stale──▶ STALE ──heartbeat──▶ HELD
//	HELD / HANDOFF / STALE ──force(R)──▶ HELD(R, e+1)
//
// Every transition is a conditional update on the epoch it starts from
// (store/lease.go); the lease commands of one workspace are serialised by
// rt.leaseMu, and a change of holder also takes rt.fence as a writer so it
// waits for in-flight pushes (§4.2). Nothing transfers automatically
// (decision Q2): STALE is informational.

// Seat is a lease holder: the server seat or one device.
type Seat struct{ Kind, Device string }

var ServerSeat = Seat{Kind: "server"}

func DeviceSeat(id string) Seat { return Seat{Kind: "device", Device: id} }

func (s Seat) String() string {
	if s.Kind == "device" {
		return "device:" + s.Device
	}
	return "server"
}

func parseSeat(v string) Seat {
	if id, ok := strings.CutPrefix(v, "device:"); ok {
		return DeviceSeat(id)
	}
	return ServerSeat
}

func holderOf(l *store.Lease) Seat {
	if l.HolderKind == "device" {
		return DeviceSeat(l.HolderDevice)
	}
	return ServerSeat
}

// LeaseError is a refused lease or commit command. It is answered with 409
// and echoes the epoch of the attempt (P-8), plus the lease state (P-7).
type LeaseError struct {
	Code           string // lease_lost, parent_mismatch, handoff_in_progress, holder_unresponsive, git_op_in_progress, not_in_handoff, superseded, head_missing, conflict
	Msg            string
	AttemptedEpoch int64
}

func (e *LeaseError) Error() string {
	if e.Msg == "" {
		return e.Code
	}
	return e.Code + ": " + e.Msg
}

// seatOps are the server seat's side of a handoff, in-process (§4.3). They
// are an interface so the transitions can be tested without Git.
type seatOps interface {
	// GitOpInProgress names a merge/rebase/... in progress in tree/, or "".
	GitOpInProgress() string
	// Quiesce closes interactive sessions and stops dev processes (Q1).
	Quiesce(reason string)
	// Flush captures and commits the server seat under the current epoch.
	Flush() error
	// BecameFollower: a device now writes; tree/ follows from current.
	BecameFollower()
	// BecameHolder: the lease returned to the server; bring tree/ to current.
	BecameHolder()
}

type transition struct {
	epoch int64
	kind  string // granted | forced
}

func (s *Server) now() int64 {
	if s.clock != nil {
		return s.clock()
	}
	return store.Now()
}

// effectiveState is the lease state as displayed: a device holder whose
// heartbeat is older than T_stale is STALE even before the sweeper writes it.
func (s *Server) effectiveState(l *store.Lease) string {
	if l.State == "held" && l.HolderKind == "device" && s.now()-l.HeartbeatAt > s.cfg.TStale().Milliseconds() {
		return "stale"
	}
	return l.State
}

func (s *Server) seatName(seat Seat) string {
	if seat.Kind == "server" {
		return "the server seat"
	}
	if d, err := s.store.DeviceByID(seat.Device); err == nil {
		return d.Name
	}
	return "device " + seat.Device
}

// leaseJSON is the lease part of every resync, heartbeat, grant and
// rejection reply (P-7).
func (s *Server) leaseJSON(l *store.Lease, caller *store.Device) map[string]any {
	h := holderOf(l)
	out := map[string]any{"holder": h.String(), "holder_kind": l.HolderKind, "holder_device": l.HolderDevice,
		"holder_name": s.seatName(h), "epoch": l.Epoch, "state": s.effectiveState(l), "handoff_to": l.HandoffTo,
		"heartbeat_at": l.HeartbeatAt, "acquired_at": l.AcquiredAt, "now": s.now(), "stale_after_ms": s.cfg.TStale().Milliseconds(),
		"you": caller != nil && l.HolderKind == "device" && l.HolderDevice == caller.ID}
	if l.HandoffTo != "" {
		out["handoff_to_name"] = s.seatName(parseSeat(l.HandoffTo))
	}
	return out
}

// leaseReply is (holder, epoch, current) for one workspace.
func (s *Server) leaseReply(wsID string, caller *store.Device) (map[string]any, error) {
	w, err := s.store.WorkspaceByID(wsID)
	if err != nil {
		return nil, err
	}
	l, err := s.store.LeaseOf(nil, wsID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"seq": w.CheckpointSeq, "id": w.CurrentCheckpoint, "state": w.State, "lease": s.leaseJSON(l, caller)}
	if w.CurrentCheckpoint != "" {
		if cp, err := s.store.CheckpointByID(wsID, w.CurrentCheckpoint); err == nil {
			out["checkpoint_at"] = cp.CreatedAt
		}
	}
	return out, nil
}

func (rt *runtime) setHandoffFailure(epoch int64, err *LeaseError) {
	rt.leaseStateMu.Lock()
	defer rt.leaseStateMu.Unlock()
	if rt.handoffFailures == nil {
		rt.handoffFailures = map[int64]*LeaseError{}
	}
	rt.handoffFailures[epoch] = err
}

func (rt *runtime) handoffFailure(epoch int64) *LeaseError {
	rt.leaseStateMu.Lock()
	defer rt.leaseStateMu.Unlock()
	return rt.handoffFailures[epoch]
}

func (rt *runtime) lastTransition() transition {
	rt.leaseStateMu.Lock()
	defer rt.leaseStateMu.Unlock()
	return rt.last
}

// AcquireLease moves the lease to `to` gracefully (§4.4). It returns once
// `to` holds the lease, or with a *LeaseError. The server seat hands off
// in-process; a holding device is asked to flush and release through its
// long poll and has T_handoff to do so.
func (s *Server) AcquireLease(ctx context.Context, rt *runtime, to Seat, actorKind, actorID string) error {
	rt.leaseMu.Lock()
	l, err := s.store.LeaseOf(nil, rt.id)
	if err != nil {
		rt.leaseMu.Unlock()
		return err
	}
	switch {
	case holderOf(l) == to && l.State != "handoff":
		rt.leaseMu.Unlock()
		return nil // already the holder: the reply re-sends the grant
	case l.State == "handoff" && l.HandoffTo == to.String():
		rt.leaseMu.Unlock()
		return s.waitHandoff(ctx, rt, l.Epoch, to)
	case l.State == "handoff":
		rt.leaseMu.Unlock()
		return &LeaseError{Code: "handoff_in_progress", Msg: fmt.Sprintf("%s is already handing the workspace to %s", s.seatName(holderOf(l)), s.seatName(parseSeat(l.HandoffTo)))}
	case s.effectiveState(l) == "stale":
		rt.leaseMu.Unlock()
		return s.unresponsive(rt, l)
	}
	deadline := s.now() + s.cfg.THandoff().Milliseconds()
	ok, err := s.store.BeginHandoff(nil, rt.id, l.Epoch, to.String(), deadline)
	if err != nil || !ok {
		rt.leaseMu.Unlock()
		if err == nil {
			err = &LeaseError{Code: "conflict", Msg: "the lease changed concurrently; retry"}
		}
		return err
	}
	s.event(rt.id, actorKind, actorID, "lease.handoff", map[string]any{"from": holderOf(l).String(), "to": to.String(), "epoch": l.Epoch})
	rt.broadcast()
	if l.HolderKind == "server" {
		defer rt.leaseMu.Unlock()
		return s.serverHandoff(rt, l, to)
	}
	go s.handoffTimer(rt, l.Epoch, deadline)
	rt.leaseMu.Unlock()
	return s.waitHandoff(ctx, rt, l.Epoch, to)
}

// serverHandoff hands the server seat's lease to a device, in-process
// (§4.3, §4.4). Called with rt.leaseMu held and the lease in HANDOFF.
func (s *Server) serverHandoff(rt *runtime, l *store.Lease, to Seat) error {
	if op := rt.ops.GitOpInProgress(); op != "" {
		s.store.EndHandoff(nil, rt.id, l.Epoch, "held")
		rt.broadcast()
		return &LeaseError{Code: "git_op_in_progress", Msg: fmt.Sprintf("a %s is in progress on the server seat; finish or abort it there, then retry (or force the takeover: sequencer state is not transferred)", op)}
	}
	// Close sessions and stop dev processes first, so nothing on the server
	// seat edits the tree after the final capture (Q1).
	rt.ops.Quiesce(fmt.Sprintf("%s took over this workspace; the server seat is read-only until it is handed back", s.seatName(to)))
	if err := rt.ops.Flush(); err != nil {
		s.store.EndHandoff(nil, rt.id, l.Epoch, "held")
		rt.broadcast()
		return fmt.Errorf("server seat flush failed, handoff cancelled: %w", err)
	}
	return s.transfer(rt, l, to, "granted", "handoff")
}

// transfer is HANDOFF/HELD/STALE(h, e) → HELD(to, e+1), under the fence.
// Called with rt.leaseMu held.
func (s *Server) transfer(rt *runtime, l *store.Lease, to Seat, kind string, from ...string) error {
	rt.fence.Lock()
	ok, err := s.store.TransferLease(nil, rt.id, l.Epoch, to.Kind, to.Device, s.now(), from...)
	rt.fence.Unlock()
	if err != nil {
		return err
	}
	if !ok {
		return &LeaseError{Code: "conflict", Msg: "the lease changed concurrently; retry"}
	}
	rt.leaseStateMu.Lock()
	rt.last = transition{epoch: l.Epoch + 1, kind: kind}
	rt.leaseStateMu.Unlock()
	prev := holderOf(l)
	if kind == "granted" {
		s.event(rt.id, "server", "", "lease.granted", map[string]any{"from": prev.String(), "to": to.String(), "epoch": l.Epoch + 1})
	}
	if to.Kind == "server" && prev.Kind != "server" {
		rt.ops.BecameHolder()
	} else if prev.Kind == "server" && to.Kind != "server" {
		rt.ops.BecameFollower()
	}
	rt.broadcast()
	return nil
}

// waitHandoff blocks until the handoff started at epoch ends.
func (s *Server) waitHandoff(ctx context.Context, rt *runtime, epoch int64, to Seat) error {
	for {
		ch := rt.changed()
		l, err := s.store.LeaseOf(nil, rt.id)
		if err != nil {
			return err
		}
		switch {
		case l.Epoch > epoch && holderOf(l) == to:
			return nil
		case l.Epoch > epoch:
			return &LeaseError{Code: "superseded", Msg: fmt.Sprintf("the workspace went to %s instead", s.seatName(holderOf(l)))}
		case l.State != "handoff" || l.HandoffTo != to.String():
			if f := rt.handoffFailure(epoch); f != nil {
				return f
			}
			return s.unresponsive(rt, l)
		}
		select {
		case <-ch:
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) unresponsive(rt *runtime, l *store.Lease) *LeaseError {
	age := "never"
	if w, err := s.store.WorkspaceByID(rt.id); err == nil && w.CurrentCheckpoint != "" {
		if cp, err := s.store.CheckpointByID(rt.id, w.CurrentCheckpoint); err == nil {
			age = ago(s.now() - cp.CreatedAt)
		}
	}
	return &LeaseError{Code: "holder_unresponsive", Msg: fmt.Sprintf("%s holds the workspace and is not responding (last heartbeat %s, last checkpoint %s). Take over with force: its unsent changes are quarantined when it returns",
		s.seatName(holderOf(l)), ago(s.now()-l.HeartbeatAt), age)}
}

func ago(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

// handoffTimer ends a handoff the holder did not complete within
// T_handoff: HANDOFF(h → R, e) → STALE(h, e).
func (s *Server) handoffTimer(rt *runtime, epoch, deadline int64) {
	if d := time.Duration(deadline-s.now()) * time.Millisecond; d > 0 {
		select {
		case <-time.After(d):
		case <-rt.done():
			return
		}
	}
	rt.leaseMu.Lock()
	defer rt.leaseMu.Unlock()
	l, err := s.store.LeaseOf(nil, rt.id)
	if err != nil || l.Epoch != epoch || l.State != "handoff" {
		return
	}
	if ok, _ := s.store.EndHandoff(nil, rt.id, epoch, "stale"); ok {
		rt.setHandoffFailure(epoch, s.unresponsive(rt, l))
		s.event(rt.id, "server", "", "lease.handoff_timeout", map[string]any{"holder": holderOf(l).String(), "to": l.HandoffTo, "epoch": epoch})
		rt.broadcast()
	}
}

// ReleaseLease completes a handoff from the holding device after it flushed
// (§4.4), or refuses it (`refuse` explains why, e.g. a rebase in progress).
// A release for an epoch the authority has already moved past is
// acknowledged: the device no longer holds anything.
func (s *Server) ReleaseLease(rt *runtime, device string, epoch int64, refuse string) error {
	rt.leaseMu.Lock()
	defer rt.leaseMu.Unlock()
	l, err := s.store.LeaseOf(nil, rt.id)
	if err != nil {
		return err
	}
	if epoch < l.Epoch {
		return nil
	}
	if holderOf(l) != DeviceSeat(device) || epoch != l.Epoch {
		return &LeaseError{Code: "lease_lost", AttemptedEpoch: epoch, Msg: "this device does not hold the lease at that epoch"}
	}
	if l.State != "handoff" {
		return &LeaseError{Code: "not_in_handoff", AttemptedEpoch: epoch, Msg: "nobody asked for the workspace (the handoff ended); keep writing"}
	}
	if refuse != "" {
		if ok, _ := s.store.EndHandoff(nil, rt.id, epoch, "held"); ok {
			rt.setHandoffFailure(epoch, &LeaseError{Code: "git_op_in_progress", Msg: refuse})
			s.event(rt.id, "device", device, "lease.handoff_refused", map[string]any{"epoch": epoch, "reason": refuse})
			rt.broadcast()
		}
		return nil
	}
	to := parseSeat(l.HandoffTo)
	return s.transfer(rt, l, to, "granted", "handoff")
}

// Heartbeat records that the holding device is alive at its epoch. A
// heartbeat from anyone else changes nothing; the reply tells them the
// lease state either way (P-7).
func (s *Server) Heartbeat(rt *runtime, device string, epoch int64) error {
	l, err := s.store.LeaseOf(nil, rt.id)
	if err != nil || holderOf(l) != DeviceSeat(device) || l.Epoch != epoch {
		return err
	}
	ok, err := s.store.LeaseHeartbeat(nil, rt.id, epoch, device, s.now())
	if err == nil && ok && l.State == "stale" {
		s.event(rt.id, "device", device, "lease.resumed", map[string]any{"epoch": epoch})
		rt.broadcast()
	}
	return err
}

// ForceTakeover is HELD/HANDOFF/STALE(h, e) → HELD(to, e+1) without the
// holder's cooperation (§4.5). The audit event records the last checkpoint
// and its age: whatever the old holder wrote after it reaches quarantine
// when it returns.
func (s *Server) ForceTakeover(rt *runtime, to Seat, actorKind, actorID, why string) error {
	rt.leaseMu.Lock()
	defer rt.leaseMu.Unlock()
	l, err := s.store.LeaseOf(nil, rt.id)
	if err != nil {
		return err
	}
	if holderOf(l).Kind == "server" && to.Kind == "device" {
		// The server seat is in-process, so even a forced move from it
		// stops its writers and saves its last state first.
		rt.ops.Quiesce(fmt.Sprintf("%s took over this workspace; the server seat is read-only until it is handed back", s.seatName(to)))
		if err := rt.ops.Flush(); err != nil {
			log.Printf("workspace %s: flush before forced takeover: %v", rt.id, err)
		}
	}
	w, _ := s.store.WorkspaceByID(rt.id)
	payload := map[string]any{"from": holderOf(l).String(), "to": to.String(), "epoch": l.Epoch + 1, "previous_state": s.effectiveState(l), "why": why}
	if w != nil && w.CurrentCheckpoint != "" {
		payload["last_seq"] = w.CheckpointSeq
		if cp, err := s.store.CheckpointByID(rt.id, w.CurrentCheckpoint); err == nil {
			payload["last_checkpoint_age_ms"] = s.now() - cp.CreatedAt
		}
	}
	if err := s.transfer(rt, l, to, "forced", "held", "handoff", "stale"); err != nil {
		return err
	}
	s.event(rt.id, actorKind, actorID, "lease.forced", payload)
	return nil
}

// sweepStale writes HELD(device) → STALE for holders past T_stale.
func (s *Server) sweepStale() {
	s.mu.Lock()
	rts := make([]*runtime, 0, len(s.rts))
	for _, rt := range s.rts {
		rts = append(rts, rt)
	}
	s.mu.Unlock()
	cutoff := s.now() - s.cfg.TStale().Milliseconds()
	for _, rt := range rts {
		l, err := s.store.LeaseOf(nil, rt.id)
		if err != nil || l.HolderKind != "device" || l.State != "held" {
			continue
		}
		if ok, _ := s.store.MarkLeaseStale(rt.id, l.Epoch, cutoff); ok {
			s.event(rt.id, "server", "", "lease.stale", map[string]any{"holder": holderOf(l).String(), "epoch": l.Epoch, "heartbeat_at": l.HeartbeatAt})
			rt.broadcast()
		}
	}
}

// resumeHandoffs re-arms handoff timers after a restart.
func (s *Server) resumeHandoff(rt *runtime) {
	l, err := s.store.LeaseOf(nil, rt.id)
	if err == nil && l.State == "handoff" {
		go s.handoffTimer(rt, l.Epoch, l.HandoffDeadline)
	}
}

// revokeLeases is an immediate forced takeover by the server seat of every
// lease a revoked device holds (§3.2, F12).
func (s *Server) revokeLeases(deviceID, actorID string) {
	wss, err := s.store.LeasesHeldByDevice(deviceID)
	if err != nil {
		log.Printf("revoke %s: %v", deviceID, err)
		return
	}
	for _, ws := range wss {
		if rt := s.runtimeFor(ws); rt != nil {
			if err := s.ForceTakeover(rt, ServerSeat, "user", actorID, "device revoked"); err != nil {
				log.Printf("revoke %s: takeover of %s: %v", deviceID, ws, err)
			}
		}
	}
}

func writeLeaseErr(rw http.ResponseWriter, s *Server, wsID string, caller *store.Device, err error) {
	var le *LeaseError
	if !errors.As(err, &le) {
		writeErr(rw, 500, err.Error())
		return
	}
	out, _ := s.leaseReply(wsID, caller)
	if out == nil {
		out = map[string]any{}
	}
	out["error"] = le.Error()
	out["code"] = le.Code
	out["attempted_epoch"] = le.AttemptedEpoch
	writeJSON(rw, http.StatusConflict, out)
}
