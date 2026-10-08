package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
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
	// Run holds per-session sockets (code-server, credential helper, SSH
	// agent). Owned by and private to the workspace user.
	Run string
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
		Run:         filepath.Join(root, "run"),
	}
}

// runtime is the in-memory side of a workspace: the authority's locks, the
// capture loop and long-poll notification.
type runtime struct {
	id   string
	p    paths
	acct *helper.Account // ws-<id>; every process "in" the workspace goes through it

	authority *net.UnixListener // hooks → authority socket (P-14)
	seat      *gitshadow.Shadow // server seat capture, runs as the workspace user
	cps       *gitshadow.Shadow // checkpoints.git, server-owned (WorkTree unused)

	// fence: receive-pack holds it as a reader, lease transitions as a writer (§4.2).
	fence sync.RWMutex
	// commitMu serialises checkpoint commits for this workspace (§4.1).
	commitMu sync.Mutex
	// leaseMu serialises lease commands (acquire, release, force, timers).
	leaseMu sync.Mutex
	// seatMu serialises everything that touches tree/ and the seat shadow:
	// capture, apply as a follower, stopping dev processes.
	seatMu sync.Mutex
	// ops is the server seat's side of a handoff (tests replace it).
	ops seatOps

	leaseStateMu    sync.Mutex
	handoffFailures map[int64]*LeaseError // why the handoff at an epoch ended without a transfer
	last            transition            // the latest change of holder, for long-poll events

	notifyMu sync.Mutex
	notify   chan struct{} // closed and replaced on every committed checkpoint and lease change

	ctx    context.Context
	cancel context.CancelFunc
}

// done is closed when the runtime stops.
func (rt *runtime) done() <-chan struct{} {
	if rt.ctx == nil {
		return nil
	}
	return rt.ctx.Done()
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
	if rt.authority != nil {
		rt.authority.Close()
	}
}

func (s *Server) runtimeFor(id string) *runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rts[id]
}

