package server

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// Identity
	mux.HandleFunc("GET /api/setup", s.handleSetupStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/invites/accept", s.handleAcceptInvite)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/invites", s.requireAdmin(s.handleCreateInvite))
	mux.HandleFunc("GET /api/users", s.requireUser(s.handleUsers))
	// Devices
	mux.HandleFunc("POST /api/pair/start", s.handlePairStart)
	mux.HandleFunc("POST /api/pair/poll", s.handlePairPoll)
	mux.HandleFunc("GET /api/pair/lookup", s.requireUser(s.handlePairLookup))
	mux.HandleFunc("POST /api/pair/approve", s.requireUser(s.handlePairApprove))
	mux.HandleFunc("POST /api/auth/challenge", s.handleChallenge)
	mux.HandleFunc("POST /api/auth/token", s.handleToken)
	mux.HandleFunc("GET /api/devices", s.requireUser(s.handleDevices))
	mux.HandleFunc("DELETE /api/devices/{id}", s.requireUser(s.handleRevokeDevice))
	// Workspaces
	mux.HandleFunc("GET /api/workspaces", s.requireUser(s.handleListWorkspaces))
	mux.HandleFunc("POST /api/workspaces", s.requireUser(s.handleCreateWorkspace))
	mux.HandleFunc("GET /api/workspaces/{id}", s.requireUser(s.member(s.handleGetWorkspace)))
	mux.HandleFunc("GET /api/workspaces/{id}/checkpoints", s.requireUser(s.member(s.handleCheckpoints)))
	mux.HandleFunc("GET /api/workspaces/{id}/events", s.requireUser(s.member(s.handleEvents)))
	mux.HandleFunc("GET /api/workspaces/{id}/members", s.requireUser(s.member(s.handleMembers)))
	mux.HandleFunc("POST /api/workspaces/{id}/members", s.requireUser(s.member(s.handleAddMember)))
	mux.HandleFunc("GET /api/workspaces/{id}/current", s.requireUser(s.member(s.handleCurrent)))
	mux.HandleFunc("GET /api/workspaces/{id}/checkpoints/{seq}/pack", s.requireUser(s.member(s.handlePack)))
	mux.HandleFunc("POST /api/workspaces/{id}/quarantines", s.requireUser(s.member(s.handleQuarantineUpload)))
	mux.HandleFunc("GET /api/workspaces/{id}/quarantines", s.requireUser(s.member(s.handleQuarantines)))
	mux.HandleFunc("POST /api/workspaces/{id}/sync", s.requireUser(s.member(s.handleSyncNow)))
	// Local write mode (M7): lease, device checkpoint upload/commit, quarantines, replicas
	mux.HandleFunc("POST /api/workspaces/{id}/lease/acquire", s.requireUser(s.member(s.handleAcquire)))
	mux.HandleFunc("POST /api/workspaces/{id}/lease/release", s.requireUser(s.member(s.handleRelease)))
	mux.HandleFunc("POST /api/workspaces/{id}/lease/heartbeat", s.requireUser(s.member(s.handleHeartbeat)))
	mux.HandleFunc("POST /api/workspaces/{id}/lease/force", s.requireUser(s.member(s.handleForce)))
	mux.HandleFunc("POST /api/workspaces/{id}/checkpoints/pack", s.requireUser(s.member(s.handleCheckpointUpload)))
	mux.HandleFunc("POST /api/workspaces/{id}/checkpoints/commit", s.requireUser(s.member(s.handleCommit)))
	mux.HandleFunc("GET /api/workspaces/{id}/quarantines/{qid}/pack", s.requireUser(s.member(s.handleQuarantinePack)))
	mux.HandleFunc("DELETE /api/workspaces/{id}/quarantines/{qid}", s.requireUser(s.member(s.handleQuarantineDrop)))
	mux.HandleFunc("POST /api/workspaces/{id}/replica", s.requireUser(s.member(s.handleReplicaReport)))
	mux.HandleFunc("GET /api/workspaces/{id}/replicas", s.requireUser(s.member(s.handleReplicas)))
	mux.HandleFunc("GET /api/workspaces/{id}/terminal", s.requireUser(s.member(s.handleTerminal)))
	// Browser IDE: every method and subpath, proxied to the workspace's
	// code-server after the same user + membership checks.
	mux.HandleFunc("/api/workspaces/{id}/ide/", s.requireUser(s.member(s.handleIDE)))
	// Git-provider credentials and the SSH endpoint
	mux.HandleFunc("GET /api/credentials", s.requireUser(s.handleListCredentials))
	mux.HandleFunc("POST /api/credentials", s.requireUser(s.handleCreateCredential))
	mux.HandleFunc("DELETE /api/credentials/{id}", s.requireUser(s.handleDeleteCredential))
	mux.HandleFunc("POST /api/admin/data-key/rotate", s.requireAdmin(s.handleRotateDataKey))
	mux.HandleFunc("GET /api/ssh", s.requireUser(s.handleSSHInfo))
	// Git and web UI
	mux.HandleFunc("/git/", s.serveGit)
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("ok\n")) })
	mux.Handle("/", webHandler())
	return securityHeaders(s.withSession(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(rw, r)
	})
}

