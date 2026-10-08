package agent

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// The quarantine CLI (contract §6.7, plan M7.7). Quarantines are kept until
// explicitly dropped (decision Q6); nothing here evicts one automatically.

type quarantineRow struct {
	ID               string
	SourceKind       string
	SourceDevice     string
	CheckpointID     string
	BaseCheckpointID string
	Reason           string
	CreatedAt        int64
}

func (c *Client) quarantines(wsID string) ([]quarantineRow, error) {
	var qs []quarantineRow
	return qs, c.Do("GET", "/api/workspaces/"+wsID+"/quarantines", nil, &qs)
}

// QuarantineList prints the workspace's uncleared quarantines.
func (c *Client) QuarantineList(wsID string, out io.Writer) error {
	qs, err := c.quarantines(wsID)
	if err != nil {
		return err
	}
	if len(qs) == 0 {
		fmt.Fprintln(out, "No quarantines.")
		return nil
	}
	devs := map[string]string{}
	var ds []struct{ ID, Name string }
	if c.Do("GET", "/api/devices", nil, &ds) == nil {
		for _, d := range ds {
			devs[d.ID] = d.Name
		}
	}
	fmt.Fprintf(out, "%-26s  %-19s  %-16s  %s\n", "ID", "CREATED", "SOURCE", "REASON")
	for _, q := range qs {
		src := "server"
		if q.SourceKind != "server" {
			src = devs[q.SourceDevice]
			if src == "" {
				src = q.SourceDevice
			}
		}
		fmt.Fprintf(out, "%-26s  %-19s  %-16s  %s\n", q.ID, time.UnixMilli(q.CreatedAt).Format("2006-01-02 15:04:05"), src, q.Reason)
	}
	return nil
}

// fetchQuarantine stores a quarantine and its base in the replica shadow.
func (c *Client) fetchQuarantine(sh *gitshadow.Shadow, wsID, qid string) (cp, base string, err error) {
	resp, err := c.Raw("GET", "/api/workspaces/"+wsID+"/quarantines/"+qid+"/pack", nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", decode(resp, nil)
	}
	pack, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	cp, base = resp.Header.Get("X-Armageddon-Checkpoint"), resp.Header.Get("X-Armageddon-Base")
	if err := sh.ReceivePack(pack, "refs/armageddon/q/"+qid, cp); err != nil {
		return "", "", err
	}
	if base != "" {
		sh.Git(nil, "update-ref", "refs/armageddon/q/"+qid+"-base", base)
	}
	return cp, base, nil
}

// QuarantineDiff shows a quarantine against the workspace's current state.
func (c *Client) QuarantineDiff(wsID, qid string, out io.Writer) error {
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	sh := shadowFor(wsID, st.Path)
	q, _, err := c.fetchQuarantine(sh, wsID, qid)
	if err != nil {
		return err
	}
	against := gitshadow.EmptyTree
	if cur, err := c.current(wsID, 0, 0); err == nil && cur.Seq > 0 {
		cp, err := c.fetchCheckpoint(sh, wsID, cur.Seq, st.AppliedOid)
		if err != nil {
			return err
		}
		against = cp + ":worktree"
		fmt.Fprintf(out, "quarantine %s against current (checkpoint #%d):\n", qid, cur.Seq)
	}
	d, err := sh.Git(nil, "diff", "--text", "--no-color", "--stat", "--patch", against, q+":worktree")
	if err != nil {
		return err
	}
	if len(d) == 0 {
		fmt.Fprintln(out, "(no differences: the quarantined files equal current)")
	}
	out.Write(d)
	return nil
}

// QuarantineExport writes the quarantined working tree into dir.
func (c *Client) QuarantineExport(wsID, qid, dir string, out io.Writer) error {
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}
	sh := shadowFor(wsID, st.Path)
	q, _, err := c.fetchQuarantine(sh, wsID, qid)
	if err != nil {
		return err
	}
	t, err := sh.Git(nil, "archive", "--format=tar", q+":worktree")
	if err != nil {
		return err
	}
	n, err := untar(bytes.NewReader(t), dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Exported %d file(s) of quarantine %s to %s\n", n, qid, dir)
	return nil
}

func untar(r io.Reader, dir string) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		// The archive comes from a quarantine another seat uploaded: no
		// path into a .git directory or through a symlink it just created.
		if err := gitshadow.CheckPath(dir, strings.TrimSuffix(h.Name, "/")); err != nil {
			return n, fmt.Errorf("bad path in archive: %w", err)
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0o755)
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return n, err
			}
			n++
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return n, err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return n, err
			}
			n++
		}
	}
}

// QuarantineDrop clears a quarantine on the server.
func (c *Client) QuarantineDrop(wsID, qid string, out io.Writer) error {
	if err := c.Do("DELETE", "/api/workspaces/"+wsID+"/quarantines/"+qid, nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "Quarantine %s dropped.\n", qid)
	return nil
}

