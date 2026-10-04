package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
	"github.com/callmehalpha/Armageddon-/internal/sysuser"
	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// Workspace lifecycle states (contract §3.1).
const (
	StateCreating  = "creating"
	StateImporting = "importing"
	StateReady     = "ready"
	StateDegraded  = "degraded"
	StateFailed    = "failed"
)

// paths is the on-disk layout of one workspace (contract §2.4).
type paths struct {
	Root, Repo, Tree, Home, SeatDir, SeatShadow, SeatIndex, Checkpoints, Hooks string
}

func (s *Server) pathsFor(id string) paths {
	root := filepath.Join(s.cfg.DataDir, "workspaces", id)
	return paths{
		Root:        root,
		Repo:        filepath.Join(root, "repo.git"),
		Tree:        filepath.Join(root, "tree"),
		Home:        filepath.Join(root, "home"),
		SeatDir:     filepath.Join(root, "seat"),
		SeatShadow:  filepath.Join(root, "seat", "shadow.git"),
		SeatIndex:   filepath.Join(root, "seat", "capture.index"),
		Checkpoints: filepath.Join(root, "checkpoints.git"),
		Hooks:       filepath.Join(root, "hooks"),
	}
}

// runtime is the in-memory side of a workspace: the authority's locks, the
// capture loop and long-poll notification.
type runtime struct {
	id   string
	p    paths
	acct *sysuser.Account
	seat *gitshadow.Shadow // server seat capture, runs as the workspace user
	cps  *gitshadow.Shadow // checkpoints.git, server-owned (WorkTree unused)

	// fence: receive-pack holds it as a reader, lease transitions as a writer (§4.2).
	fence sync.RWMutex
	// commitMu serialises authority commands for this workspace (§4.1).
	commitMu sync.Mutex

	notifyMu sync.Mutex
	notify   chan struct{} // closed and replaced on every committed checkpoint

	cancel context.CancelFunc
}

func (rt *runtime) changed() <-chan struct{} {
	rt.notifyMu.Lock()
	defer rt.notifyMu.Unlock()
	return rt.notify
}

func (rt *runtime) broadcast() {
	rt.notifyMu.Lock()
	close(rt.notify)
	rt.notify = make(chan struct{})
	rt.notifyMu.Unlock()
}

func (rt *runtime) stop() {
	if rt.cancel != nil {
		rt.cancel()
	}
}

func (s *Server) runtimeFor(id string) *runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rts[id]
}

func (s *Server) newRuntime(w *store.Workspace) (*runtime, error) {
	p := s.pathsFor(w.ID)
	acct, err := sysuser.Ensure(w.OSUser, p.Home)
	if err != nil {
		return nil, err
	}
	rt := &runtime{id: w.ID, p: p, acct: acct, notify: make(chan struct{})}
	rt.seat = &gitshadow.Shadow{GitDir: p.SeatShadow, WorkTree: p.Tree, IndexFile: p.SeatIndex,
		Preserve: []string{".env*"}, Prepare: func(c *exec.Cmd) { acct.Prepare(c) }}
	rt.cps = &gitshadow.Shadow{GitDir: p.Checkpoints, WorkTree: p.Root, IndexFile: filepath.Join(p.Root, "cps.index")}
	return rt, nil
}

// startWorkspaces brings up runtimes for every ready workspace and runs
// startup reconciliation (§6.4: the database is truth for "current").
func (s *Server) startWorkspaces(ctx context.Context) error {
	all, err := s.store.AllWorkspaces()
	if err != nil {
		return err
	}
	for _, w := range all {
		if w.State != StateReady {
			continue
		}
		rt, err := s.newRuntime(w)
		if err != nil {
			log.Printf("workspace %s: %v", w.ID, err)
			continue
		}
		if w.CurrentCheckpoint != "" {
			// Recreate refs/checkpoints/current from the DB if a crash left it behind.
			rt.cps.Git(nil, "update-ref", "refs/checkpoints/current", w.CurrentCheckpoint)
		}
		s.mu.Lock()
		s.rts[w.ID] = rt
		s.mu.Unlock()
		s.startCaptureLoop(ctx, rt)
	}
	return nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "workspace"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// CreateWorkspace registers a workspace and imports it in the background.
func (s *Server) CreateWorkspace(owner *store.User, name, sourceURL string) (*store.Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	if sourceURL != "" && !validSourceURL(sourceURL) {
		return nil, errors.New("source URL must be https://, http:// or ssh (git@host:path)")
	}
	now := store.Now()
	id := ids.New()
	w := &store.Workspace{ID: id, OwnerID: owner.ID, Name: name, Slug: slugify(name), State: StateCreating,
		SourceKind: "empty", SourceURL: sourceURL, OSUser: sysuser.NameFor(id), CreatedAt: now, UpdatedAt: now}
	if sourceURL != "" {
		w.SourceKind = "clone"
	}
	if err := s.store.Tx(context.Background(), func(tx *sql.Tx) error { return s.store.CreateWorkspace(tx, w) }); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("you already have a workspace with that name")
		}
		return nil, err
	}
	s.event(id, "user", owner.ID, "workspace.created", map[string]string{"name": name, "source": sourceURL})
	go func() {
		if err := s.importWorkspace(w, owner); err != nil {
			log.Printf("workspace %s import failed: %v", id, err)
			s.store.SetWorkspaceState(id, StateImporting, StateFailed, err.Error(), store.Now())
			s.event(id, "server", "", "workspace.failed", map[string]string{"error": err.Error()})
		}
	}()
	return w, nil
}

