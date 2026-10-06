package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/identity"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

func (s *Server) quarantineQuota() int {
	if n, err := strconv.Atoi(os.Getenv("ARMAGEDDON_QUARANTINE_QUOTA")); err == nil && n > 0 {
		return n
	}
	return 100
}

func actorOf(r *http.Request) (string, string) {
	if d := deviceOf(r); d != nil {
		return "device", d.ID
	}
	return "user", userOf(r).ID
}

func (s *Server) readyRuntime(rw http.ResponseWriter, w *store.Workspace) *runtime {
	rt := s.runtimeFor(w.ID)
	if rt == nil || w.State != StateReady {
		writeErr(rw, 503, "workspace not running")
		return nil
	}
	return rt
}

// handleCurrent is the resync and event channel (P-7, §4.4a). It returns
// the current checkpoint and the lease. With ?after=<seq>&wait=<s> it
// long-polls until a newer checkpoint exists; with ?known_epoch=<e> and
// ?known_state=<s> it also returns as soon as the lease differs from what
// the caller knows. "event" names what changed for this caller:
// flush_and_release (hand the workspace over), granted (this device now
// writes), lease.forced / lease.moved (this device's lease is gone).
func (s *Server) handleCurrent(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	wait, _ := strconv.Atoi(q.Get("wait"))
	knownEpoch := int64(-1)
	if v := q.Get("known_epoch"); v != "" {
		knownEpoch, _ = strconv.ParseInt(v, 10, 64)
	}
	knownState := q.Get("known_state")
	knownHolder := q.Get("known_holder")
	if wait > 55 {
		wait = 55
	}
	dev := deviceOf(r)
	deadline := time.After(time.Duration(wait) * time.Second)
	rt := s.runtimeFor(w.ID)
	for {
		var changed <-chan struct{}
		if rt != nil {
			changed = rt.changed()
		}
		out, err := s.leaseReply(w.ID, dev)
		if err != nil {
			writeErr(rw, 404, "no such workspace")
			return
		}
		lj := out["lease"].(map[string]any)
		epoch, state, holder := lj["epoch"].(int64), lj["state"].(string), lj["holder"].(string)
		leaseChanged := knownEpoch >= 0 && (epoch != knownEpoch || (knownState != "" && state != knownState) || (knownHolder != "" && holder != knownHolder))
		if out["seq"].(int64) > after || wait == 0 || leaseChanged {
			you := lj["you"].(bool)
			ev := ""
			switch {
			case you && state == "handoff":
				ev = "flush_and_release"
			case you && knownEpoch >= 0 && epoch > knownEpoch:
				ev = "granted"
			case !you && knownEpoch >= 0 && epoch > knownEpoch:
				ev = "lease.moved"
				if rt != nil && rt.lastTransition().epoch == epoch && rt.lastTransition().kind == "forced" {
					ev = "lease.forced"
				}
			}
			out["event"] = ev
			writeJSON(rw, 200, out)
			return
		}
		if rt == nil {
			writeErr(rw, 503, "workspace not running")
			return
		}
		select {
		case <-changed:
		case <-deadline:
			wait = 0 // answer with the unchanged state
		case <-r.Context().Done():
			return
		}
	}
}

