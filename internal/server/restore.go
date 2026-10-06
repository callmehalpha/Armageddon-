package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/callmehalpha/Armageddon-/internal/store"
	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// WorkspaceDir is the directory of one workspace under a data directory
// (contract §2.4).
func WorkspaceDir(dataDir, id string) string { return filepath.Join(dataDir, "workspaces", id) }

// Close releases the database. Used by offline tools (restore) that create
// a Server without running it.
func (s *Server) Close() error { return s.store.Close() }

// CheckpointNow captures the server seat of a loaded workspace, like
// POST /api/workspaces/{id}/sync. Returns 0 when nothing changed.
func (s *Server) CheckpointNow(id string) (int64, error) {
	rt := s.runtimeFor(id)
	if rt == nil {
		return 0, fmt.Errorf("workspace %s is not running", id)
	}
	return s.captureOnce(rt)
}

// RestoreResult reports what RestoreWorkspace rebuilt.
type RestoreResult struct {
	// Exact is true when a fresh capture of the rebuilt server seat equals
	// the current checkpoint, so the server will not create a new one.
	Exact bool
}

// RestoreWorkspace rebuilds a workspace's on-disk state on a fresh data
// directory from the bundles written by `server backup`: repo.git from
// repoBundle (all refs, including trash refs), checkpoints.git from
// cpBundle, then the server seat worktree at the current checkpoint
// recorded in the database (the database is truth, §8.3). Checkpoint refs
// newer than the database's sequence are dropped.
//
// Git commands on repo.git and tree/ run as the workspace user (§2.5).
func (s *Server) RestoreWorkspace(w *store.Workspace, repoBundle, cpBundle, headRef string) (*RestoreResult, error) {
	owner, err := s.store.UserByID(w.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("owner: %w", err)
	}
	rt, err := s.newRuntime(w)
	if err != nil {
		return nil, err
	}
	p, a := rt.p, rt.acct
	if _, err := os.Stat(p.Root); err == nil {
		return nil, fmt.Errorf("%s already exists: restore needs a fresh data directory", p.Root)
	}
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return nil, err
	}
	for _, d := range []string{filepath.Dir(p.Root), p.Root} {
		if err := os.Chmod(d, 0o755); err != nil {
			return nil, err
		}
	}
	// repo.git, tree, home and seat, owned by the workspace user (§2.5).
	if err := s.helper.PrepareWorkspaceDirs(context.Background(), w.ID); err != nil {
		return nil, fmt.Errorf("prepare workspace directories: %w", err)
	}
	if err := s.prepareHome(rt, owner, w.Slug); err != nil {
		return nil, err
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

	// repo.git, as the workspace user. The bundle is copied next to it
	// (the server-owned workspace directory, readable by the workspace
	// user), fetched, then removed.
	if err := run(p.Repo, "init", "--bare", "--quiet"); err != nil {
		return nil, err
	}
	if repoBundle != "" {
		tmp := filepath.Join(p.Root, "restore.bundle")
		if err := copyFile(repoBundle, tmp, 0o644); err != nil {
			return nil, err
		}
		// The server's umask is 077: make the copy readable explicitly.
		if err := os.Chmod(tmp, 0o644); err != nil {
			return nil, err
		}
		err := run(p.Repo, "fetch", "--quiet", "--no-write-fetch-head", tmp, "refs/*:refs/*")
		os.Remove(tmp)
		if err != nil {
			return nil, err
		}
	}
	if headRef != "" {
		if err := run(p.Repo, "symbolic-ref", "HEAD", headRef); err != nil {
			return nil, err
		}
	}
	for _, kv := range [][2]string{{"core.logAllRefUpdates", "always"}, {"gc.auto", "0"}, {"core.hooksPath", p.Hooks}} {
		if err := run(p.Repo, "config", kv[0], kv[1]); err != nil {
			return nil, err
		}
	}

	// checkpoints.git, as the server.
	if err := gitshadow.Init(p.Checkpoints, nil); err != nil {
		return nil, fmt.Errorf("checkpoints repo: %w", err)
	}
	os.Chmod(p.Checkpoints, 0o700)
	if cpBundle != "" {
		if _, err := rt.cps.Git(nil, "fetch", "--quiet", "--no-write-fetch-head", cpBundle, "refs/*:refs/*"); err != nil {
			return nil, err
		}
	}
	if err := reconcileCheckpointRefs(rt, w); err != nil {
		return nil, err
	}

	// The server seat worktree, on the checkpoint's branch (or detached).
	meta, err := rt.cps.ReadMeta(w.CurrentCheckpoint)
	if err != nil {
		return nil, fmt.Errorf("current checkpoint %s: %w", w.CurrentCheckpoint, err)
	}
	switch {
	case strings.HasPrefix(meta.HeadRef, "refs/heads/"):
		err = run(p.Repo, "worktree", "add", "--quiet", p.Tree, strings.TrimPrefix(meta.HeadRef, "refs/heads/"))
	case meta.HeadOid != "":
		err = run(p.Repo, "worktree", "add", "--quiet", "--detach", p.Tree, meta.HeadOid)
	default:
		err = run(p.Repo, "worktree", "add", "--quiet", p.Tree)
	}
	if err != nil {
		return nil, err
	}
	if err := s.writeHooks(rt); err != nil {
		return nil, err
	}
	if err := gitshadow.InitWith(p.SeatShadow, rt.seat.Prepare, a.WriteFile); err != nil {
		return nil, fmt.Errorf("seat shadow: %w", err)
	}
	res := &RestoreResult{}
	if w.CurrentCheckpoint != "" {
		// Uncommitted work: bring the worktree and index to the current
		// checkpoint exactly as a new replica would (P-2).
		if _, err := gitshadow.TransferPack(rt.cps, rt.seat, "refs/seat/latest", w.CurrentCheckpoint, ""); err != nil {
			return nil, fmt.Errorf("seat shadow: %w", err)
		}
		// Seed as the workspace user: it writes files in tree/ (§2.5).
		if err := s.seatApply(rt, "", w.CurrentCheckpoint); err != nil {
			return nil, fmt.Errorf("restore worktree: %w", err)
		}
		st, err := rt.seat.CaptureState()
		if err != nil {
			return nil, err
		}
		prev, err := rt.cps.StateOf(w.CurrentCheckpoint)
		res.Exact = err == nil && prev.Same(st)
	}
	return res, nil
}

