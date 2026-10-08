package server

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Scheduled fsck (contract §10 F10): every workspace's repo.git (as the
// workspace user) and checkpoints.git (as the server) get a connectivity
// check. A failure marks the workspace DEGRADED with reason repo_corrupt;
// `armageddon workspace repair --from-device` brings the missing objects
// back from a replica and returns it to READY.

// ReasonRepoCorrupt prefixes the state_reason of a workspace degraded by F10.
const ReasonRepoCorrupt = "repo_corrupt"

// fsckRound tracks the scheduled fsck rounds of one server.
type fsckRound struct {
	sync.Mutex
	running bool
	last    time.Time
}

// scheduledFsck starts a background fsck round when one is due.
func (s *Server) scheduledFsck() {
	every := s.cfg.FsckInterval()
	if every == 0 {
		return
	}
	s.fsckRound.Lock()
	if s.fsckRound.last.IsZero() {
		s.fsckRound.last = time.Now() // the first round runs one interval after startup
	}
	if s.fsckRound.running || time.Since(s.fsckRound.last) < every {
		s.fsckRound.Unlock()
		return
	}
	s.fsckRound.running = true
	s.fsckRound.Unlock()
	go func() {
		defer func() {
			s.fsckRound.Lock()
			s.fsckRound.running, s.fsckRound.last = false, time.Now()
			s.fsckRound.Unlock()
		}()
		ws, err := s.store.AllWorkspaces()
		if err != nil {
			return
		}
		for _, w := range ws {
			if w.State != StateReady {
				continue
			}
			if rt := s.runtimeFor(w.ID); rt != nil {
				if problem := s.fsck(rt); problem != "" {
					s.degradeCorrupt(w.ID, problem)
				}
			}
		}
	}()
}

// fsck checks both repositories of a workspace and returns the first
// problem, or "".
func (s *Server) fsck(rt *runtime) string {
	out, err := rt.acct.Command(rt.p.Repo, "git", "--git-dir="+rt.p.Repo, "fsck", "--no-progress", "--connectivity-only").CombinedOutput()
	if err != nil && !onlyMissingEmptyTree(out) {
		return "repo.git: " + firstProblem(out, err)
	}
	if out, err := rt.cps.Git(nil, "fsck", "--no-progress", "--connectivity-only"); err != nil && !onlyMissingEmptyTree(out) {
		return "checkpoints.git: " + firstProblem(out, err)
	}
	return ""
}

func (s *Server) degradeCorrupt(id, problem string) {
	reason := ReasonRepoCorrupt + ": " + problem
	if err := s.store.SetWorkspaceState(id, StateReady, StateDegraded, reason, store.Now()); err == nil {
		log.Printf("workspace %s: DEGRADED (%s); restore the objects with `armageddon workspace repair --from-device` from a replica, or from a backup", id, reason)
		s.event(id, "server", "", "workspace.degraded", map[string]string{"reason": ReasonRepoCorrupt, "detail": problem})
	}
}

// onlyMissingEmptyTree: workspaces created by v0.1.0-mvp referenced Git's
// implicit empty tree without writing it; harmless (doctor --repair).
func onlyMissingEmptyTree(out []byte) bool {
	found := false
	for _, l := range strings.Split(string(out), "\n") {
		switch l = strings.TrimSpace(l); {
		case l == "" || strings.HasPrefix(l, "notice:") || strings.HasPrefix(l, "dangling "):
		case l == "missing tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904":
			found = true
		default:
			return false
		}
	}
	return found
}

func firstProblem(out []byte, err error) string {
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "notice:") && !strings.HasPrefix(l, "dangling ") {
			return l
		}
	}
	if err != nil {
		return err.Error()
	}
	return "fsck failed"
}