// handleAcquire: `armageddon work local` (to=device, from the device) and
// `armageddon work remote` / the UI's "Work on server" (to=server). It
// blocks until the lease moved or the handoff failed (§4.4).
func (s *Server) handleAcquire(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	var req struct {
		To      string `json:"to"`
		Restart bool   `json:"restart"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(rw, 400, "bad request")
		return
	}
	dev := deviceOf(r)
	to := ServerSeat
	switch req.To {
	case "device":
		if dev == nil {
			writeErr(rw, 400, "only a device can take the workspace local (run `armageddon work local` on it)")
			return
		}
		to = DeviceSeat(dev.ID)
	case "server", "":
		if req.To == "" && dev != nil {
			to = DeviceSeat(dev.ID)
		}
	default:
		writeErr(rw, 400, "to must be device or server")
		return
	}
	kind, id := actorOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.THandoff()+15*time.Second)
	defer cancel()
	if err := s.AcquireLease(ctx, rt, to, kind, id); err != nil {
		writeLeaseErr(rw, s, w.ID, dev, err)
		return
	}
	if req.Restart && to == ServerSeat {
		// Q1: dev processes stopped at handoff are restarted by the user or
		// by `work remote --restart`. Runtimes arrive with Phase 4, which
		// acts on this event; until then it is recorded only.
		s.event(w.ID, kind, id, "runtime.restart_requested", nil)
	}
	out, err := s.leaseReply(w.ID, dev)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, out)
}

// handleRelease: the holding device flushed and releases (or refuses).
func (s *Server) handleRelease(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	if dev == nil {
		writeErr(rw, 403, "devices only")
		return
	}
	var req struct {
		Epoch  int64  `json:"epoch"`
		Refuse string `json:"refuse"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	if err := s.ReleaseLease(rt, dev.ID, req.Epoch, req.Refuse); err != nil {
		writeLeaseErr(rw, s, w.ID, dev, err)
		return
	}
	out, _ := s.leaseReply(w.ID, dev)
	writeJSON(rw, 200, out)
}

// handleHeartbeat: the writer is alive. The reply carries (holder, epoch,
// current) for missed-grant recovery and writer resync (P-7, P-9).
func (s *Server) handleHeartbeat(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	if dev == nil {
		writeErr(rw, 403, "devices only")
		return
	}
	var req struct {
		Epoch int64 `json:"epoch"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	if err := s.Heartbeat(rt, dev.ID, req.Epoch); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	s.store.TouchDevice(dev.ID, store.Now())
	out, _ := s.leaseReply(w.ID, dev)
	writeJSON(rw, 200, out)
}

// handleForce: forced takeover (§4.5). From a browser it needs the
// account password again (plan M2.3: re-auth for dangerous actions).
func (s *Server) handleForce(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	var req struct {
		To       string `json:"to"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	dev := deviceOf(r)
	if dev == nil {
		u := userOf(r)
		if req.Password == "" || !identity.VerifyPassword(u.PasswordHash, req.Password) {
			time.Sleep(300 * time.Millisecond)
			writeErr(rw, 401, "forced takeover needs your password")
			return
		}
	}
	to := ServerSeat
	switch req.To {
	case "server", "":
	case "device":
		if dev == nil {
			writeErr(rw, 400, "only a device can force the workspace onto itself")
			return
		}
		to = DeviceSeat(dev.ID)
	default:
		writeErr(rw, 400, "to must be device or server")
		return
	}
	kind, id := actorOf(r)
	if err := s.ForceTakeover(rt, to, kind, id, "explicit"); err != nil {
		writeLeaseErr(rw, s, w.ID, dev, err)
		return
	}
	out, _ := s.leaseReply(w.ID, dev)
	writeJSON(rw, 200, out)
}

// handleCheckpointUpload stages a device checkpoint: a thin pack against
// ?base (P-1, §6.4 step 2), under refs/staging/<device>/<oid>. Any
// non-revoked device of a member may stage; the commit checks the lease.
func (s *Server) handleCheckpointUpload(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	if dev == nil {
		writeErr(rw, 403, "devices only")
		return
	}
	cp, base := r.URL.Query().Get("checkpoint"), r.URL.Query().Get("base")
	if !isOid(cp) || (base != "" && !isOid(base)) {
		writeErr(rw, 400, "bad checkpoint or base")
		return
	}
	if _, err := rt.cps.Git(nil, "cat-file", "-e", cp+"^{commit}"); err == nil {
		writeJSON(rw, 200, map[string]any{"staged": cp, "already": true})
		return
	}
	pack, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 2<<30))
	if err != nil {
		writeErr(rw, 400, "pack too large or unreadable")
		return
	}
	if err := rt.cps.ReceivePack(pack, "refs/staging/"+dev.ID+"/"+cp, cp); err != nil {
		// Typically a thin pack against a base the server no longer has:
		// the agent resends a full pack.
		writeJSON(rw, 400, map[string]string{"code": "incomplete", "error": err.Error()})
		return
	}
	writeJSON(rw, 200, map[string]any{"staged": cp})
}