func validSourceURL(u string) bool {
	if strings.HasPrefix(u, "-") {
		return false // never let a URL be parsed as a git option
	}
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") ||
		regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^ ]+$`).MatchString(u) || strings.HasPrefix(u, "ssh://")
}

// importWorkspace builds the on-disk workspace. Every Git command touching
// workspace-writable data runs as the workspace user (§2.5).
func (s *Server) importWorkspace(w *store.Workspace, owner *store.User) error {
	if err := s.store.SetWorkspaceState(w.ID, StateCreating, StateImporting, "", store.Now()); err != nil {
		return err
	}
	rt, err := s.newRuntime(w)
	if err != nil {
		return err
	}
	p, a := rt.p, rt.acct
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	for _, d := range []string{p.Home, p.SeatDir} {
		if err := a.MkdirOwned(d, 0o700); err != nil {
			return err
		}
	}
	// Workspace user's Git identity and a guard against inherited config.
	gitcfg := fmt.Sprintf("[user]\n\tname = %s\n\temail = %s@armageddon.local\n[init]\n\tdefaultBranch = main\n", owner.Username, owner.Username)
	if err := os.WriteFile(filepath.Join(p.Home, ".gitconfig"), []byte(gitcfg), 0o600); err != nil {
		return err
	}
	a.Chown(filepath.Join(p.Home, ".gitconfig"))

	run := func(dir string, args ...string) error {
		cmd := a.Command(dir, "git", args...)
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// The workspace dir itself is root-owned; let the workspace user create
	// repo.git and tree inside a directory it owns.
	if err := a.MkdirOwned(p.Repo, 0o700); err != nil {
		return err
	}
	if w.SourceKind == "clone" {
		if err := run(p.Root, "clone", "--bare", "--quiet", "--", w.SourceURL, p.Repo); err != nil {
			return err
		}
		if err := run(p.Repo, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return err
		}
	} else {
		if err := run(p.Repo, "init", "--bare", "--quiet", "-b", "main"); err != nil {
			return err
		}
		// An initial empty commit so HEAD, the index delta and replicas all
		// have a commit to work against.
		cmd := a.Command(p.Repo, "git", "commit-tree", gitshadow.EmptyTree, "-m", "Initial commit (created by Armageddon)")
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("initial commit: %w", err)
		}
		if err := run(p.Repo, "update-ref", "refs/heads/main", strings.TrimSpace(string(out))); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{{"core.logAllRefUpdates", "always"}, {"gc.auto", "0"}, {"core.hooksPath", p.Hooks}} {
		if err := run(p.Repo, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	head, err := a.Command(p.Repo, "git", "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("default branch: %w", err)
	}
	if err := a.MkdirOwned(p.Tree, 0o700); err != nil {
		return err
	}
	// p.Tree exists, empty and owned by the workspace user: the user cannot
	// create directories in the root-owned workspace directory itself.
	if err := run(p.Repo, "worktree", "add", "--quiet", p.Tree, strings.TrimSpace(string(head))); err != nil {
		return err
	}
	if err := s.writeHooks(rt); err != nil {
		return err
	}
	if err := gitshadow.Init(p.SeatShadow, rt.seat.Prepare); err != nil {
		return fmt.Errorf("seat shadow: %w", err)
	}
	if err := gitshadow.Init(p.Checkpoints, nil); err != nil {
		return fmt.Errorf("checkpoints repo: %w", err)
	}
	os.Chmod(p.Checkpoints, 0o700)
	if err := s.store.SetWorkspaceState(w.ID, StateImporting, StateReady, "", store.Now()); err != nil {
		return err
	}
	s.mu.Lock()
	s.rts[w.ID] = rt
	s.mu.Unlock()
	s.event(w.ID, "server", "", "workspace.ready", nil)
	// First checkpoint right away, then the loop.
	if _, err := s.captureOnce(rt); err != nil {
		log.Printf("workspace %s: first checkpoint: %v", w.ID, err)
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.startCaptureLoop(ctx, rt)
	return nil
}

// writeHooks installs the server-owned hooks (trash refs, §5.3–§5.4).
func (s *Server) writeHooks(rt *runtime) error {
	if err := os.MkdirAll(rt.p.Hooks, 0o755); err != nil {
		return err
	}
	for _, h := range []string{"post-receive", "reference-transaction"} {
		script := fmt.Sprintf("#!/bin/sh\nexec %q hook %s \"$@\"\n", s.hookBin, h)
		if err := os.WriteFile(filepath.Join(rt.p.Hooks, h), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ---- server seat capture & authority commit ----

func (s *Server) startCaptureLoop(ctx context.Context, rt *runtime) {
	cctx, cancel := context.WithCancel(ctx)
	rt.cancel = cancel
	interval := time.Duration(s.cfg.CaptureIntervalMS) * time.Millisecond
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		failures := 0
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
			}
			if _, err := s.captureOnce(rt); err != nil {
				failures++
				if failures == 1 || failures%30 == 0 {
					log.Printf("workspace %s: capture: %v", rt.id, err)
				}
			} else {
				failures = 0
			}
		}
	}()
}

// captureOnce captures the server seat and commits a checkpoint if the
// working state changed. Returns the new sequence number (0 if unchanged).
func (s *Server) captureOnce(rt *runtime) (int64, error) {
	w, err := s.store.WorkspaceByID(rt.id)
	if err != nil {
		return 0, err
	}
	if w.State != StateReady {
		return 0, nil
	}
	lease, err := s.store.LeaseOf(nil, rt.id)
	if err != nil {
		return 0, err
	}
	if lease.HolderKind != "server" {
		return 0, nil // the server seat is a follower while a device holds the lease
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
	return s.commitCheckpoint(rt, lease.Epoch, "server", "", w.CurrentCheckpoint, cp, "auto")
}

var (
	ErrLeaseLost      = errors.New("lease_lost")
	ErrParentMismatch = errors.New("parent_mismatch")
)

// commitCheckpoint is the authority's commit command (§6.4): epoch check,
// parent CAS, sequence assignment, then ref update and notification.
func (s *Server) commitCheckpoint(rt *runtime, epoch int64, authorKind, authorDevice, parent, cp, kind string) (int64, error) {
	rt.commitMu.Lock()
	defer rt.commitMu.Unlock()
	meta, err := rt.cps.ReadMeta(cp)
	if err != nil {
		return 0, fmt.Errorf("checkpoint %s unreadable: %w", cp, err)
	}
	tree, err := rt.cps.Git(nil, "rev-parse", cp+":worktree")
	if err != nil {
		return 0, err
	}
	var seq int64
	err = s.store.Tx(context.Background(), func(tx *sql.Tx) error {
		if existing, err := s.store.CheckpointByID(rt.id, cp); err == nil {
			seq = existing.Seq // idempotent retry
			return nil
		}
		lease, err := s.store.LeaseOf(tx, rt.id)
		if err != nil {
			return err
		}
		if lease.Epoch != epoch || lease.HolderKind != authorKind || lease.HolderDevice != authorDevice {
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
		now := store.Now()
		if err := s.store.InsertCheckpoint(tx, &store.Checkpoint{ID: cp, WorkspaceID: rt.id, Seq: seq, Epoch: epoch, ParentID: parent,
			AuthorKind: authorKind, AuthorDevice: authorDevice, HeadRef: meta.HeadRef, HeadOid: meta.HeadOid,
			WorktreeTree: strings.TrimSpace(string(tree)), Kind: kind, CreatedAt: now}); err != nil {
			return err
		}
		ok, err := s.store.AdvanceCurrent(tx, rt.id, parent, cp, seq, now)
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
	rt.cps.Git(nil, "update-ref", fmt.Sprintf("refs/checkpoints/%d", seq), cp)
	rt.cps.Git(nil, "update-ref", "refs/checkpoints/current", cp)
	rt.broadcast()
	return seq, nil
}