type wsHandler func(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string)

// member resolves {id} to a workspace the caller belongs to; non-members get
// 404, never 403 (§7.4).
func (s *Server) member(h wsHandler) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		w, role, err := s.store.WorkspaceForMember(r.PathValue("id"), userOf(r).ID)
		if err != nil {
			writeErr(rw, 404, "no such workspace")
			return
		}
		h(rw, r, w, role)
	}
}

func (s *Server) wsJSON(w *store.Workspace, role string, caller *store.Device) map[string]any {
	out := map[string]any{"id": w.ID, "name": w.Name, "slug": w.Slug, "state": w.State, "state_reason": w.StateReason,
		"source_url": w.SourceURL, "role": role, "checkpoint_seq": w.CheckpointSeq, "current_checkpoint": w.CurrentCheckpoint,
		"git_url": s.cfg.PublicURL + "/git/" + w.ID + ".git", "created_at": w.CreatedAt,
		"ide_path": "/api/workspaces/" + w.ID + "/ide/", "ide_running": s.ide.running(w.ID)}
	if l, err := s.store.LeaseOf(nil, w.ID); err == nil {
		out["lease"] = s.leaseJSON(l, caller)
	}
	return out
}

func (s *Server) handleListWorkspaces(rw http.ResponseWriter, r *http.Request) {
	ws, err := s.store.WorkspacesForUser(userOf(r).ID)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, w := range ws {
		_, role, _ := s.store.WorkspaceForMember(w.ID, userOf(r).ID)
		out = append(out, s.wsJSON(w, role, deviceOf(r)))
	}
	writeJSON(rw, 200, out)
}

