package agent

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// agentRunning reports whether an agent (`agent run` or `follow`) holds
// the replica: the lock is busy.
func agentRunning(wsID string) bool {
	lk, err := lockReplica(wsID)
	if err != nil {
		return errors.Is(err, errBusy)
	}
	lk.release()
	return false
}

func leaseLine(l leaseInfo) string {
	s := fmt.Sprintf("%s holds it (epoch %d, %s)", l.HolderName, l.Epoch, l.State)
	if l.HolderKind == "device" && l.Now > 0 {
		s += fmt.Sprintf(", last heartbeat %s ago", (time.Duration(l.Now-l.HeartbeatAt) * time.Millisecond).Round(time.Second))
	}
	if l.State == "stale" {
		s += " — STALE: it may be asleep or gone; `armageddon work remote --force` (or `work local --force`) takes over"
	}
	return s
}

// WorkLocal is `armageddon work local`: take the workspace's lease for
// this device (explicit, never automatic: decision Q2). The running agent
// processes the grant (fast-forward or quarantine first, §3.3).
func (c *Client) WorkLocal(wsID string, force bool, out io.Writer) error {
	if !agentRunning(wsID) {
		return errors.New("no agent is running for this replica: start `armageddon agent run` (or `armageddon follow` in it) first, so local edits are captured and sent")
	}
	start := time.Now().UnixMilli()
	path, body := "/api/workspaces/"+wsID+"/lease/acquire", map[string]any{"to": "device"}
	if force {
		path = "/api/workspaces/" + wsID + "/lease/force"
		fmt.Fprintln(out, "Forcing the workspace onto this device; the previous holder's unsent changes go to quarantine when it returns.")
	} else {
		fmt.Fprintln(out, "Asking for the workspace (the current holder flushes and releases it)…")
	}
	var res current
	if _, err := c.call("POST", path, body, &res); err != nil {
		if res.Code == "holder_unresponsive" {
			return fmt.Errorf("%s\nTo take over anyway: armageddon work local --force", res.Error)
		}
		if res.Error != "" {
			return errors.New(res.Error)
		}
		return err
	}
	fmt.Fprintf(out, "Granted: this device holds the workspace at epoch %d.\n", res.Lease.Epoch)
	return waitAgent(wsID, start, func(st *replicaState) bool { return st.Epoch >= res.Lease.Epoch && st.Mode == "write" }, out)
}

// waitAgent waits for the replica's agent to act and prints its notices.
func waitAgent(wsID string, since int64, done func(*replicaState) bool, out io.Writer) error {
	printed := map[notice]bool{}
	for i := 0; i < 120; i++ {
		st, err := loadState(wsID)
		if err == nil {
			for _, n := range st.Notices {
				if n.At >= since && !printed[n] {
					printed[n] = true
					fmt.Fprintf(out, "  %s\n", n.Msg)
				}
			}
			if done(st) {
				fmt.Fprintf(out, "Replica %s is WRITING: edits are checkpointed to the server; branches and tags are pushed.\n", st.Path)
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("granted, but the agent has not picked the grant up yet; it will on its next resync (check `armageddon status`)")
}

// WorkRemote is `armageddon work remote`: hand the workspace back to the
// server seat. With force, take it without the holder's cooperation.
func (c *Client) WorkRemote(wsID string, restart, force bool, out io.Writer) error {
	path, body := "/api/workspaces/"+wsID+"/lease/acquire", map[string]any{"to": "server", "restart": restart}
	if force {
		path, body = "/api/workspaces/"+wsID+"/lease/force", map[string]any{"to": "server"}
	}
	var res current
	if _, err := c.call("POST", path, body, &res); err != nil {
		if res.Code == "holder_unresponsive" {
			return fmt.Errorf("%s\nTo take over anyway: armageddon work remote --force", res.Error)
		}
		if res.Error != "" {
			return errors.New(res.Error)
		}
		return err
	}
	fmt.Fprintf(out, "The server seat holds the workspace (epoch %d, checkpoint #%d). Work in the browser terminal or IDE.\n", res.Lease.Epoch, res.Seq)
	if restart {
		fmt.Fprintln(out, "Restart of the workspace's dev processes requested.")
	}
	return nil
}

// Status reports replica health and the lease (`armageddon status`).
func (c *Client) Status(wsID string, out io.Writer) error {
	st, err := loadState(wsID)
	if err != nil {
		return err
	}
	sh := shadowFor(wsID, st.Path)
	fmt.Fprintf(out, "replica:     %s\nworkspace:   %s\napplied:     checkpoint #%d (%.12s)\n", st.Path, wsID, st.AppliedSeq, st.AppliedOid)
	if cur, err := c.current(wsID, 0, 0); err == nil {
		lag := "up to date"
		if cur.Seq > st.AppliedSeq && st.Epoch == 0 {
			lag = fmt.Sprintf("%d checkpoint(s) behind", cur.Seq-st.AppliedSeq)
		}
		fmt.Fprintf(out, "server:      checkpoint #%d (%s)\nlease:       %s\n", cur.Seq, lag, leaseLine(cur.Lease))
	} else {
		fmt.Fprintf(out, "server:      unreachable (%v)\n", err)
	}
	if agentRunning(wsID) {
		state := st.Status
		if st.Offline {
			state += " (OFFLINE)"
		}
		fmt.Fprintf(out, "agent:       running; replica %s, mode %s", state, st.Mode)
		if st.Epoch > 0 {
			fmt.Fprintf(out, ", epoch %d, %d checkpoint(s) queued", st.Epoch, len(st.Pending))
		}
		fmt.Fprintln(out)
		if len(st.Outbox) > 0 {
			fmt.Fprintf(out, "quarantine:  %d local quarantine(s) waiting to upload\n", len(st.Outbox))
		}
		if st.Status == "DIRTY" {
			fmt.Fprintln(out, "local:       DIRTY — this replica is read-only; run `armageddon work local` to take ownership, or the edits are quarantined at the next checkpoint")
		}
		return nil
	}
	fmt.Fprintln(out, "agent:       not running (start `armageddon agent run`)")
	if st.AppliedOid != "" {
		lk, err := lockReplica(wsID)
		if err != nil {
			return err
		}
		defer lk.release()
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
