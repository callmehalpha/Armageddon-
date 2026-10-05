package store

import (
	"database/sql"
	"strings"
)

// Lease is the row of the `leases` table (contract §3.2, §8.2).
type Lease struct {
	WorkspaceID, HolderKind, HolderDevice, State, HandoffTo string
	Epoch, AcquiredAt, HeartbeatAt, HandoffDeadline         int64
}

type execer interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func (s *Store) ex(tx *sql.Tx) execer {
	if tx != nil {
		return tx
	}
	return s.db
}

func (s *Store) LeaseOf(tx *sql.Tx, wsID string) (*Lease, error) {
	var l Lease
	var dev, to sql.NullString
	q := `SELECT workspace_id, holder_kind, holder_device_id, state, handoff_to, epoch, acquired_at, heartbeat_at, handoff_deadline FROM leases WHERE workspace_id = ?`
	if err := s.ex(tx).QueryRow(q, wsID).Scan(&l.WorkspaceID, &l.HolderKind, &dev, &l.State, &to, &l.Epoch, &l.AcquiredAt, &l.HeartbeatAt, &l.HandoffDeadline); err != nil {
		return nil, notFound(err)
	}
	l.HolderDevice, l.HandoffTo = dev.String, to.String
	return &l, nil
}

func one(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Every lease transition below is a conditional update on the epoch it
// starts from (contract §4.1): 0 rows affected means another command won,
// and the caller reports a conflict. None of them can lower the epoch.

// BeginHandoff: HELD(h, e) → HANDOFF(h → to, e).
func (s *Store) BeginHandoff(tx *sql.Tx, wsID string, epoch int64, to string, deadline int64) (bool, error) {
	return one(s.ex(tx).Exec(`UPDATE leases SET state = 'handoff', handoff_to = ?, handoff_deadline = ?
		WHERE workspace_id = ? AND epoch = ? AND state = 'held'`, to, deadline, wsID, epoch))
}

// EndHandoff: HANDOFF(h → R, e) → HELD(h, e) (the holder refused) or
// STALE(h, e) (the holder did not answer within T_handoff).
func (s *Store) EndHandoff(tx *sql.Tx, wsID string, epoch int64, state string) (bool, error) {
	return one(s.ex(tx).Exec(`UPDATE leases SET state = ?, handoff_to = NULL, handoff_deadline = 0
		WHERE workspace_id = ? AND epoch = ? AND state = 'handoff'`, state, wsID, epoch))
}

// TransferLease: <state in fromStates>(h, e) → HELD(to, e+1). Used for a
// completed handoff and for forced takeover. The epoch increments on every
// change of holder.
func (s *Store) TransferLease(tx *sql.Tx, wsID string, epoch int64, kind, device string, now int64, fromStates ...string) (bool, error) {
	ph := strings.TrimSuffix(strings.Repeat("?, ", len(fromStates)), ", ")
	args := []any{kind, nullable(device), now, now, wsID, epoch}
	for _, st := range fromStates {
		args = append(args, st)
	}
	return one(s.ex(tx).Exec(`UPDATE leases SET holder_kind = ?, holder_device_id = ?, epoch = epoch + 1, state = 'held',
		handoff_to = NULL, handoff_deadline = 0, acquired_at = ?, heartbeat_at = ?
		WHERE workspace_id = ? AND epoch = ? AND state IN (`+ph+`)`, args...))
}

// LeaseHeartbeat records a heartbeat from the holding device at its epoch;
// STALE(h, e) → HELD(h, e) when heartbeats resume.
func (s *Store) LeaseHeartbeat(tx *sql.Tx, wsID string, epoch int64, device string, now int64) (bool, error) {
	return one(s.ex(tx).Exec(`UPDATE leases SET heartbeat_at = ?, state = CASE WHEN state = 'stale' THEN 'held' ELSE state END
		WHERE workspace_id = ? AND epoch = ? AND holder_kind = 'device' AND holder_device_id = ?`, now, wsID, epoch, device))
}

// CheckHolder is the lease half of a commit (§6.4 step 4a), as a
// conditional update inside the commit transaction: it succeeds only for
// the holder at its epoch, and counts as a heartbeat for a device.
func (s *Store) CheckHolder(tx *sql.Tx, wsID string, epoch int64, kind, device string, now int64) (bool, error) {
	if kind == "server" {
		return one(s.ex(tx).Exec(`UPDATE leases SET heartbeat_at = heartbeat_at
			WHERE workspace_id = ? AND epoch = ? AND holder_kind = 'server'`, wsID, epoch))
	}
	return s.LeaseHeartbeat(tx, wsID, epoch, device, now)
}

// MarkLeaseStale: HELD(device, e) → STALE(device, e) when the last
// heartbeat is older than cutoff. Informational only: nothing transfers.
func (s *Store) MarkLeaseStale(wsID string, epoch, cutoff int64) (bool, error) {
	return one(s.db.Exec(`UPDATE leases SET state = 'stale'
		WHERE workspace_id = ? AND epoch = ? AND state = 'held' AND holder_kind = 'device' AND heartbeat_at < ?`, wsID, epoch, cutoff))
}

// LeasesHeldByDevice lists the workspaces whose lease the device holds.
func (s *Store) LeasesHeldByDevice(device string) ([]string, error) {
	rows, err := s.db.Query(`SELECT workspace_id FROM leases WHERE holder_kind = 'device' AND holder_device_id = ?`, device)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- replicas (display only; the agent owns replica state, §3.3) ----

type Replica struct {
	WorkspaceID, DeviceID, DeviceName, State, Mode, AgentVersion string
	LastAppliedSeq, Pending, LastSeenAt                          int64
}

func (s *Store) UpsertReplica(r *Replica) error {
	_, err := s.db.Exec(`INSERT INTO replicas (workspace_id, device_id, state, mode, last_applied_seq, pending, last_seen_at, agent_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id, device_id) DO UPDATE SET state = excluded.state, mode = excluded.mode,
		last_applied_seq = excluded.last_applied_seq, pending = excluded.pending, last_seen_at = excluded.last_seen_at,
		agent_version = excluded.agent_version`,
		r.WorkspaceID, r.DeviceID, r.State, r.Mode, r.LastAppliedSeq, r.Pending, r.LastSeenAt, r.AgentVersion)
	return err
}

func (s *Store) Replicas(wsID string) ([]*Replica, error) {
	rows, err := s.db.Query(`SELECT r.workspace_id, r.device_id, d.name, r.state, r.mode, r.agent_version, r.last_applied_seq, r.pending, r.last_seen_at
		FROM replicas r JOIN devices d ON d.id = r.device_id WHERE r.workspace_id = ? AND d.revoked_at IS NULL ORDER BY d.name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Replica
	for rows.Next() {
		var r Replica
		if err := rows.Scan(&r.WorkspaceID, &r.DeviceID, &r.DeviceName, &r.State, &r.Mode, &r.AgentVersion, &r.LastAppliedSeq, &r.Pending, &r.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
