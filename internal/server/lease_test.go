package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// fakeSeat records the server seat's side of each transition.
type fakeSeat struct {
	mu       sync.Mutex
	calls    []string
	gitOp    string
	flushErr error
}

func (f *fakeSeat) rec(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }
func (f *fakeSeat) GitOpInProgress() string {
	f.rec("gitop")
	return f.gitOp
}
func (f *fakeSeat) Quiesce(reason string) { f.rec("quiesce") }
func (f *fakeSeat) Flush() error          { f.rec("flush"); return f.flushErr }
func (f *fakeSeat) BecameFollower()       { f.rec("follower") }
func (f *fakeSeat) BecameHolder()         { f.rec("holder") }
func (f *fakeSeat) take() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := strings.Join(f.calls, ",")
	f.calls = nil
	return s
}

type leaseEnv struct {
	t    *testing.T
	s    *Server
	rt   *runtime
	seat *fakeSeat
	now  int64
	max  int64 // highest epoch seen: it must never decrease
}

func newLeaseEnv(t *testing.T) *leaseEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default(t.TempDir())
	cfg.HandoffTimeoutMS = 300
	e := &leaseEnv{t: t, seat: &fakeSeat{}, now: 1_000_000}
	s := &Server{cfg: cfg, store: st, rts: map[string]*runtime{}}
	s.clock = func() int64 { return e.now }
	now := store.Now()
	if err := st.CreateUser(&store.User{ID: "u1", Username: "u", PasswordHash: "x", Role: "admin", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	w := &store.Workspace{ID: "ws1", OwnerID: "u1", Name: "w", Slug: "w", State: StateReady, SourceKind: "empty", CreatedAt: e.now, UpdatedAt: e.now}
	if err := st.Tx(context.Background(), func(tx *sql.Tx) error { return st.CreateWorkspace(tx, w) }); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"d1", "d2"} {
		if _, err := st.DB().Exec(`INSERT INTO devices (id, user_id, name, platform, public_key, created_at, last_seen_at) VALUES (?, 'u1', ?, 'test', ?, 0, 0)`, d, d+"-laptop", "pk-"+d); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := &runtime{id: "ws1", notify: make(chan struct{}), ops: e.seat, ctx: ctx}
	s.rts["ws1"] = rt
	e.s, e.rt = s, rt
	return e
}

// expect checks the lease is <state>(<holder>, <epoch>) and that the
// epoch never went down.
func (e *leaseEnv) expect(state, holder string, epoch int64) {
	e.t.Helper()
	l, err := e.s.store.LeaseOf(nil, "ws1")
	if err != nil {
		e.t.Fatal(err)
	}
	if l.Epoch < e.max {
		e.t.Fatalf("epoch decreased: %d after %d", l.Epoch, e.max)
	}
	e.max = l.Epoch
	if l.State != state || holderOf(l).String() != holder || l.Epoch != epoch {
		e.t.Fatalf("lease = %s(%s, %d), want %s(%s, %d)", l.State, holderOf(l), l.Epoch, state, holder, epoch)
	}
}

func code(err error) string {
	var le *LeaseError
	if errors.As(err, &le) {
		return le.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}

// acquireAsync starts AcquireLease and returns its result channel.
func (e *leaseEnv) acquireAsync(to Seat) chan error {
	ch := make(chan error, 1)
	go func() { ch <- e.s.AcquireLease(context.Background(), e.rt, to, "test", "") }()
	return ch
}

// waitState waits for the lease to reach state (a handoff in flight).
func (e *leaseEnv) waitState(state string) {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		if l, _ := e.s.store.LeaseOf(nil, "ws1"); l != nil && l.State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("lease never reached %s", state)
}

func (e *leaseEnv) toD1() {
	e.t.Helper()
	if err := e.s.AcquireLease(context.Background(), e.rt, DeviceSeat("d1"), "test", ""); err != nil {
		e.t.Fatal(err)
	}
	e.seat.take()
	e.expect("held", "device:d1", 2)
}

func TestLeaseInitialState(t *testing.T) {
	e := newLeaseEnv(t)
	e.expect("held", "server", 1)
}

// HELD(server, 1) → HANDOFF → HELD(d1, 2), in-process: Git-op check,
// sessions + dev processes stopped, flush, then the transfer.
func TestLeaseServerToDevice(t *testing.T) {
	e := newLeaseEnv(t)
	if err := e.s.AcquireLease(context.Background(), e.rt, DeviceSeat("d1"), "test", ""); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d1", 2)
	if got := e.seat.take(); got != "gitop,quiesce,flush,follower" {
		t.Fatalf("server seat steps = %q", got)
	}
	if tr := e.rt.lastTransition(); tr.epoch != 2 || tr.kind != "granted" {
		t.Fatalf("last transition = %+v", tr)
	}
}

// The transfer takes the fence as a writer: it waits for an in-flight push.
func TestLeaseTransferWaitsForFence(t *testing.T) {
	e := newLeaseEnv(t)
	e.rt.fence.RLock() // a receive-pack in flight
	ch := e.acquireAsync(DeviceSeat("d1"))
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-ch:
		t.Fatalf("lease moved during a push: %v", err)
	default:
	}
	e.expect("handoff", "server", 1)
	e.rt.fence.RUnlock()
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d1", 2)
}

// §5.6/F11: a Git operation in progress on the server seat refuses the
// handoff before any session is closed.
func TestLeaseRefusedGitOpOnServer(t *testing.T) {
	e := newLeaseEnv(t)
	e.seat.gitOp = "rebase"
	err := e.s.AcquireLease(context.Background(), e.rt, DeviceSeat("d1"), "test", "")
	if code(err) != "git_op_in_progress" {
		t.Fatalf("err = %v", err)
	}
	if got := e.seat.take(); got != "gitop" {
		t.Fatalf("steps = %q (sessions must stay open)", got)
	}
	e.expect("held", "server", 1)
}

func TestLeaseFlushFailureCancels(t *testing.T) {
	e := newLeaseEnv(t)
	e.seat.flushErr = errors.New("disk full")
	if err := e.s.AcquireLease(context.Background(), e.rt, DeviceSeat("d1"), "test", ""); err == nil {
		t.Fatal("handoff succeeded without a flush")
	}
	e.expect("held", "server", 1)
}

// Acquire by the holder is idempotent: no epoch change (re-sent grant).
func TestLeaseAcquireByHolder(t *testing.T) {
	e := newLeaseEnv(t)
	if err := e.s.AcquireLease(context.Background(), e.rt, ServerSeat, "test", ""); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "server", 1)
	e.toD1()
	if err := e.s.AcquireLease(context.Background(), e.rt, DeviceSeat("d1"), "test", ""); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d1", 2)
}

