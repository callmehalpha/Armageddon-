package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	Refs        map[string]string `json:"refs"` // branch tips as last synced with the server

	// LocalBase, when set, is a local-only checkpoint the tree is known to
	// equal (its content was just quarantined); the next apply starts
	// from it instead of AppliedOid.
	LocalBase string `json:"local_base,omitempty"`

	// Everything the protocol relies on across a crash lives here or in
	// shadow.git refs: the epoch this device believes it holds, the
	// pending checkpoint queue and the quarantine outbox (P4: a volatile
	// outbox loses work).
	Mode       string            `json:"mode"`  // follow | write
	Epoch      int64             `json:"epoch"` // > 0 while this device holds the lease
	Pending    []pendingCP       `json:"pending,omitempty"`
	Outbox     []outboxQ         `json:"outbox,omitempty"`
	PushedRefs map[string]string `json:"pushed_refs,omitempty"` // heads and tags as last pushed

	// Display only.
	Status  string    `json:"status"`
	Offline bool      `json:"offline"`
	Lease   leaseInfo `json:"lease"`
	Notices []notice  `json:"notices,omitempty"`
}

// pendingCP is a captured, not yet acknowledged checkpoint; parents chain.
type pendingCP struct {
	Oid    string `json:"oid"`
	Parent string `json:"parent"`
	Ref    string `json:"ref"`
}

// outboxQ is local state kept for quarantine, not yet stored on the server.
type outboxQ struct {
	Oid    string `json:"oid"`
	Base   string `json:"base"`
	Reason string `json:"reason"`
	Ref    string `json:"ref"`
}

type notice struct {
	At  int64  `json:"at"`
	Msg string `json:"msg"`
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

// leaseInfo is the lease part of every resync, heartbeat and rejection.
type leaseInfo struct {
	Holder        string `json:"holder"`
	HolderKind    string `json:"holder_kind"`
	HolderDevice  string `json:"holder_device"`
	HolderName    string `json:"holder_name"`
	Epoch         int64  `json:"epoch"`
	State         string `json:"state"`
	HandoffTo     string `json:"handoff_to"`
	HandoffToName string `json:"handoff_to_name"`
	HeartbeatAt   int64  `json:"heartbeat_at"`
	Now           int64  `json:"now"`
	StaleAfterMS  int64  `json:"stale_after_ms"`
	You           bool   `json:"you"`
}

// current is (holder, epoch, current checkpoint), plus a rejection's code
// and attempted epoch (P-7, P-8).
type current struct {
	Seq            int64     `json:"seq"`
	ID             string    `json:"id"`
	State          string    `json:"state"`
	Lease          leaseInfo `json:"lease"`
	Event          string    `json:"event"`
	CheckpointAt   int64     `json:"checkpoint_at"`
	Error          string    `json:"error"`
	Code           string    `json:"code"`
	AttemptedEpoch int64     `json:"attempted_epoch"`
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
	full := append([]string{"-c", "credential.helper=", "-c", "credential.helper=!" + shellQuote(self) + " git-credential"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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
	st := &replicaState{WorkspaceID: w.ID, Path: abs, Refs: map[string]string{}, Mode: "follow", Status: "FOLLOWING"}
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
	fmt.Fprintf(out, "Replica ready at checkpoint #%d, including uncommitted work.\nIt stays current while `armageddon agent run` (or `armageddon follow` in it) runs.\n", st.AppliedSeq)
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