// reconcileCheckpointRefs makes checkpoints.git agree with the database:
// refs/checkpoints/current is the DB's current checkpoint, numbered refs
// beyond the DB's sequence and leftover staging refs are removed.
func reconcileCheckpointRefs(rt *runtime, w *store.Workspace) error {
	out, err := rt.cps.Git(nil, "for-each-ref", "--format=%(refname)", "refs/checkpoints/", "refs/staging/")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(string(out)) {
		drop := strings.HasPrefix(ref, "refs/staging/")
		if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "refs/checkpoints/"), 10, 64); err == nil && n > w.CheckpointSeq {
			drop = true
		}
		if drop {
			if _, err := rt.cps.Git(nil, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
	}
	if w.CurrentCheckpoint == "" {
		return nil
	}
	if _, err := rt.cps.Git(nil, "cat-file", "-e", w.CurrentCheckpoint+"^{commit}"); err != nil {
		return fmt.Errorf("current checkpoint %s is missing from the checkpoints bundle", w.CurrentCheckpoint)
	}
	_, err = rt.cps.Git(nil, "update-ref", "refs/checkpoints/current", w.CurrentCheckpoint)
	return err
}

// prepareHome writes the workspace user's Git identity and shell prompt,
// as the workspace user.
func (s *Server) prepareHome(rt *runtime, owner *store.User, slug string) error {
	p, a := rt.p, rt.acct
	// Written as the workspace user: the server cannot write into home/,
	// which PrepareWorkspaceDirs created (§2.5).
	gitcfg := fmt.Sprintf("[user]\n\tname = %s\n\temail = %s@armageddon.local\n[init]\n\tdefaultBranch = main\n", owner.Username, owner.Username)
	if err := a.WriteFile(filepath.Join(p.Home, ".gitconfig"), []byte(gitcfg), 0o600); err != nil {
		return err
	}
	// Login shells read .bash_profile; keep the prompt short and relative
	// to the workspace rather than the server's directory layout.
	bashrc := fmt.Sprintf("export PS1='\\[\\e[1;36m\\]%s\\[\\e[0m\\]:\\W\\$ '\n", slug)
	if err := a.WriteFile(filepath.Join(p.Home, ".bashrc"), []byte(bashrc), 0o600); err != nil {
		return err
	}
	return a.WriteFile(filepath.Join(p.Home, ".bash_profile"), []byte("[ -f ~/.bashrc ] && . ~/.bashrc\n"), 0o600)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