// HELD(d1, 2) → HANDOFF(d1 → d2, 2) → (flush+release) → HELD(d2, 3).
func TestLeaseDeviceToDevice(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	ch := e.acquireAsync(DeviceSeat("d2"))
	e.waitState("handoff")
	l, _ := e.s.store.LeaseOf(nil, "ws1")
	if l.HandoffTo != "device:d2" || l.Epoch != 2 {
		t.Fatalf("handoff = %+v", l)
	}
	// Forbidden while handing off: a third party, a release by the wrong
	// device, a release with a wrong (newer) epoch.
	if err := e.s.AcquireLease(context.Background(), e.rt, ServerSeat, "test", ""); code(err) != "handoff_in_progress" {
		t.Fatalf("third-party acquire during handoff: %v", err)
	}
	if err := e.s.ReleaseLease(e.rt, "d2", 2, ""); code(err) != "lease_lost" {
		t.Fatalf("release by a non-holder: %v", err)
	}
	if err := e.s.ReleaseLease(e.rt, "d1", 3, ""); code(err) != "lease_lost" {
		t.Fatalf("release with a future epoch: %v", err)
	}
	e.expect("handoff", "device:d1", 2)
	if err := e.s.ReleaseLease(e.rt, "d1", 2, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d2", 3)
	if got := e.seat.take(); got != "" {
		t.Fatalf("server seat touched by a device-to-device handoff: %q", got)
	}
	// A late duplicate release for the old epoch is acknowledged and
	// changes nothing.
	if err := e.s.ReleaseLease(e.rt, "d1", 2, ""); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d2", 3)
}

// HANDOFF(d1 → server) → HELD(server, 3): the server seat is brought to
// current before it writes.
func TestLeaseDeviceToServer(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	ch := e.acquireAsync(ServerSeat)
	e.waitState("handoff")
	if err := e.s.ReleaseLease(e.rt, "d1", 2, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	e.expect("held", "server", 3)
	if got := e.seat.take(); got != "holder" {
		t.Fatalf("server seat steps = %q", got)
	}
}

// The holder refuses (Git op in progress on the device): HANDOFF → HELD(h, e)
// and the requester sees why.
func TestLeaseHolderRefuses(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	ch := e.acquireAsync(ServerSeat)
	e.waitState("handoff")
	if err := e.s.ReleaseLease(e.rt, "d1", 2, "a rebase is in progress on d1-laptop"); err != nil {
		t.Fatal(err)
	}
	err := <-ch
	if code(err) != "git_op_in_progress" || !strings.Contains(err.Error(), "rebase") {
		t.Fatalf("requester got %v", err)
	}
	e.expect("held", "device:d1", 2)
}

// HANDOFF → STALE after T_handoff; heartbeat resumes → HELD.
func TestLeaseHandoffTimeout(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	e.now += 1000 // the timer compares with the injected clock
	ch := e.acquireAsync(ServerSeat)
	e.waitState("handoff")
	e.now += 400 // past T_handoff (300 ms)
	select {
	case err := <-ch:
		if code(err) != "holder_unresponsive" {
			t.Fatalf("requester got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handoff never timed out")
	}
	e.expect("stale", "device:d1", 2)
	// The holder finishing its flush late cannot complete the dead handoff.
	if err := e.s.ReleaseLease(e.rt, "d1", 2, ""); code(err) != "not_in_handoff" {
		t.Fatalf("late release: %v", err)
	}
	if err := e.s.Heartbeat(e.rt, "d1", 2); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d1", 2)
}

// HELD(device) → STALE after T_stale without heartbeats; STALE is
// informational (nothing transfers) and a graceful acquire is refused.
func TestLeaseStaleIsInformational(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	e.now += (2 * time.Minute).Milliseconds() + 1
	e.s.sweepStale()
	e.expect("stale", "device:d1", 2)
	e.s.sweepStale()
	e.expect("stale", "device:d1", 2)
	if err := e.s.AcquireLease(context.Background(), e.rt, ServerSeat, "test", ""); code(err) != "holder_unresponsive" {
		t.Fatalf("acquire from a stale holder: %v", err)
	}
	e.expect("stale", "device:d1", 2)
	// Heartbeats from the wrong device or a wrong epoch change nothing.
	e.s.Heartbeat(e.rt, "d2", 2)
	e.s.Heartbeat(e.rt, "d1", 1)
	e.expect("stale", "device:d1", 2)
	if err := e.s.Heartbeat(e.rt, "d1", 2); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d1", 2)
	// The server seat never goes stale.
	e.s.ForceTakeover(e.rt, ServerSeat, "test", "", "test")
	e.now += (10 * time.Minute).Milliseconds()
	e.s.sweepStale()
	e.expect("held", "server", 3)
}

// STALE/HELD/HANDOFF(h, e) → force → HELD(R, e+1), always a new epoch,
// including a device forcing its own lease (P4 found that case).
func TestLeaseForce(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	e.now += (3 * time.Minute).Milliseconds()
	e.s.sweepStale()
	if err := e.s.ForceTakeover(e.rt, ServerSeat, "user", "u1", "explicit"); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "server", 3)
	if got := e.seat.take(); got != "holder" {
		t.Fatalf("steps = %q", got)
	}
	if tr := e.rt.lastTransition(); tr.kind != "forced" || tr.epoch != 3 {
		t.Fatalf("transition = %+v", tr)
	}
	// From the server to a device: sessions and processes still stop.
	if err := e.s.ForceTakeover(e.rt, DeviceSeat("d2"), "device", "d2", "explicit"); err != nil {
		t.Fatal(err)
	}
	e.expect("held", "device:d2", 4)
	if got := e.seat.take(); got != "quiesce,flush,follower" {
		t.Fatalf("steps = %q", got)
	}
	// During a handoff.
	ch := e.acquireAsync(DeviceSeat("d1"))
	e.waitState("handoff")
	if err := e.s.ForceTakeover(e.rt, ServerSeat, "user", "u1", "explicit"); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; code(err) != "superseded" {
		t.Fatalf("superseded requester got %v", err)
	}
	e.expect("held", "server", 5)
	// Its own lease: still a new epoch.
	e.s.ForceTakeover(e.rt, DeviceSeat("d1"), "device", "d1", "explicit")
	e.s.ForceTakeover(e.rt, DeviceSeat("d1"), "device", "d1", "explicit")
	e.expect("held", "device:d1", 7)
	evs, _ := e.s.store.Events("ws1", 100)
	forced := 0
	for _, ev := range evs {
		if ev.Type == "lease.forced" {
			forced++
		}
	}
	if forced != 5 {
		t.Fatalf("lease.forced events = %d", forced)
	}
}

// Revoking a holder is an immediate takeover by the server seat.
func TestLeaseRevokedHolder(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	e.s.revokeLeases("d1", "u1")
	e.expect("held", "server", 3)
}

// Release outside a handoff is refused; the holder keeps writing.
func TestLeaseReleaseWithoutHandoff(t *testing.T) {
	e := newLeaseEnv(t)
	e.toD1()
	if err := e.s.ReleaseLease(e.rt, "d1", 2, ""); code(err) != "not_in_handoff" {
		t.Fatalf("release: %v", err)
	}
	e.expect("held", "device:d1", 2)
}

// The store transitions are conditional on the epoch they start from.
func TestLeaseConditionalSQL(t *testing.T) {
	e := newLeaseEnv(t)
	st := e.s.store
	if ok, _ := st.TransferLease(nil, "ws1", 7, "device", "d1", 0, "held"); ok {
		t.Fatal("transfer from a wrong epoch succeeded")
	}
	if ok, _ := st.TransferLease(nil, "ws1", 1, "device", "d1", 0, "handoff"); ok {
		t.Fatal("transfer from a wrong state succeeded")
	}
	if ok, _ := st.EndHandoff(nil, "ws1", 1, "held"); ok {
		t.Fatal("ended a handoff that never started")
	}
	if ok, _ := st.BeginHandoff(nil, "ws1", 0, "device:d1", 0); ok {
		t.Fatal("handoff from a wrong epoch")
	}
	// Commit-side check (§6.4 4a): the holder at its epoch only, including
	// during its own handoff (the flush).
	if ok, _ := st.CheckHolder(nil, "ws1", 1, "server", "", 0); !ok {
		t.Fatal("server holder rejected")
	}
	if ok, _ := st.CheckHolder(nil, "ws1", 1, "device", "d1", 0); ok {
		t.Fatal("non-holder device accepted")
	}
	e.toD1()
	if ok, _ := st.CheckHolder(nil, "ws1", 1, "server", "", 0); ok {
		t.Fatal("old server epoch accepted after the transfer")
	}
	if ok, _ := st.BeginHandoff(nil, "ws1", 2, "server", 0); !ok {
		t.Fatal("begin handoff")
	}
	if ok, _ := st.CheckHolder(nil, "ws1", 2, "device", "d1", 0); !ok {
		t.Fatal("holder's flush commit rejected during its handoff")
	}
	if ok, _ := st.BeginHandoff(nil, "ws1", 2, "device:d2", 0); ok {
		t.Fatal("second handoff started over the first")
	}
}
