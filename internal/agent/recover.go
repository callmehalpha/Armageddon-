package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Recovery from this replica (contract §10 F9 b, F10): the replica holds
// the workspace's full history and its latest working state.

// historyPack packs everything reachable from the replica's branches, tags
// and HEAD, and lists those refs.
func historyPack(dir string) (refs map[string]string, headRef string, pack []byte, err error) {
	gitOut := func(stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	out, err := gitOut(nil, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, "", nil, err
	}
	refs = map[string]string{}
	var tips []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if oid, ref, ok := strings.Cut(line, " "); ok {
			refs[ref] = oid
			tips = append(tips, oid)
		}
	}
	if len(refs) == 0 {
		return nil, "", nil, fmt.Errorf("%s has no branches or tags", dir)
	}
	if b, err := gitOut(nil, "symbolic-ref", "-q", "HEAD"); err == nil {
		headRef = strings.TrimSpace(string(b))
		if _, ok := refs[headRef]; !ok {
			headRef = "" // an unborn branch
		}
	}
	if b, err := gitOut(nil, "rev-parse", "-q", "--verify", "HEAD"); err == nil {
		tips = append(tips, strings.TrimSpace(string(b)))
	}
	pack, err = gitOut([]byte(strings.Join(tips, "\n")+"\n"), "pack-objects", "--stdout", "--revs", "-q")
	return refs, headRef, pack, err
}

// SeedFromReplica creates a new workspace on the server this device is
// logged in to, from the replica in dir: its branches, tags and current
// working state, uncommitted edits and .env files included
// (`armageddon workspace seed --from-replica`).
func (c *Client) SeedFromReplica(dir, name string, out io.Writer) error {
	wsID, err := FindReplica(dir)
	if err != nil {
		return err
	}
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	if name == "" {
		name = filepath.Base(st.Path)
	}
	fmt.Fprintf(out, "Reading the replica at %s…\n", st.Path)
	refs, headRef, hist, err := historyPack(st.Path)
	if err != nil {
		return err
	}
	// A fresh capture of the working tree, with a private index so a
	// running agent's capture state is left alone.
	sh := shadowFor(wsID, st.Path)
	sh.IndexFile = filepath.Join(replicaDir(wsID), "seed.index")
	defer os.Remove(sh.IndexFile)
	state, err := sh.CaptureState()
	if err != nil {
		return fmt.Errorf("capture the working state: %w", err)
	}
	cp, err := sh.CommitState(state, "refs/armageddon/seed", "", 0)
	if err != nil {
		return err
	}
	cpPack, err := sh.PackSince(cp, "")
	if err != nil {
		return err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	meta, _ := json.Marshal(map[string]any{"name": name, "head_ref": headRef, "refs": refs, "checkpoint": cp})
	for _, part := range []struct {
		name string
		data []byte
	}{{"meta", meta}, {"history", hist}, {"checkpoint", cpPack}} {
		w, err := mw.CreateFormFile(part.name, part.name)
		if err != nil {
			return err
		}
		w.Write(part.data)
	}
	mw.Close()
	fmt.Fprintf(out, "Sending %d refs, %d KiB of history and the working state to %s…\n", len(refs), len(hist)>>10, c.Cfg.Server)
	tok, err := c.Token()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", c.url("/api/workspaces/seed"), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	var ws Workspace
	err = decode(resp, &ws)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("seed refused: %w", err)
	}
	for deadline := time.Now().Add(15 * time.Minute); ; time.Sleep(time.Second) {
		var cur struct {
			State       string `json:"state"`
			StateReason string `json:"state_reason"`
		}
		if err := c.Do("GET", "/api/workspaces/"+ws.ID, nil, &cur); err != nil {
			return err
		}
		if cur.State == "failed" {
			return fmt.Errorf("the server could not build the workspace: %s", cur.StateReason)
		}
		if cur.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("workspace %s is still %s; check the server log", ws.Slug, cur.State)
		}
	}
	// The working state is applied right after the workspace is ready.
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		var evs []struct {
			Type    string          `json:"Type"`
			Payload json.RawMessage `json:"Payload"`
		}
		if err := c.Do("GET", "/api/workspaces/"+ws.ID+"/events", nil, &evs); err != nil {
			return err
		}
		for _, e := range evs {
			switch e.Type {
			case "workspace.seeded":
				fmt.Fprintf(out, "Workspace %q is ready on %s with this replica's history and working state.\n"+
					"Clone it to keep working: armageddon clone %s\n", name, c.Cfg.Server, ws.Slug)
				return nil
			case "workspace.seed_partial":
				return fmt.Errorf("workspace %q has the history, but the working state was not applied (%s). "+
					"Your uncommitted files are still in %s; copy them into the new workspace", name, e.Payload, st.Path)
			}
		}
	}
	return fmt.Errorf("workspace %s is ready, but the server did not confirm the working state; check `armageddon status` after cloning it", ws.Slug)
}

// RepairFromDevice sends the replica's history to the server so objects
// missing from the workspace's repository come back (F10;
// `armageddon workspace repair --from-device`).
func (c *Client) RepairFromDevice(dir string, out io.Writer) error {
	wsID, err := FindReplica(dir)
	if err != nil {
		return err
	}
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	_, _, pack, err := historyPack(st.Path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Sending %d KiB of history from %s…\n", len(pack)>>10, st.Path)
	var res struct {
		Clean   bool   `json:"fsck_clean"`
		Problem string `json:"problem"`
		State   string `json:"state"`
	}
	if err := c.Do("POST", "/api/workspaces/"+wsID+"/repair", bytes.NewReader(pack), &res); err != nil {
		return err
	}
	if !res.Clean {
		return fmt.Errorf("objects added, but the repository still fails fsck: %s. Restore from a backup, or try from another replica", res.Problem)
	}
	fmt.Fprintf(out, "Repository repaired: fsck is clean, the workspace is %s.\n", strings.ToUpper(res.State))
	return nil
}