func (s *Server) handleCreateWorkspace(rw http.ResponseWriter, r *http.Request) {
	if deviceOf(r) != nil {
		writeErr(rw, 403, "create workspaces from the web UI")
		return
	}
	var req struct {
		Name      string `json:"name"`
		SourceURL string `json:"source_url"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	w, err := s.CreateWorkspace(userOf(r), req.Name, strings.TrimSpace(req.SourceURL))
	if err != nil {
		writeErr(rw, 400, err.Error())
		return
	}
	writeJSON(rw, 201, s.wsJSON(w, "owner", nil))
}

func (s *Server) handleGetWorkspace(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	writeJSON(rw, 200, s.wsJSON(w, role, deviceOf(r)))
}

func (s *Server) handleCheckpoints(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	cps, err := s.store.Checkpoints(w.ID, 100)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, c := range cps {
		out = append(out, map[string]any{"seq": c.Seq, "id": c.ID, "epoch": c.Epoch, "author": c.AuthorKind,
			"head_ref": c.HeadRef, "head_oid": c.HeadOid, "kind": c.Kind, "created_at": c.CreatedAt})
	}
	writeJSON(rw, 200, out)
}

func (s *Server) handleEvents(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	evs, err := s.store.Events(w.ID, 100)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, evs)
}

func (s *Server) handleMembers(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	ms, err := s.store.Members(w.ID)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	out := []map[string]string{}
	for _, m := range ms {
		out = append(out, map[string]string{"user_id": m[0], "username": m[1], "role": m[2]})
	}
	writeJSON(rw, 200, out)
}

func (s *Server) handleAddMember(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	if role != "owner" {
		writeErr(rw, 403, "only owners can add members")
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(rw, 400, "bad request")
		return
	}
	u, err := s.store.UserByName(strings.ToLower(strings.TrimSpace(req.Username)))
	if err != nil {
		writeErr(rw, 404, "no such user (invite them first)")
		return
	}
	if err := s.store.AddMember(w.ID, u.ID, "member", userOf(r).ID, store.Now()); err != nil {
		writeErr(rw, 409, "already a member")
		return
	}
	s.event(w.ID, "user", userOf(r).ID, "member.added", map[string]string{"username": u.Username})
	writeJSON(rw, 200, map[string]bool{"ok": true})
}

// handlePack serves checkpoint <seq> as a thin pack against ?base=<oid>
// (P-1). If the server does not have base, it sends a full pack and says so.
func (s *Server) handlePack(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.runtimeFor(w.ID)
	if rt == nil {
		writeErr(rw, 503, "workspace not running")
		return
	}
	seq, _ := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	cp, err := s.store.CheckpointBySeq(w.ID, seq)
	if err != nil {
		writeErr(rw, 404, "no such checkpoint")
		return
	}
	base := r.URL.Query().Get("base")
	full := "0"
	if base != "" {
		if _, err := rt.cps.Git(nil, "cat-file", "-e", base+"^{tree}"); err != nil || !isOid(base) {
			base, full = "", "1"
		}
	} else {
		full = "1"
	}
	pack, err := rt.cps.PackSince(cp.ID, base)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	rw.Header().Set("Content-Type", "application/x-git-packfile")
	rw.Header().Set("X-Armageddon-Checkpoint", cp.ID)
	rw.Header().Set("X-Armageddon-Full", full)
	rw.Write(pack)
}

func isOid(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// handleQuarantineUpload stores a device's diverged local state (I4): the
// body is a pack containing ?checkpoint=<oid>, thin against ?base=<oid>.
func (s *Server) handleQuarantineUpload(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	rt := s.runtimeFor(w.ID)
	if dev == nil || rt == nil {
		writeErr(rw, 403, "devices only")
		return
	}
	cp, base := r.URL.Query().Get("checkpoint"), r.URL.Query().Get("base")
	reason := r.URL.Query().Get("reason")
	if !isOid(cp) || (base != "" && !isOid(base)) {
		writeErr(rw, 400, "bad checkpoint or base")
		return
	}
	if q, err := s.store.QuarantineByCheckpoint(w.ID, dev.ID, cp); err == nil {
		writeJSON(rw, 200, map[string]string{"id": q.ID}) // retry of an upload that landed
		return
	}
	// Per-device quota (§6.7): refused, never evicted (Q6).
	if n, err := s.store.OpenQuarantinesOfDevice(w.ID, dev.ID); err != nil || n >= s.quarantineQuota() {
		writeJSON(rw, http.StatusInsufficientStorage, map[string]string{"code": "quarantine_quota",
			"error": fmt.Sprintf("this device has %d uncleared quarantines in this workspace (quota %d). Review them with `armageddon quarantine list` and clear old ones with `armageddon quarantine drop`; nothing is evicted automatically", n, s.quarantineQuota())})
		return
	}
	pack, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 2<<30))
	if err != nil {
		writeErr(rw, 400, "pack too large or unreadable")
		return
	}
	qid := ids.New()
	ref := fmt.Sprintf("refs/quarantine/%s/%s", dev.ID, qid)
	if err := rt.cps.ReceivePack(pack, ref, cp); err != nil {
		writeErr(rw, 400, err.Error())
		return
	}
	if err := s.store.InsertQuarantine(&store.Quarantine{ID: qid, WorkspaceID: w.ID, SourceKind: "device", SourceDevice: dev.ID,
		CheckpointID: cp, BaseCheckpointID: base, Reason: reason, CreatedAt: store.Now()}); err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	s.event(w.ID, "device", dev.ID, "quarantine.created", map[string]string{"id": qid, "reason": reason})
	writeJSON(rw, 201, map[string]string{"id": qid})
}

func (s *Server) handleQuarantines(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	qs, err := s.store.Quarantines(w.ID)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	if qs == nil {
		qs = []*store.Quarantine{}
	}
	writeJSON(rw, 200, qs)
}

// handleSyncNow forces an immediate server-seat capture (`armageddon sync`).
func (s *Server) handleSyncNow(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.runtimeFor(w.ID)
	if rt == nil {
		writeErr(rw, 503, "workspace not running")
		return
	}
	seq, err := s.captureOnce(rt)
	if err != nil {
		writeErr(rw, 500, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]int64{"new_seq": seq})
}