// QuarantineApply merges a quarantine into this replica's working tree,
// 3-way: base = the quarantine's parent checkpoint, ours = the working
// tree, theirs = the quarantine. Overlapping text changes get conflict
// markers; nothing is merged silently where both sides changed. The result
// is a normal edit by the current writer, so this device must hold the
// lease (`armageddon work local`).
func (c *Client) QuarantineApply(wsID, qid string, out io.Writer) error {
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	if st.Epoch == 0 {
		return errors.New("this replica is read-only: run `armageddon work local` first, so the merged result is checkpointed")
	}
	sh := shadowFor(wsID, st.Path)
	q, base, err := c.fetchQuarantine(sh, wsID, qid)
	if err != nil {
		return err
	}
	baseTree := gitshadow.EmptyTree
	if base != "" {
		baseTree = base + ":worktree"
	}
	res, err := merge3(sh, st.Path, baseTree, q+":worktree", qid)
	if err != nil {
		return err
	}
	taken, merged, conflicts, kept := res.Taken, res.Merged, res.Conflicts, res.Kept
	for _, x := range [][2]any{{"taken from the quarantine", taken}, {"merged cleanly", merged}, {"CONFLICTS (resolve the <<<<<<< markers)", conflicts}, {"kept local", kept}} {
		if l := x[1].([]string); len(l) > 0 {
			fmt.Fprintf(out, "%s:\n", x[0])
			for _, p := range l {
				fmt.Fprintf(out, "  %s\n", p)
			}
		}
	}
	if len(taken)+len(merged)+len(conflicts)+len(kept) == 0 {
		fmt.Fprintln(out, "Nothing to apply: the working tree already has the quarantine's changes.")
	}
	fmt.Fprintf(out, "The quarantine is kept; clear it with `armageddon quarantine drop %s` when you are done.\n", qid)
	return nil
}

func writeFile(p, mode string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	os.Remove(p)
	if mode == "120000" {
		return os.Symlink(string(content), p)
	}
	perm := os.FileMode(0o644)
	if mode == "100755" {
		perm = 0o755
	}
	if err := os.WriteFile(p, content, perm); err != nil {
		return err
	}
	return os.Chmod(p, perm)
}

func splitNul(b []byte) []string {
	s := string(b)
	s = strings.TrimSuffix(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// mergeResult lists what a 3-way merge did, by path.
type mergeResult struct{ Taken, Merged, Conflicts, Kept []string }

// merge3 merges the change base→theirs (two trees in the shadow) into the
// working tree dir, where "ours" is what is on disk. Overlapping text
// changes get conflict markers; binaries and symlinks in conflict get the
// other version beside them; nothing both sides changed is merged silently.
func merge3(sh *gitshadow.Shadow, dir, baseTree, theirsTree, label string) (*mergeResult, error) {
	raw, err := sh.Git(nil, "diff-tree", "-r", "-z", "--no-renames", baseTree, theirsTree)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "armageddon-merge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	blob := func(oid string) []byte {
		if oid == "" || strings.Trim(oid, "0") == "" {
			return nil
		}
		b, _ := sh.Git(nil, "cat-file", "blob", oid)
		return b
	}
	res := &mergeResult{}
	f := splitNul(raw)
	for i := 0; i+1 < len(f); i += 2 {
		m := strings.Fields(strings.TrimPrefix(f[i], ":"))
		bMode, tMode, bOid, tOid, path := m[0], m[1], m[2], m[3], f[i+1]
		if bMode == "000000" {
			bOid = ""
		}
		if tMode == "000000" {
			tOid = ""
		}
		if err := gitshadow.CheckPath(dir, path); err != nil {
			return nil, err
		}
		disk := filepath.Join(dir, filepath.FromSlash(path))
		oMode, oOid, err := gitshadow.DiskBlob(disk)
		if err != nil {
			return nil, err
		}
		if oMode == "000000" {
			oOid = ""
		}
		switch {
		case oOid == tOid && (tOid == "" || oMode == tMode):
			// already as in the quarantine
		case oOid == bOid && (bOid == "" || oMode == bMode):
			// unchanged here: take the quarantine's version
			if tOid == "" {
				os.Remove(disk)
			} else if err := writeFile(disk, tMode, blob(tOid)); err != nil {
				return nil, err
			}
			res.Taken = append(res.Taken, path)
		case tOid == "":
			res.Kept = append(res.Kept, path+" (deleted in the quarantine, changed here: kept)")
		case oMode == "120000" || tMode == "120000" || oMode == "040000":
			alt := disk + ".quarantine-" + label
			if err := writeFile(alt, tMode, blob(tOid)); err != nil {
				return nil, err
			}
			res.Conflicts = append(res.Conflicts, path+" (quarantine version written to "+filepath.Base(alt)+")")
		default:
			ours, _ := os.ReadFile(disk)
			files := []string{filepath.Join(tmp, "ours"), filepath.Join(tmp, "base"), filepath.Join(tmp, "theirs")}
			for i, b := range [][]byte{ours, blob(bOid), blob(tOid)} {
				os.WriteFile(files[i], b, 0o600)
			}
			cmd := exec.Command("git", "merge-file", "-p", "-L", "local", "-L", "base", "-L", "quarantine "+label, files[0], files[1], files[2])
			content, err := cmd.Output()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				return nil, err
			}
			mode := tMode
			if oMode != "000000" {
				mode = oMode
			}
			if code < 0 || code > 127 {
				// binary: keep ours, put theirs beside it
				alt := disk + ".quarantine-" + label
				if err := writeFile(alt, tMode, blob(tOid)); err != nil {
					return nil, err
				}
				res.Conflicts = append(res.Conflicts, path+" (binary; quarantine version written to "+filepath.Base(alt)+")")
				continue
			}
			if err := writeFile(disk, mode, content); err != nil {
				return nil, err
			}
			if code > 0 {
				res.Conflicts = append(res.Conflicts, path)
			} else {
				res.Merged = append(res.Merged, path)
			}
		}
	}
	return res, nil
}
