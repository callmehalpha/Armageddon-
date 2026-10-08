package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Recovery from a device replica (contract §10 F9, F10). A follower
// replica holds the full history and the latest working state, which is
// what makes every follower a disaster-recovery copy:
//
//   - F9 (b) `armageddon workspace seed --from-replica`: a new server gets a
//     new workspace built from the replica's refs and working state.
//   - F10 `armageddon workspace repair --from-device`: objects missing or
//     lost from a workspace's repo.git come back from the replica.
//
// Both receive packs from a device and index them as the workspace user
// with `index-pack --strict`, so the objects are checked like a push
// (receive.fsckObjects) and the server never trusts the device's bytes.

// maxRecoveryPack bounds a history or checkpoint pack (as for uploads).
const maxRecoveryPack = 2 << 30

// seedData is what a seed carries into importWorkspace.
type seedData struct {
	device     string
	headRef    string            // symbolic HEAD, or "" (detached)
	refs       map[string]string // refs/heads/* and refs/tags/* → oid
	history    []byte            // pack: everything reachable from refs and HEAD
	checkpoint string            // the replica's working state, as a checkpoint
	cpPack     []byte            // full pack of checkpoint
}

var seedRefRe = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9._/@+-]+$`)

// validSeedRef rejects anything but plain branch and tag names (no "..",
// no lock files, nothing Git would refuse or a hook could misread).
func validSeedRef(ref string) bool {
	return seedRefRe.MatchString(ref) && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") &&
		!strings.HasSuffix(ref, ".lock") && !strings.HasSuffix(ref, "/") && !strings.Contains(ref, "/.")
}

// handleSeed: POST /api/workspaces/seed (devices only), multipart with
// "meta" (JSON), "history" (pack) and "checkpoint" (pack). It creates the
// workspace and imports it in the background, like any new workspace.
func (s *Server) handleSeed(rw http.ResponseWriter, r *http.Request) {
	dev := deviceOf(r)
	if dev == nil {
		writeErr(rw, 403, "seed from a device replica: `armageddon workspace seed --from-replica`")
		return
	}
	if s.checkDisk() {
		writeJSON(rw, http.StatusInsufficientStorage, map[string]string{"code": ReasonDiskFull, "error": "the server's disk is almost full"})
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(rw, 400, "multipart body expected")
		return
	}
	var meta struct {
		Name       string            `json:"name"`
		HeadRef    string            `json:"head_ref"`
		Refs       map[string]string `json:"refs"`
		Checkpoint string            `json:"checkpoint"`
	}
	sd := &seedData{device: dev.ID}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(rw, 400, "bad multipart body")
			return
		}
		b, err := io.ReadAll(io.LimitReader(p, maxRecoveryPack+1))
		if err != nil || len(b) > maxRecoveryPack {
			writeErr(rw, 400, "part too large or unreadable")
			return
		}
		switch p.FormName() {
		case "meta":
			if err := json.Unmarshal(b, &meta); err != nil {
				writeErr(rw, 400, "bad meta")
				return
			}
		case "history":
			sd.history = b
		case "checkpoint":
			sd.cpPack = b
		}
	}
	if strings.TrimSpace(meta.Name) == "" || len(meta.Refs) == 0 || len(meta.Refs) > 100000 || len(sd.history) == 0 {
		writeErr(rw, 400, "name, refs and history are required")
		return
	}
	for ref, oid := range meta.Refs {
		if !validSeedRef(ref) || !isOid(oid) {
			writeErr(rw, 400, "bad ref "+ref)
			return
		}
	}
	if meta.HeadRef != "" {
		if _, ok := meta.Refs[meta.HeadRef]; !ok {
			writeErr(rw, 400, "head_ref must be one of refs")
			return
		}
	}
	if meta.Checkpoint != "" && (!isOid(meta.Checkpoint) || len(sd.cpPack) == 0) {
		writeErr(rw, 400, "bad checkpoint")
		return
	}
	sd.headRef, sd.refs, sd.checkpoint = meta.HeadRef, meta.Refs, meta.Checkpoint
	w, err := s.createWorkspace(userOf(r), meta.Name, "", sd)
	if err != nil {
		writeErr(rw, 400, err.Error())
		return
	}
	writeJSON(rw, 202, s.wsJSON(w, "owner", dev))
}

// importSeedRepo fills a new repo.git from the seed's history pack.
func (s *Server) importSeedRepo(rt *runtime, sd *seedData, run func(dir string, args ...string) error) error {
	branch := "main"
	if strings.HasPrefix(sd.headRef, "refs/heads/") {
		branch = strings.TrimPrefix(sd.headRef, "refs/heads/")
	}
	if err := run(rt.p.Repo, "init", "--bare", "--quiet", "-b", branch); err != nil {
		return err
	}
	if out, err := s.indexPackAsWorkspace(rt, sd.history); err != nil {
		return fmt.Errorf("history pack refused: %v: %s", err, out)
	}
	var upd bytes.Buffer
	for ref, oid := range sd.refs {
		fmt.Fprintf(&upd, "create %s %s\n", ref, oid)
	}
	cmd := rt.acct.Command(rt.p.Repo, "git", "update-ref", "--stdin")
	cmd.Stdin = &upd
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("refs: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if sd.headRef == "" {
		// Detached on the replica: HEAD names the default branch here; the
		// seeded checkpoint moves it.
		for ref := range sd.refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				return run(rt.p.Repo, "symbolic-ref", "HEAD", ref)
			}
		}
	}
	return nil
}

// indexPackAsWorkspace stores a pack in repo.git as the workspace user,
// checked like a push (§2.5: Git on repo.git never runs as the server).
func (s *Server) indexPackAsWorkspace(rt *runtime, pack []byte) (string, error) {
	cmd := rt.acct.Command(rt.p.Repo, "git", "--git-dir="+rt.p.Repo, "index-pack", "--stdin", "--strict")
	cmd.Stdin = bytes.NewReader(pack)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// applySeedCheckpoint brings the server seat to the replica's working
// state (uncommitted edits, staged changes, preserved .env files) and
// commits it as the workspace's next checkpoint.
func (s *Server) applySeedCheckpoint(rt *runtime, sd *seedData) error {
	if sd.checkpoint == "" {
		return nil
	}
	if err := rt.cps.ReceivePack(sd.cpPack, "refs/staging/seed", sd.checkpoint); err != nil {
		return fmt.Errorf("checkpoint pack: %w", err)
	}
	defer rt.cps.Git(nil, "update-ref", "-d", "refs/staging/seed")
	meta, err := rt.cps.ReadMeta(sd.checkpoint)
	if err != nil {
		return err
	}
	if meta.HeadOid != "" {
		if err := rt.acct.Command(rt.p.Repo, "git", "cat-file", "-e", meta.HeadOid+"^{commit}").Run(); err != nil {
			return fmt.Errorf("the checkpoint's HEAD %s is not in the history sent", meta.HeadOid)
		}
	}
	err = func() error {
		rt.seatMu.Lock()
		defer rt.seatMu.Unlock()
		w, err := s.store.WorkspaceByID(rt.id)
		if err != nil {
			return err
		}
		pack, err := rt.cps.PackSince(sd.checkpoint, "")
		if err != nil {
			return err
		}
		if err := rt.seat.ReceivePack(pack, "refs/seat/applied", sd.checkpoint); err != nil {
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
		// From the first checkpoint (the fresh worktree), verified like any
		// apply; then the server seat captures it as its own.
		return s.seatApply(rt, w.CurrentCheckpoint, sd.checkpoint)
	}()
	if err != nil {
		return err
	}
	_, err = s.captureOnce(rt)
	return err
}

// handleRepair: POST /api/workspaces/{id}/repair (devices of members).
// The body is a pack of everything reachable from the replica's branches
// and tags. Objects already present are kept; missing ones are added.
// If both repositories then pass fsck, a workspace DEGRADED by F10
// returns to READY.
func (s *Server) handleRepair(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	dev := deviceOf(r)
	if dev == nil {
		writeErr(rw, 403, "repair from a device replica: `armageddon workspace repair --from-device`")
		return
	}
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	if s.refuseIfDiskFull(rw, w) {
		return
	}
	pack, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxRecoveryPack))
	if err != nil || len(pack) == 0 {
		writeErr(rw, 400, "pack too large, empty or unreadable")
		return
	}
	if out, err := s.indexPackAsWorkspace(rt, pack); err != nil {
		writeJSON(rw, 400, map[string]string{"code": "pack_refused", "error": fmt.Sprintf("%v: %s", err, out)})
		return
	}
	// Loose copies of objects now also packed may be the damaged ones.
	rt.acct.Command(rt.p.Repo, "git", "--git-dir="+rt.p.Repo, "prune-packed").Run()
	problem := s.fsck(rt)
	state := w.State
	if problem == "" && w.State == StateDegraded && strings.HasPrefix(w.StateReason, ReasonRepoCorrupt) {
		if err := s.store.SetWorkspaceState(w.ID, StateDegraded, StateReady, "", store.Now()); err == nil {
			state = StateReady
		}
	}
	s.event(w.ID, "device", dev.ID, "workspace.repaired", map[string]any{"fsck": problem == "", "problem": problem, "state": state})
	log.Printf("workspace %s: repair from device %s: fsck %s", w.ID, dev.ID, map[bool]string{true: "clean", false: problem}[problem == ""])
	writeJSON(rw, 200, map[string]any{"fsck_clean": problem == "", "problem": problem, "state": state})
}
