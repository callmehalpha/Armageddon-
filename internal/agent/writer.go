package agent

import (
	"bytes"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/faults"
	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// The device writer (contract §6.4, §5.3, plan M7.2–M7.4): capture into a
// durable pending queue, push refs under the lease, upload each checkpoint
// as a thin pack and commit it with (epoch, parent, oid). Retries resend
// the same triple, so a lost acknowledgement never creates a second commit
// and never changes the seq (F5).

// localRefs returns heads and tags (what the writer mirrors, §5.3).
func localRefs(dir string) (map[string]string, error) {
	out, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/").Output()
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m, nil
}

func sameRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (r *replica) tail() string {
	if n := len(r.st.Pending); n > 0 {
		return r.st.Pending[n-1].Oid
	}
	return r.st.AppliedOid
}

// captureWriter appends a checkpoint to the pending queue if the working
// state changed since the queue's tail (§6.3: no-op captures cost nothing).
func (r *replica) captureWriter() {
	if gitshadow.IndexLocked(r.gitDir) {
		return // a git command is running; the next trigger retries (§5.6)
	}
	state, err := r.sh.CaptureState()
	if err != nil {
		r.say("capture failed: %v", err)
		return
	}
	tail := r.tail()
	if prev, err := r.sh.StateOf(tail); err == nil && state.Same(prev) {
		return
	}
	ref := fmt.Sprintf("refs/armageddon/pending/%d", faults.Now().UnixNano())
	oid, err := r.sh.CommitState(state, ref, tail, 0)
	if err != nil {
		r.say("capture failed: %v", err)
		return
	}
	r.st.Pending = append(r.st.Pending, pendingCP{Oid: oid, Parent: tail, Ref: ref})
	r.save()
}

// pushRefs mirrors heads and tags to repo.git under the lease (§5.3). The
// epoch header is passed per invocation, never written to .git/config.
func (r *replica) pushRefs() error {
	refs, err := localRefs(r.st.Path)
	if err != nil {
		return err
	}
	if sameRefs(refs, r.st.PushedRefs) {
		return nil
	}
	if gitshadow.IndexLocked(r.gitDir) {
		return fmt.Errorf("index.lock present; ref push retried later")
	}
	_, err = r.c.git(r.st.Path, "-c", fmt.Sprintf("http.extraHeader=X-Armageddon-Epoch: %d", r.st.Epoch),
		"push", "--atomic", "--porcelain", "--prune", "armageddon", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	if err != nil {
		return err
	}
	r.st.PushedRefs = refs
	r.st.Refs, _ = localBranches(r.st.Path)
	r.save()
	return nil
}

// upload stages a checkpoint on the server as a thin pack against its
// parent (P-1), falling back to a full pack.
func (r *replica) upload(p pendingCP) error {
	try := func(base string) (int, error) {
		pack, err := r.sh.PackSince(p.Oid, base)
		if err != nil {
			return 0, err
		}
		v := url.Values{"checkpoint": {p.Oid}, "base": {base}}
		resp, err := r.c.Raw("POST", "/api/workspaces/"+r.st.WorkspaceID+"/checkpoints/pack?"+v.Encode(), bytes.NewReader(pack))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, decode(resp, nil)
	}
	code, err := try(p.Parent)
	if code == 400 && p.Parent != "" {
		_, err = try("")
	}
	return err
}

// drain commits the pending queue in order (§6.4; F3 offline queue).
func (r *replica) drain() {
	for len(r.st.Pending) > 0 && r.st.Epoch > 0 {
		if err := r.pushRefs(); err != nil {
			if strings.Contains(err.Error(), "lease_lost") {
				r.heartbeat() // find out who holds it
			} else if r.connected {
				r.say("ref push: %v", err)
			}
			return
		}
		p := r.st.Pending[0]
		if err := r.upload(p); err != nil {
			if r.connected {
				r.say("checkpoint upload: %v", err)
			}
			return
		}
		epoch := r.st.Epoch
		var res current
		status, err := r.c.call("POST", "/api/workspaces/"+r.st.WorkspaceID+"/checkpoints/commit",
			map[string]any{"epoch": epoch, "parent": p.Parent, "checkpoint": p.Oid}, &res)
		switch {
		case status == 0:
			r.setOffline(err)
			return
		case status == 200:
			r.acked(p, res.Seq)
		case status == 409 && res.Code == "lease_lost":
			if res.AttemptedEpoch < r.st.Epoch {
				continue // a rejection of an older attempt: ignore it (P-8)
			}
			r.onCurrent(&res)
			if r.st.Epoch == epoch {
				r.loseLease("commit rejected: lease_lost")
			}
			return
		case status == 409 && res.Code == "parent_mismatch":
			r.parentMismatch(&res)
			return
		case status == 409 && res.Code == "head_missing":
			r.collapse()
			return
		default:
			r.say("commit failed: %v", err)
			return
		}
	}
}

// acked records an acknowledged checkpoint (I3: it is now the base).
func (r *replica) acked(p pendingCP, seq int64) {
	r.st.Pending = r.st.Pending[1:]
	r.sh.Git(nil, "update-ref", "-d", p.Ref)
	r.st.AppliedOid = p.Oid
	if seq > r.st.AppliedSeq {
		r.st.AppliedSeq = seq
	}
	r.save()
	r.say("committed checkpoint #%d", seq)
}

// parentMismatch: only after a lost acknowledgement or a bug (§6.4). If
// current is one of this device's queued checkpoints, the queue up to it
// landed; otherwise the queue is quarantined and current adopted.
func (r *replica) parentMismatch(cur *current) {
	for i, p := range r.st.Pending {
		if p.Oid == cur.ID {
			for _, q := range r.st.Pending[:i+1] {
				r.sh.Git(nil, "update-ref", "-d", q.Ref)
			}
			r.st.Pending = r.st.Pending[i+1:]
			r.st.AppliedOid, r.st.AppliedSeq = cur.ID, cur.Seq
			r.save()
			return
		}
	}
	r.quarantineLocal("parent_mismatch: queue not on current")
	r.dropPending()
	r.applyTo(cur, "parent_mismatch")
}

// collapse replaces the queue with one checkpoint of the current tree on
// top of the base: an intermediate checkpoint named a HEAD commit that no
// longer exists anywhere (amended before it was pushed). The tree holds
// the latest work, so nothing is lost.
func (r *replica) collapse() {
	if len(r.st.Pending) <= 1 && r.st.Pending[0].Parent == r.st.AppliedOid {
		head, _ := r.c.git(r.st.Path, "rev-parse", "HEAD")
		if branch, _ := r.c.git(r.st.Path, "symbolic-ref", "-q", "HEAD"); branch == "" {
			r.notify("HEAD is detached at %.12s, a commit on no branch: the server cannot store it. Create a branch (`git switch -c <name>`) so checkpoints can be committed", head)
		}
	}
	r.dropPending()
	r.captureWriter()
}

func (r *replica) dropPending() {
	for _, p := range r.st.Pending {
		r.sh.Git(nil, "update-ref", "-d", p.Ref)
	}
	r.st.Pending = nil
	r.save()
}

// heartbeat tells the authority this writer is alive; the reply carries
// the lease state (missed grant, writer resync, lease lost: P-7, P-9).
func (r *replica) heartbeat() {
	r.lastHeartbeat = time.Now()
	var res current
	status, err := r.c.call("POST", "/api/workspaces/"+r.st.WorkspaceID+"/lease/heartbeat", map[string]any{"epoch": r.st.Epoch}, &res)
	if status == 0 {
		r.setOffline(err)
		return
	}
	if status == 200 {
		r.setOnline()
		r.onCurrent(&res)
	}
}

// onGranted: this device now holds the lease (§3.3 ACQUIRING → WRITING).
// Local edits made on top of current are kept as a fast-forward; edits on
// an older base are quarantined first (I4, F4).
func (r *replica) onGranted(cur *current) {
	st := r.st
	r.notify("granted: this device now writes %s (epoch %d)", st.Path, cur.Lease.Epoch)
	// History: if the server's branches moved since the last sync, local
	// branch changes are kept aside before taking the server's (§5.5).
	if server, err := r.serverHeads(); err == nil && !sameRefs(server, st.Refs) {
		r.notify("server branches moved since this replica last synced; fetching them")
		if local, err := localBranches(st.Path); err == nil {
			for ref, oid := range local {
				if prev, ok := st.Refs[ref]; ok && prev != oid {
					q := fmt.Sprintf("refs/armageddon/quarantine/%d/%s", time.Now().Unix(), strings.TrimPrefix(ref, "refs/"))
					r.c.git(st.Path, "update-ref", q, oid)
					r.notify("local commits on %s kept as %s", ref, q)
				}
			}
		}
		r.c.git(st.Path, "fetch", "-q", "--prune", "--update-head-ok", "armageddon", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
		st.Refs, _ = localBranches(st.Path)
	}
	switch {
	case len(st.Pending) > 0 && st.Pending[0].Parent == cur.ID:
		// Unacknowledged work that builds on current carries over.
	case len(st.Pending) > 0:
		r.quarantineLocal("diverged: queued checkpoints not on current")
		r.dropPending()
		r.applyOne(cur.Seq, "diverged")
	case st.AppliedOid == cur.ID && st.LocalBase == "":
		// The server has not moved since the last apply: local edits are a
		// fast-forward and become the first checkpoint of this epoch.
	default:
		if err := r.applyOne(cur.Seq, "diverged: the server moved before `work local`"); err != nil {
			r.say("could not bring the replica to current: %v (retrying)", err)
			return // the next resync retries the grant
		}
	}
	st.Epoch, st.Mode, st.Status = cur.Lease.Epoch, "write", "WRITING"
	if st.PushedRefs == nil {
		st.PushedRefs = map[string]string{}
		for k, v := range st.Refs {
			st.PushedRefs[k] = v
		}
	}
	r.save()
	r.lastHeartbeat = time.Time{}
	r.captureWriter()
	r.drain()
}

func (r *replica) serverHeads() (map[string]string, error) {
	out, err := r.c.git(r.st.Path, "ls-remote", "--heads", "armageddon")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			m[f[1]] = f[0]
		}
	}
	return m, nil
}

// loseLease: the lease moved while this device believed it wrote (forced
// takeover, F2/F3). Its unacknowledged tail is quarantined, then the
// replica follows current again (§4.5).
func (r *replica) loseLease(why string) {
	st := r.st
	r.notify("LEASE LOST: %s", why)
	r.quarantineLocal("lease_lost")
	r.dropPending()
	st.Epoch, st.Mode, st.Status = 0, "follow", "FOLLOWING"
	r.save()
	r.resync()
}

// flushAndRelease answers flush_and_release (§4.4): refuse during a Git
// operation, wait for quiescence, push refs, commit everything, release.
func (r *replica) flushAndRelease() {
	if r.flushing || r.st.Epoch == 0 {
		return
	}
	r.flushing = true
	defer func() { r.flushing = false }()
	epoch := r.st.Epoch
	path := "/api/workspaces/" + r.st.WorkspaceID + "/lease/release"
	if op := gitshadow.OpInProgress(r.gitDir); op != "" {
		msg := fmt.Sprintf("a %s is in progress on %s (%s); finish or abort it there, or force the takeover (sequencer state is not transferred)", op, r.c.Cfg.Name, r.st.Path)
		if _, err := r.c.call("POST", path, map[string]any{"epoch": epoch, "refuse": msg}, nil); err == nil {
			r.notify("handoff to %s refused: %s", r.st.Lease.HandoffToName, msg)
		}
		return
	}
	r.say("handing the workspace to %s: flushing", r.st.Lease.HandoffToName)
	deadline := time.Now().Add(flushQuietMax)
	for r.deb.pending() && time.Since(r.deb.last) < writerQuiet && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	r.deb.reset()
	r.captureWriter()
	r.drain()
	if err := r.pushRefs(); err != nil || len(r.st.Pending) > 0 || r.st.Epoch != epoch {
		return // retried on the next tick while the handoff lasts
	}
	var res current
	status, err := r.c.call("POST", path, map[string]any{"epoch": epoch}, &res)
	switch {
	case status == 200:
		r.st.Epoch, r.st.Mode, r.st.Status = 0, "follow", "FOLLOWING"
		r.notify("released: %s now writes; this replica follows", res.Lease.HolderName)
		r.save()
	case status == 409 && res.Code == "not_in_handoff":
		r.st.Lease.State = "held" // the handoff ended; keep writing
	case status == 409:
		r.onCurrent(&res)
	default:
		r.say("release failed: %v", err)
	}
}
