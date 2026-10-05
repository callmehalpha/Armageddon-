package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// Timings of the replica loop. Tests shorten nothing: these are the
// contract's values (§6.3, §3.2).
const (
	followerQuiet   = 300 * time.Millisecond // DIRTY is reported within ~1 s of an edit (M6.7: < 2 s)
	writerQuiet     = 2 * time.Second        // capture after 2 s of quiet …
	writerMax       = 15 * time.Second       // … or at most 15 s into continuous edits
	backstop        = 30 * time.Second       // full capture even if the watcher saw nothing (P-12, §5.3 ref rescan)
	heartbeatEvery  = 15 * time.Second       // well inside T_stale (2 min)
	reportEvery     = time.Minute
	flushQuietMax   = 5 * time.Second // a flush waits for quiescence at most this long (§4.4)
	maxNoticeMemory = 20
)

// AgentVersion is reported with replica status.
var AgentVersion = "dev"

// replica runs one replica's state machine (contract §3.3): FOLLOWING,
// DIRTY, WRITING, RELEASING, QUARANTINING, with OFFLINE as a flag. One
// goroutine owns the shadow and the state; the long poll runs beside it.
type replica struct {
	c      *Client
	st     *replicaState
	sh     *gitshadow.Shadow
	gitDir string
	out    io.Writer
	deb    debouncer

	lastCur       *current
	lastHeartbeat time.Time
	lastBackstop  time.Time
	lastReport    time.Time
	lastRetry     time.Time
	reported      string
	dirtyNotified bool
	flushing      bool
	connected     bool

	knownMu sync.Mutex
	known   struct {
		seq, epoch    int64
		state, holder string
	}
}

func (r *replica) say(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	fmt.Fprintf(r.out, "%s %s\n", ts(), msg)
}

// notify is say plus a durable notice `work local` and `status` show.
func (r *replica) notify(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	r.say("%s", msg)
	r.st.Notices = append(r.st.Notices, notice{At: time.Now().UnixMilli(), Msg: msg})
	if len(r.st.Notices) > maxNoticeMemory {
		r.st.Notices = r.st.Notices[len(r.st.Notices)-maxNoticeMemory:]
	}
}

func (r *replica) save() {
	if err := r.st.save(); err != nil {
		r.say("cannot save replica state: %v", err)
	}
}

func ts() string { return time.Now().Format("15:04:05") }

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (st *replicaState) normalize() {
	if st.Refs == nil {
		st.Refs = map[string]string{}
	}
	if st.Mode == "" {
		st.Mode = "follow"
	}
	if st.Epoch > 0 {
		st.Mode = "write"
	}
}

// Follow runs one replica in the foreground until ctx ends: it follows,
// and writes while this device holds the lease (`armageddon follow`).
func (c *Client) Follow(ctx context.Context, wsID string, out io.Writer) error {
	lk, err := lockReplica(wsID)
	if err != nil {
		return err
	}
	defer lk.release()
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	st.normalize()
	r := &replica{c: c, st: st, sh: shadowFor(wsID, st.Path), gitDir: gitshadow.GitDirOf(st.Path), out: out}
	return r.run(ctx)
}

type pollResult struct {
	cur  *current
	err  error
	done chan struct{}
}

func (r *replica) setKnown(cur *current) {
	r.knownMu.Lock()
	defer r.knownMu.Unlock()
	if cur.Seq > r.known.seq {
		r.known.seq = cur.Seq
	}
	r.known.epoch, r.known.state, r.known.holder = cur.Lease.Epoch, cur.Lease.State, cur.Lease.Holder
}