func (s *Server) newRuntime(w *store.Workspace) (*runtime, error) {
	p := s.pathsFor(w.ID)
	acct, err := s.helper.CreateWorkspaceUser(context.Background(), w.ID)
	if err != nil {
		return nil, err
	}
	if s.helper.Isolated() && acct.Name != w.OSUser {
		return nil, fmt.Errorf("workspace user is %s, expected %s", acct.Name, w.OSUser)
	}
	rt := &runtime{id: w.ID, p: p, acct: acct, notify: make(chan struct{})}
	rt.seat = &gitshadow.Shadow{GitDir: p.SeatShadow, WorkTree: p.Tree, IndexFile: p.SeatIndex,
		Preserve: []string{".env*"}, Prepare: func(c *exec.Cmd) { acct.Prepare(c) }}
	rt.cps = &gitshadow.Shadow{GitDir: p.Checkpoints, WorkTree: p.Root, IndexFile: filepath.Join(p.Root, "cps.index")}
	rt.ops = &serverSeat{s: s, rt: rt}
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
		if !running(w) {
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
		// Rewrite hooks (they carry the authority socket path) and open
		// the socket.
		if err := s.writeHooks(rt); err != nil {
			log.Printf("workspace %s: hooks: %v", w.ID, err)
		}
		if err := s.listenAuthority(rt); err != nil {
			log.Printf("workspace %s: authority socket: %v", w.ID, err)
		}
		s.mu.Lock()
		s.rts[w.ID] = rt
		s.mu.Unlock()
		s.startCaptureLoop(ctx, rt)
		s.resumeHandoff(rt)
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
	return s.createWorkspace(owner, name, sourceURL, nil)
}

// createWorkspace is CreateWorkspace, optionally seeded from a device
// replica (F9 b) instead of empty or cloned.
func (s *Server) createWorkspace(owner *store.User, name, sourceURL string, seed *seedData) (*store.Workspace, error) {
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
		SourceKind: "empty", SourceURL: sourceURL, OSUser: helper.UserName(id), CreatedAt: now, UpdatedAt: now}
	if sourceURL != "" {
		w.SourceKind = "clone"
	}
	if err := s.store.Tx(context.Background(), func(tx *sql.Tx) error { return s.store.CreateWorkspace(tx, w) }); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("you already have a workspace with that name")
		}
		return nil, err
	}
	if seed != nil {
		s.event(id, "device", seed.device, "workspace.created", map[string]string{"name": name, "source": "replica"})
	} else {
		s.event(id, "user", owner.ID, "workspace.created", map[string]string{"name": name, "source": sourceURL})
	}
	go func() {
		if err := s.importWorkspace(w, owner, seed); err != nil {
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
func (s *Server) importWorkspace(w *store.Workspace, owner *store.User, seed *seedData) error {
	if err := s.store.SetWorkspaceState(w.ID, StateCreating, StateImporting, "", store.Now()); err != nil {
		return err
	}
	rt, err := s.newRuntime(w)
	if err != nil {
		return err
	}
	p, a := rt.p, rt.acct
	// The workspace directory and its server-side entries (hooks,
	// checkpoints.git) belong to the server; repo.git, tree, home and seat
	// are created by the helper, owned by the workspace user.
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	// The server's umask is 077; these two must stay traversable.
	for _, d := range []string{filepath.Dir(p.Root), p.Root} {
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	if err := s.helper.PrepareWorkspaceDirs(context.Background(), w.ID); err != nil {
		return fmt.Errorf("prepare workspace directories: %w", err)
	}
	// Workspace user's Git identity and a guard against inherited config.
	if err := s.prepareHome(rt, owner, w.Slug); err != nil {
		return err
	}

	run := func(dir string, args ...string) error {
		cmd := a.Command(dir, "git", args...)
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if seed != nil {
		if err := s.importSeedRepo(rt, seed, run); err != nil {
			return err
		}
	} else if w.SourceKind == "clone" {
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
		// have a commit to work against. The empty tree is written as a real
		// object first: Git only pretends it exists, and `git fsck` reports
		// it missing otherwise (found by doctor).
		if err := run(p.Repo, "hash-object", "-w", "-t", "tree", "/dev/null"); err != nil {
			return err
		}
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
	// p.Tree exists (PrepareWorkspaceDirs), empty and owned by the
	// workspace user, who cannot create directories in the server-owned
	// workspace directory itself.
	if err := run(p.Repo, "worktree", "add", "--quiet", p.Tree, strings.TrimSpace(string(head))); err != nil {
		return err
	}
	if err := s.writeHooks(rt); err != nil {
		return err
	}
	// The seat shadow belongs to the workspace user, including the info/
	// files carrying the byte-exact capture settings.
	if err := gitshadow.InitWith(p.SeatShadow, rt.seat.Prepare, a.WriteFile); err != nil {
		return fmt.Errorf("seat shadow: %w", err)
	}
	// checkpoints.git belongs to the server and is never opened by
	// workspace processes; objects reach it only as a pack stream (§2.5).
	if err := gitshadow.Init(p.Checkpoints, nil); err != nil {
		return fmt.Errorf("checkpoints repo: %w", err)
	}
	os.Chmod(p.Checkpoints, 0o700)
	if err := s.listenAuthority(rt); err != nil {
		return fmt.Errorf("authority socket: %w", err)
	}
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
	if seed != nil {
		if err := s.applySeedCheckpoint(rt, seed); err != nil {
			// The history is in; only the uncommitted working state is
			// missing. The device still has it: nothing is lost.
			log.Printf("workspace %s: seed: working state not applied: %v", w.ID, err)
			s.event(w.ID, "server", "", "workspace.seed_partial", map[string]string{"error": err.Error()})
		} else {
			s.event(w.ID, "device", seed.device, "workspace.seeded", map[string]any{"refs": len(seed.refs), "checkpoint": seed.checkpoint})
		}
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
	if err := os.Chmod(rt.p.Hooks, 0o755); err != nil {
		return err
	}
	for _, h := range []string{"post-receive", "reference-transaction"} {
		script := fmt.Sprintf("#!/bin/sh\nARMAGEDDON_AUTHORITY_SOCK=%q exec %q hook %s \"$@\"\n", s.authoritySocket(rt.id), s.hookBin, h)
		f := filepath.Join(rt.p.Hooks, h)
		if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
			return err
		}
		if err := os.Chmod(f, 0o755); err != nil {
			return err
		}
	}
	return nil
}