// handleCommit is CommitCheckpoint for a device (§6.4 steps 3–6).
func (s *Server) handleCommit(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	if dev == nil {
		writeErr(rw, 403, "devices only")
		return
	}
	var req struct {
		Epoch      int64  `json:"epoch"`
		Parent     string `json:"parent"`
		Checkpoint string `json:"checkpoint"`
		Kind       string `json:"kind"`
	}
	if err := readJSON(r, &req); err != nil || !isOid(req.Checkpoint) || (req.Parent != "" && !isOid(req.Parent)) {
		writeErr(rw, 400, "bad request")
		return
	}
	if req.Kind != "flush" && req.Kind != "manual" {
		req.Kind = "auto"
	}
	if _, err := rt.cps.Git(nil, "cat-file", "-e", req.Checkpoint+"^{commit}"); err != nil {
		writeJSON(rw, 400, map[string]string{"code": "not_staged", "error": "upload the checkpoint pack first"})
		return
	}
	seq, err := s.commitCheckpoint(rt, req.Epoch, "device", dev.ID, req.Parent, req.Checkpoint, req.Kind)
	switch {
	case errors.Is(err, ErrLeaseLost):
		writeLeaseErr(rw, s, w.ID, dev, &LeaseError{Code: "lease_lost", AttemptedEpoch: req.Epoch, Msg: "this device does not hold the lease at that epoch"})
	case errors.Is(err, ErrParentMismatch):
		writeLeaseErr(rw, s, w.ID, dev, &LeaseError{Code: "parent_mismatch", AttemptedEpoch: req.Epoch, Msg: "current is not the checkpoint's parent"})
	case errors.Is(err, ErrHeadMissing):
		writeLeaseErr(rw, s, w.ID, dev, &LeaseError{Code: "head_missing", AttemptedEpoch: req.Epoch, Msg: "the checkpoint's HEAD commit is not in the repository; push refs first"})
	case err != nil:
		writeErr(rw, 500, err.Error())
	default:
		s.store.TouchDevice(dev.ID, store.Now())
		writeJSON(rw, 200, map[string]any{"seq": seq, "epoch": req.Epoch})
	}
}

// handleQuarantinePack serves a quarantine's checkpoint together with its
// base checkpoint, for `quarantine diff|apply|export` on a device.
func (s *Server) handleQuarantinePack(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	q, err := s.store.QuarantineByID(w.ID, r.PathValue("qid"))
	if err != nil {
		writeErr(rw, 404, "no such quarantine")
		return
	}
	revs := q.CheckpointID + "\n"
	base := ""
	if q.BaseCheckpointID != "" {
		if _, err := rt.cps.Git(nil, "cat-file", "-e", q.BaseCheckpointID+"^{commit}"); err == nil {
			revs += q.BaseCheckpointID + "\n"
			base = q.BaseCheckpointID
		}
	}
	pack, err := rt.cps.Git([]byte(revs), "pack-objects", "--revs", "--stdout", "-q")
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	rw.Header().Set("Content-Type", "application/x-git-packfile")
	rw.Header().Set("X-Armageddon-Checkpoint", q.CheckpointID)
	rw.Header().Set("X-Armageddon-Base", base)
	rw.Write(pack)
}

// handleQuarantineDrop clears a quarantine (`quarantine drop`, Q6).
func (s *Server) handleQuarantineDrop(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	qid := r.PathValue("qid")
	q, err := s.store.QuarantineByID(w.ID, qid)
	if err != nil {
		writeErr(rw, 404, "no such quarantine")
		return
	}
	if ok, err := s.store.ResolveQuarantine(w.ID, qid, "dropped", store.Now()); err != nil || !ok {
		writeErr(rw, 404, "no such quarantine")
		return
	}
	src := q.SourceDevice
	if q.SourceKind == "server" {
		src = "server"
	}
	rt.cps.Git(nil, "update-ref", "-d", fmt.Sprintf("refs/quarantine/%s/%s", src, qid))
	kind, id := actorOf(r)
	s.event(w.ID, kind, id, "quarantine.dropped", map[string]string{"id": qid})
	writeJSON(rw, 200, map[string]bool{"ok": true})
}

// handleReplicaReport mirrors a device's replica state for display (§3.3).
func (s *Server) handleReplicaReport(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	if dev == nil {
		writeErr(rw, 403, "devices only")
		return
	}
	var req struct {
		State        string `json:"state"`
		Mode         string `json:"mode"`
		AppliedSeq   int64  `json:"applied_seq"`
		Pending      int64  `json:"pending"`
		AgentVersion string `json:"agent_version"`
	}
	if err := readJSON(r, &req); err != nil || (req.Mode != "follow" && req.Mode != "write") || len(req.State) > 40 {
		writeErr(rw, 400, "bad request")
		return
	}
	if err := s.store.UpsertReplica(&store.Replica{WorkspaceID: w.ID, DeviceID: dev.ID, State: strings.ToUpper(req.State), Mode: req.Mode,
		LastAppliedSeq: req.AppliedSeq, Pending: req.Pending, LastSeenAt: store.Now(), AgentVersion: req.AgentVersion}); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]bool{"ok": true})
}

func (s *Server) handleReplicas(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rs, err := s.store.Replicas(w.ID)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, x := range rs {
		out = append(out, map[string]any{"device_id": x.DeviceID, "device_name": x.DeviceName, "state": x.State, "mode": x.Mode,
			"applied_seq": x.LastAppliedSeq, "pending": x.Pending, "last_seen_at": x.LastSeenAt})
	}
	writeJSON(rw, 200, out)
}