// poll is the long poll on …/current: checkpoints and lease events
// (flush_and_release, granted, lease.forced) arrive through it.
func (r *replica) poll(ctx context.Context, ch chan<- pollResult) {
	backoff := time.Second
	for ctx.Err() == nil {
		r.knownMu.Lock()
		k := r.known
		r.knownMu.Unlock()
		wait := "50"
		if k.epoch < 0 {
			wait = "0" // the lease is unknown: resync at once
		}
		q := url.Values{"after": {fmt.Sprint(k.seq)}, "wait": {wait}, "known_epoch": {fmt.Sprint(k.epoch)},
			"known_state": {k.state}, "known_holder": {k.holder}}
		var cur current
		_, err := r.c.call("GET", "/api/workspaces/"+r.st.WorkspaceID+"/current?"+q.Encode(), nil, &cur)
		res := pollResult{cur: &cur, err: err, done: make(chan struct{})}
		select {
		case ch <- res:
		case <-ctx.Done():
			return
		}
		select {
		case <-res.done:
		case <-ctx.Done():
			return
		}
		if err != nil {
			sleep(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
		} else {
			backoff = time.Second
		}
	}
}

func (r *replica) run(ctx context.Context) error {
	w, err := newWatcher(r.st.Path)
	if err != nil {
		r.say("file watcher unavailable (%v); relying on a full capture every %s", err, backstop)
	}
	defer w.Close()
	var events <-chan struct{}
	if w != nil {
		events = w.Events
	}
	mode := "following"
	if r.st.Epoch > 0 {
		mode = fmt.Sprintf("writing (epoch %d, %d checkpoint(s) queued)", r.st.Epoch, len(r.st.Pending))
	}
	fmt.Fprintf(r.out, "Replica %s at checkpoint #%d, %s (Ctrl-C to stop)\n", r.st.Path, r.st.AppliedSeq, mode)
	r.knownMu.Lock()
	r.known.seq, r.known.epoch = r.st.AppliedSeq, -1
	r.knownMu.Unlock()
	pollCh := make(chan pollResult)
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	r.lastBackstop = time.Now()
	r.onLocalChange() // startup is a full capture (P-12)
	r.resync()        // learn the lease now (missed grant, lost lease while down)
	r.flushOutbox()
	go r.poll(pctx, pollCh)
	for {
		select {
		case <-ctx.Done():
			if r.st.Epoch > 0 {
				// Shutdown flush (§6.3): capture, and send what can be sent.
				r.captureWriter()
				r.drain()
			}
			r.save()
			return nil
		case <-events:
			r.deb.touch(time.Now())
		case pr := <-pollCh:
			if pr.err != nil {
				r.setOffline(pr.err)
			} else {
				r.setOnline()
				r.onCurrent(pr.cur)
				r.setKnown(pr.cur)
			}
			close(pr.done)
		case now := <-tick.C:
			r.deb.quiet, r.deb.max = followerQuiet, writerMax
			if r.st.Epoch > 0 {
				r.deb.quiet = writerQuiet
			}
			if r.deb.due(now) || now.Sub(r.lastBackstop) >= backstop {
				r.deb.reset()
				r.lastBackstop = now
				r.onLocalChange()
			}
			r.periodic(now)
		}
	}
}

func (r *replica) setOffline(err error) {
	if !r.st.Offline {
		r.st.Offline = true
		if r.st.Epoch > 0 {
			r.notify("OFFLINE: server unreachable (%v). Still writing: checkpoints queue locally and are sent in order on reconnect", err)
		} else {
			r.say("OFFLINE: server unreachable (%v); retrying", err)
		}
		r.save()
	}
	r.connected = false
}

func (r *replica) setOnline() {
	if r.st.Offline {
		r.st.Offline = false
		r.say("back online")
		r.save()
	}
	r.connected = true
}

// heartbeatInterval is 15 s, or a quarter of the server's T_stale when
// that is shorter, so a live writer is never shown as STALE.
func (r *replica) heartbeatInterval() time.Duration {
	d := heartbeatEvery
	if s := time.Duration(r.st.Lease.StaleAfterMS) * time.Millisecond / 4; s > 0 && s < d {
		d = s
	}
	return d
}

// periodic runs every tick: retries, heartbeats, status reports.
func (r *replica) periodic(now time.Time) {
	if len(r.st.Outbox) > 0 && now.Sub(r.lastRetry) >= 2*time.Second {
		r.lastRetry = now
		r.flushOutbox()
	}
	if r.st.Epoch > 0 {
		if now.Sub(r.lastHeartbeat) >= r.heartbeatInterval() {
			r.heartbeat()
		}
		if len(r.st.Pending) > 0 && now.Sub(r.lastRetry) >= 2*time.Second {
			r.lastRetry = now
			r.drain()
		}
		if r.st.Epoch > 0 && r.st.Lease.State == "handoff" && r.st.Lease.You && r.st.Lease.Epoch == r.st.Epoch && now.Sub(r.lastRetry) >= 2*time.Second {
			r.lastRetry = now
			r.flushAndRelease()
		}
	} else if r.lastCur != nil && (r.lastCur.Seq > r.st.AppliedSeq || r.st.LocalBase != "") && now.Sub(r.lastRetry) >= 5*time.Second {
		// A failed apply is retried.
		r.lastRetry = now
		r.applyTo(r.lastCur, "follower dirty")
	}
	r.report(now)
}

func (r *replica) status() string {
	switch {
	case r.st.Epoch > 0 && r.st.Lease.State == "handoff" && r.st.Lease.You:
		return "RELEASING"
	case r.st.Epoch > 0:
		return "WRITING"
	}
	return r.st.Status
}

// report mirrors the replica state to the server for display (§3.3).
func (r *replica) report(now time.Time) {
	st := r.status()
	if r.st.Offline {
		st += "+OFFLINE"
	}
	if r.st.Status != r.status() {
		r.st.Status = r.status()
		r.save()
	}
	if !r.connected || (st == r.reported && now.Sub(r.lastReport) < reportEvery) {
		return
	}
	mode := "follow"
	if r.st.Epoch > 0 {
		mode = "write"
	}
	body := map[string]any{"state": st, "mode": mode, "applied_seq": r.st.AppliedSeq, "pending": len(r.st.Pending), "agent_version": AgentVersion}
	if _, err := r.c.call("POST", "/api/workspaces/"+r.st.WorkspaceID+"/replica", body, nil); err == nil {
		r.reported, r.lastReport = st, now
	}
}

// resync reads (holder, epoch, current) now and acts on it.
func (r *replica) resync() {
	var cur current
	if _, err := r.c.call("GET", "/api/workspaces/"+r.st.WorkspaceID+"/current?wait=0", nil, &cur); err != nil {
		r.setOffline(err)
		return
	}
	r.setOnline()
	r.onCurrent(&cur)
	r.setKnown(&cur)
}

// onCurrent applies the rules of §3.3 and §4.4a to a resync, heartbeat or
// rejection reply.
func (r *replica) onCurrent(cur *current) {
	if cur.Lease.Epoch == 0 {
		return // not a lease reply (error body)
	}
	r.lastCur = cur
	l := cur.Lease
	if l.Epoch < r.st.Epoch {
		return // older than what this device already knows: stale reply
	}
	r.st.Lease = l
	switch {
	case l.You && l.Epoch > r.st.Epoch:
		// A grant, or a missed grant found in a resync (P-7).
		r.onGranted(cur)
	case r.st.Epoch > 0 && l.Epoch > r.st.Epoch:
		why := "the lease moved to " + l.HolderName
		if cur.Event == "lease.forced" {
			why = l.HolderName + " took the workspace over (forced)"
		}
		r.loseLease(why)
	case r.st.Epoch > 0:
		if l.State == "handoff" {
			r.flushAndRelease()
		} else if len(r.st.Pending) == 0 && cur.ID != r.st.AppliedOid && cur.Seq > r.st.AppliedSeq {
			// Writer resync (P-9): one of this device's own commits landed
			// after it stopped tracking it.
			r.applyTo(cur, "writer resync: local edits on a stale base")
		}
	default:
		if cur.Seq > r.st.AppliedSeq || r.st.LocalBase != "" {
			r.applyTo(cur, "follower dirty")
		}
	}
}

// onLocalChange: the watcher (debounced) or the backstop fired.
func (r *replica) onLocalChange() {
	if r.st.Epoch > 0 {
		r.captureWriter()
		r.drain()
		return
	}
	if r.st.AppliedOid == "" && r.st.LocalBase == "" {
		return
	}
	base := r.st.AppliedOid
	if r.st.LocalBase != "" {
		base = r.st.LocalBase
	}
	state, err := r.sh.CaptureState()
	if err != nil {
		return
	}
	prev, err := r.sh.StateOf(base)
	if err != nil {
		return
	}
	if !state.Same(prev) {
		if r.st.Status != "DIRTY" {
			r.st.Status = "DIRTY"
			r.save()
		}
		if !r.dirtyNotified {
			r.dirtyNotified = true
			owner := r.st.Lease.HolderName
			if owner == "" {
				owner = "its current writer"
			}
			r.notify("DIRTY: you edited a read-only replica. The workspace is owned by %s. Run `armageddon work local` to take ownership; otherwise these edits are set aside on the server, for review, when the next checkpoint arrives", owner)
		}
	} else if r.st.Status == "DIRTY" {
		r.st.Status, r.dirtyNotified = "FOLLOWING", false
		r.save()
	}
}

// applyTo moves the replica to checkpoint cur (§6.5). Local state that
// never reached the server is quarantined before it is overwritten (I4).
func (r *replica) applyTo(cur *current, reason string) {
	if err := r.applyOne(cur.Seq, reason); err != nil {
		r.say("apply failed: %v", err)
	}
}

func (r *replica) applyOne(seq int64, reason string) error {
	st, sh := r.st, r.sh
	cp, err := r.c.fetchCheckpoint(sh, st.WorkspaceID, seq, st.AppliedOid)
	if err != nil {
		return err
	}
	from := st.AppliedOid
	if st.LocalBase != "" {
		from = st.LocalBase
	}
	if cp == from {
		st.AppliedOid, st.AppliedSeq, st.LocalBase = cp, seq, ""
		r.save()
		return nil
	}
	// History first. Local commits on a read-only replica are kept under
	// refs/armageddon/quarantine/ before branches are overwritten (§5.5).
	if local, err := localBranches(st.Path); err == nil {
		for ref, oid := range local {
			if prev, ok := st.Refs[ref]; ok && prev != oid {
				q := fmt.Sprintf("refs/armageddon/quarantine/%d/%s", time.Now().Unix(), strings.TrimPrefix(ref, "refs/"))
				r.c.git(st.Path, "update-ref", q, oid)
				r.notify("local commits on %s kept as %s", ref, q)
			}
		}
	}
	if _, err := r.c.git(st.Path, "fetch", "-q", "--prune", "--update-head-ok", "armageddon", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	if err := r.c.pointHead(sh, cp); err != nil {
		return err
	}
	err = sh.Apply(from, cp, gitshadow.ApplyOptions{})
	if errors.Is(err, gitshadow.ErrDiverged) || (err != nil && strings.Contains(err.Error(), "diverged")) {
		if _, qerr := r.quarantineLocal(reason); qerr != nil {
			return fmt.Errorf("local edits found but not saved for quarantine (%v); not overwriting them", qerr)
		}
		err = sh.Seed(cp)
	}
	if err != nil {
		return err
	}
	st.AppliedOid, st.AppliedSeq, st.LocalBase = cp, seq, ""
	st.Refs, _ = localBranches(st.Path)
	st.PushedRefs, _ = localRefs(st.Path)
	if st.Epoch == 0 {
		st.Status = "FOLLOWING"
	}
	r.dirtyNotified = false
	r.save()
	r.say("applied checkpoint #%d", seq)
	return nil
}

// quarantineLocal keeps the replica's local state (the tree, which holds
// any pending checkpoints' content) in the durable outbox and tries to
// upload it (§6.7, I4). It returns "" when there is nothing to keep.
func (r *replica) quarantineLocal(reason string) (string, error) {
	st := r.st
	state, err := r.sh.CaptureState()
	if err != nil {
		return "", err
	}
	base := st.AppliedOid
	cmp := base
	if st.LocalBase != "" {
		cmp = st.LocalBase
	}
	if len(st.Pending) == 0 {
		if prev, err := r.sh.StateOf(cmp); err == nil && state.Same(prev) {
			return "", nil
		}
	}
	ref := fmt.Sprintf("refs/armageddon/outbox/%d", time.Now().UnixNano())
	oid, err := r.sh.CommitState(state, ref, base, 0)
	if err != nil {
		return "", err
	}
	st.Outbox = append(st.Outbox, outboxQ{Oid: oid, Base: base, Reason: reason, Ref: ref})
	st.LocalBase = oid
	st.Status = "QUARANTINING"
	r.save()
	r.flushOutbox()
	return oid, nil
}

// flushOutbox uploads queued quarantines (append-only, any epoch, §4.2).
func (r *replica) flushOutbox() {
	st := r.st
	for len(st.Outbox) > 0 {
		q := st.Outbox[0]
		id, err := r.uploadQuarantine(q)
		if err != nil {
			if !strings.Contains(err.Error(), "quota") || r.connected {
				r.say("quarantine upload pending: %v", err)
			}
			return
		}
		if q.Reason == "follower dirty" {
			r.notify("local edits on this read-only replica were saved to the server as quarantine %s", id)
		} else {
			r.notify("local changes (%s) were saved to the server as quarantine %s — see `armageddon quarantine diff %s`", q.Reason, id, id)
		}
		r.sh.Git(nil, "update-ref", "-d", q.Ref)
		st.Outbox = st.Outbox[1:]
		r.save()
	}
}

func (r *replica) uploadQuarantine(q outboxQ) (string, error) {
	try := func(thin bool) (string, int, error) {
		base := ""
		if thin {
			base = q.Base
		}
		pack, err := r.sh.PackSince(q.Oid, base)
		if err != nil {
			return "", 0, err
		}
		v := url.Values{"checkpoint": {q.Oid}, "base": {q.Base}, "reason": {q.Reason}}
		resp, err := r.c.Raw("POST", "/api/workspaces/"+r.st.WorkspaceID+"/quarantines?"+v.Encode(), bytes.NewReader(pack))
		if err != nil {
			return "", 0, err
		}
		defer resp.Body.Close()
		var res struct {
			ID string `json:"id"`
		}
		err = decode(resp, &res)
		return res.ID, resp.StatusCode, err
	}
	id, code, err := try(q.Base != "")
	if code == 400 && q.Base != "" {
		id, _, err = try(false) // the server lacks the base: full pack
	}
	return id, err
}
