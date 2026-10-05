package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// Workspace is the server's view of a workspace.
type Workspace struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	State         string `json:"state"`
	GitURL        string `json:"git_url"`
	CheckpointSeq int64  `json:"checkpoint_seq"`
}

// replicaState is persisted next to the shadow (durable agent state).
type replicaState struct {
	WorkspaceID string            `json:"workspace_id"`
	Path        string            `json:"path"`
	AppliedOid  string            `json:"applied_oid"`
	AppliedSeq  int64             `json:"applied_seq"`
	Refs        map[string]string `json:"refs"` // branch tips as last received from the server
}

func replicaDir(wsID string) string { return filepath.Join(dataDir(), "replicas", wsID) }

func loadState(wsID string) (*replicaState, error) {
	b, err := os.ReadFile(filepath.Join(replicaDir(wsID), "state.json"))
	if err != nil {
		return nil, err
	}
	var st replicaState
	return &st, json.Unmarshal(b, &st)
}

func (st *replicaState) save() error {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(replicaDir(st.WorkspaceID), "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(replicaDir(st.WorkspaceID), "state.json"))
}

func shadowFor(wsID, worktree string) *gitshadow.Shadow {
	d := replicaDir(wsID)
	return &gitshadow.Shadow{GitDir: filepath.Join(d, "shadow.git"), WorkTree: worktree,
		IndexFile: filepath.Join(d, "capture.index"), Preserve: []string{".env*"}}
}

// Resolve finds a workspace by ID, slug or name.
func (c *Client) Resolve(ref string) (*Workspace, error) {
	var ws []Workspace
	if err := c.Do("GET", "/api/workspaces", nil, &ws); err != nil {
		return nil, err
	}
	for _, w := range ws {
		if w.ID == ref || w.Slug == ref || strings.EqualFold(w.Name, ref) {
			w := w
			return &w, nil
		}
	}
	return nil, fmt.Errorf("no workspace %q (see `armageddon workspaces`)", ref)
}

func (c *Client) Workspaces() ([]Workspace, error) {
	var ws []Workspace
	return ws, c.Do("GET", "/api/workspaces", nil, &ws)
}

type current struct {
	Seq   int64  `json:"seq"`
	ID    string `json:"id"`
	State string `json:"state"`
}

func (c *Client) current(wsID string, after int64, wait int) (*current, error) {
	var cur current
	err := c.Do("GET", fmt.Sprintf("/api/workspaces/%s/current?after=%d&wait=%d", wsID, after, wait), nil, &cur)
	return &cur, err
}

// fetchCheckpoint downloads checkpoint seq as a thin pack against base (P-1)
// and stores it in the replica shadow.
func (c *Client) fetchCheckpoint(sh *gitshadow.Shadow, wsID string, seq int64, base string) (string, error) {
	q := ""
	if base != "" {
		q = "?base=" + url.QueryEscape(base)
	}
	resp, err := c.Raw("GET", fmt.Sprintf("/api/workspaces/%s/checkpoints/%d/pack%s", wsID, seq, q), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", decode(resp, nil)
	}
	pack, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	cp := resp.Header.Get("X-Armageddon-Checkpoint")
	return cp, sh.ReceivePack(pack, "refs/armageddon/current", cp)
}

// git runs git in dir with the Armageddon credential helper configured.
func (c *Client) git(dir string, args ...string) (string, error) {
	self, _ := os.Executable()
	full := []string{"-c", "credential.helper=", "-c", "credential.helper=!" + shellQuote(self) + " git-credential"}
	if c.Cfg.CertFingerprint != "" {
		// The pinned self-signed certificate is Git's only trust anchor.
		full = append(full, "-c", "http.sslCAInfo="+pinnedCertPath())
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if c.Cfg.CertFingerprint != "" {
		// GIT_SSL_CAINFO in the environment would override the -c above.
		cmd.Env = append(cmd.Env, "GIT_SSL_CAINFO="+pinnedCertPath())
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Clone creates a follower replica: git clone, then seed the working state
// from the current checkpoint (P-2: a clone is never assumed equal to it).
func (c *Client) Clone(ref, dir string, out io.Writer) error {
	w, err := c.Resolve(ref)
	if err != nil {
		return err
	}
	if w.State != "ready" {
		return fmt.Errorf("workspace %s is %s", w.Name, w.State)
	}
	if dir == "" {
		dir = w.Slug
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("%s already exists", abs)
	}
	if st, err := loadState(w.ID); err == nil {
		if _, err := os.Stat(st.Path); err == nil {
			return fmt.Errorf("this device already has a replica of %s at %s", w.Name, st.Path)
		}
		// Stale state from a deleted replica; refuse while a follower of the
		// old replica is still running.
		lk, err := lockReplica(w.ID)
		if err != nil {
			return err
		}
		lk.release()
		os.RemoveAll(replicaDir(w.ID))
	}
	fmt.Fprintf(out, "Cloning %s into %s…\n", w.Name, abs)
	if _, err := c.git(filepath.Dir(abs), "clone", "-q", "--origin", "armageddon", w.GitURL, abs); err != nil {
		return err
	}
	// The replica uses the helper for every later fetch.
	self, _ := os.Executable()
	if _, err := c.git(abs, "config", "credential."+strings.TrimSuffix(w.GitURL, "/")+".helper", "!"+shellQuote(self)+" git-credential"); err != nil {
		return err
	}
	if c.Cfg.CertFingerprint != "" {
		// Plain `git fetch` in the replica trusts the pinned certificate too.
		if _, err := c.git(abs, "config", "http."+strings.TrimSuffix(w.GitURL, "/")+".sslCAInfo", pinnedCertPath()); err != nil {
			return err
		}
	}
	lk, err := lockReplica(w.ID)
	if err != nil {
		return err
	}
	defer lk.release()
	sh := shadowFor(w.ID, abs)
	if err := gitshadow.Init(sh.GitDir, nil); err != nil {
		return err
	}
	cur, err := c.current(w.ID, 0, 0)
	if err != nil {
		return err
	}
	st := &replicaState{WorkspaceID: w.ID, Path: abs, Refs: map[string]string{}}
	if cur.Seq > 0 {
		cp, err := c.fetchCheckpoint(sh, w.ID, cur.Seq, "")
		if err != nil {
			return err
		}
		if _, err := c.git(abs, "fetch", "-q", "--update-head-ok", "armageddon", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
			return err
		}
		if err := c.pointHead(sh, cp); err != nil {
			return err
		}
		if err := sh.Seed(cp); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		st.AppliedOid, st.AppliedSeq = cp, cur.Seq
	}
	st.Refs, _ = localBranches(abs)
	if err := st.save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Replica ready at checkpoint #%d, including uncommitted work.\nRun `armageddon follow` inside it to keep it current.\n", st.AppliedSeq)
	return nil
}

// pointHead makes the replica's HEAD match the checkpoint's HEAD (branch or
// detached), fetching branches first so the commit exists.
func (c *Client) pointHead(sh *gitshadow.Shadow, cp string) error {
	m, err := sh.ReadMeta(cp)
	if err != nil {
		return err
	}
	if m.HeadRef != "" {
		_, err = c.git(sh.WorkTree, "symbolic-ref", "HEAD", m.HeadRef)
	} else if m.HeadOid != "" {
		_, err = c.git(sh.WorkTree, "update-ref", "--no-deref", "HEAD", m.HeadOid)
	}
	return err
}

func localBranches(dir string) (map[string]string, error) {
	out, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/").Output()
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

// FindReplica returns the workspace ID of the replica containing dir.
func FindReplica(dir string) (string, error) {
	abs, _ := filepath.Abs(dir)
	ents, _ := os.ReadDir(filepath.Join(dataDir(), "replicas"))
	for _, e := range ents {
		st, err := loadState(e.Name())
		if err == nil && (abs == st.Path || strings.HasPrefix(abs, st.Path+string(filepath.Separator))) {
			return st.WorkspaceID, nil
		}
	}
	return "", errors.New("not inside an Armageddon replica (use `armageddon clone` first)")
}

// Follow keeps the replica current until ctx ends (§3.3 FOLLOWING).
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
	sh := shadowFor(wsID, st.Path)
	fmt.Fprintf(out, "Following %s at checkpoint #%d (Ctrl-C to stop)\n", st.Path, st.AppliedSeq)
	backoff := time.Second
	for ctx.Err() == nil {
		cur, err := c.current(wsID, st.AppliedSeq, 50)
		if err != nil {
			fmt.Fprintf(out, "%s server unreachable (%v); retrying in %s\n", ts(), err, backoff)
			sleep(ctx, backoff)
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if cur.Seq <= st.AppliedSeq {
			continue
		}
		if err := c.applyOne(sh, st, cur.Seq, out); err != nil {
			fmt.Fprintf(out, "%s apply failed: %v\n", ts(), err)
			sleep(ctx, 5*time.Second)
		}
	}
	return nil
}

func ts() string { return time.Now().Format("15:04:05") }

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// applyOne moves the replica to checkpoint seq. Local state that never
// reached the server is quarantined before it is overwritten (I4).
func (c *Client) applyOne(sh *gitshadow.Shadow, st *replicaState, seq int64, out io.Writer) error {
	cp, err := c.fetchCheckpoint(sh, st.WorkspaceID, seq, st.AppliedOid)
	if err != nil {
		return err
	}
	// History first. Local commits on a read-only replica are kept under
	// refs/armageddon/quarantine/ before branches are overwritten (§5.5).
	if local, err := localBranches(st.Path); err == nil {
		for ref, oid := range local {
			if prev, ok := st.Refs[ref]; ok && prev != oid {
				q := fmt.Sprintf("refs/armageddon/quarantine/%d/%s", time.Now().Unix(), strings.TrimPrefix(ref, "refs/"))
				c.git(st.Path, "update-ref", q, oid)
				fmt.Fprintf(out, "%s local commits on %s kept as %s\n", ts(), ref, q)
			}
		}
	}
	if _, err := c.git(st.Path, "fetch", "-q", "--prune", "--update-head-ok", "armageddon", "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	if err := c.pointHead(sh, cp); err != nil {
		return err
	}
	err = sh.Apply(st.AppliedOid, cp, gitshadow.ApplyOptions{})
	if errors.Is(err, gitshadow.ErrDiverged) || (err != nil && strings.Contains(err.Error(), "diverged")) {
		qid, qerr := c.quarantine(sh, st, "follower dirty")
		if qerr != nil {
			return fmt.Errorf("local edits found but quarantine upload failed (%v); not overwriting them", qerr)
		}
		fmt.Fprintf(out, "%s local edits on this read-only replica were saved to the server as quarantine %s\n", ts(), qid)
		err = sh.Seed(cp)
	}
	if err != nil {
		return err
	}
	st.AppliedOid, st.AppliedSeq = cp, seq
	st.Refs, _ = localBranches(st.Path)
	if err := st.save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s applied checkpoint #%d\n", ts(), seq)
	return nil
}

// quarantine captures the replica's local state and uploads it (I4).
func (c *Client) quarantine(sh *gitshadow.Shadow, st *replicaState, reason string) (string, error) {
	state, err := sh.CaptureState()
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("refs/armageddon/quarantine/%d", time.Now().UnixNano())
	cp, err := sh.CommitState(state, ref, st.AppliedOid, 0)
	if err != nil {
		return "", err
	}
	pack, err := sh.PackSince(cp, st.AppliedOid)
	if err != nil {
		return "", err
	}
	q := url.Values{"checkpoint": {cp}, "base": {st.AppliedOid}, "reason": {reason}}
	var res struct {
		ID string `json:"id"`
	}
	if err := c.Do("POST", "/api/workspaces/"+st.WorkspaceID+"/quarantines?"+q.Encode(), bytes.NewReader(pack), &res); err != nil {
		return "", err
	}
	return res.ID, nil
}

// Status reports replica health.
func (c *Client) Status(wsID string, out io.Writer) error {
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	sh := shadowFor(wsID, st.Path)
	fmt.Fprintf(out, "replica:     %s\nworkspace:   %s\napplied:     checkpoint #%d (%.12s)\n", st.Path, wsID, st.AppliedSeq, st.AppliedOid)
	if cur, err := c.current(wsID, 0, 0); err == nil {
		lag := "up to date"
		if cur.Seq > st.AppliedSeq {
			lag = fmt.Sprintf("%d checkpoint(s) behind — run `armageddon follow`", cur.Seq-st.AppliedSeq)
		}
		fmt.Fprintf(out, "server:      checkpoint #%d (%s)\n", cur.Seq, lag)
	} else {
		fmt.Fprintf(out, "server:      unreachable (%v)\n", err)
	}
	lk, err := lockReplica(wsID)
	if errors.Is(err, errBusy) {
		fmt.Fprintln(out, "local:       a follower is running (dirty check skipped)")
		return nil
	} else if err != nil {
		return err
	}
	defer lk.release()
	if st.AppliedOid != "" {
		state, err := sh.CaptureState()
		prev, err2 := sh.StateOf(st.AppliedOid)
		if err == nil && err2 == nil && !state.Same(prev) {
			fmt.Fprintln(out, "local:       DIRTY — this replica is read-only; local edits will be quarantined at the next checkpoint")
		} else if err == nil {
			fmt.Fprintln(out, "local:       clean")
		}
	}
	return nil
}
