package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

type Workspace struct {
	ID, OwnerID, Name, Slug, State, StateReason, SourceKind, SourceURL, OSUser string
	CurrentCheckpoint                                                          string
	CheckpointSeq, RowVersion, CreatedAt, UpdatedAt                            int64
}

const wsCols = `id, owner_id, name, slug, state, state_reason, source_kind, source_url, os_user, COALESCE(current_checkpoint_id, ''), checkpoint_seq, row_version, created_at, updated_at`

func scanWS(row interface{ Scan(...any) error }) (*Workspace, error) {
	var w Workspace
	if err := row.Scan(&w.ID, &w.OwnerID, &w.Name, &w.Slug, &w.State, &w.StateReason, &w.SourceKind, &w.SourceURL, &w.OSUser,
		&w.CurrentCheckpoint, &w.CheckpointSeq, &w.RowVersion, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	return &w, nil
}

// CreateWorkspace inserts the workspace, its owner membership and the initial
// lease (held by the server seat at epoch 1) in one transaction.
func (s *Store) CreateWorkspace(tx *sql.Tx, w *Workspace) error {
	if _, err := tx.Exec(`INSERT INTO workspaces (id, owner_id, name, slug, state, source_kind, source_url, os_user, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.OwnerID, w.Name, w.Slug, w.State, w.SourceKind, w.SourceURL, w.OSUser, w.CreatedAt, w.UpdatedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role, added_by, created_at) VALUES (?, ?, 'owner', ?, ?)`,
		w.ID, w.OwnerID, w.OwnerID, w.CreatedAt); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO leases (workspace_id, holder_kind, epoch, state, acquired_at, heartbeat_at) VALUES (?, 'server', 1, 'held', ?, ?)`,
		w.ID, w.CreatedAt, w.CreatedAt)
	return err
}

func (s *Store) WorkspaceByID(id string) (*Workspace, error) {
	return scanWS(s.db.QueryRow(`SELECT `+wsCols+` FROM workspaces WHERE id = ? AND deleted_at IS NULL`, id))
}

// WorkspaceForMember returns the workspace and the caller's role, or
// ErrNotFound for non-members (who must see 404, not 403).
func (s *Store) WorkspaceForMember(id, userID string) (*Workspace, string, error) {
	var role string
	err := s.db.QueryRow(`SELECT role FROM workspace_members m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.workspace_id = ? AND m.user_id = ? AND w.deleted_at IS NULL`, id, userID).Scan(&role)
	if err != nil {
		return nil, "", notFound(err)
	}
	w, err := s.WorkspaceByID(id)
	return w, role, err
}

func (s *Store) WorkspacesForUser(userID string) ([]*Workspace, error) {
	rows, err := s.db.Query(`SELECT `+prefix(wsCols, "w.")+` FROM workspaces w JOIN workspace_members m ON m.workspace_id = w.id
		WHERE m.user_id = ? AND w.deleted_at IS NULL ORDER BY w.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w, err := scanWS(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) AllWorkspaces() ([]*Workspace, error) {
	rows, err := s.db.Query(`SELECT ` + wsCols + ` FROM workspaces WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w, err := scanWS(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func prefix(cols, p string) string {
	out := ""
	for i, c := range splitCols(cols) {
		if i > 0 {
			out += ", "
		}
		if len(c) > 9 && c[:9] == "COALESCE(" {
			out += "COALESCE(" + p + c[9:]
		} else {
			out += p + c
		}
	}
	return out
}

func splitCols(cols string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range cols {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, trim(cols[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, trim(cols[start:]))
}

func trim(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return s
}

// SetWorkspaceState is a conditional lifecycle transition.
func (s *Store) SetWorkspaceState(id, from, to, reason string, now int64) error {
	res, err := s.db.Exec(`UPDATE workspaces SET state = ?, state_reason = ?, updated_at = ?, row_version = row_version + 1 WHERE id = ? AND state = ?`,
		to, reason, now, id, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("workspace state changed concurrently")
	}
	return nil
}

func (s *Store) Members(wsID string) ([][3]string, error) {
	rows, err := s.db.Query(`SELECT u.id, u.username, m.role FROM workspace_members m JOIN users u ON u.id = m.user_id WHERE m.workspace_id = ? ORDER BY u.username`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var r [3]string
		if err := rows.Scan(&r[0], &r[1], &r[2]); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AddMember(wsID, userID, role, by string, now int64) error {
	_, err := s.db.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role, added_by, created_at) VALUES (?, ?, ?, ?, ?)`, wsID, userID, role, by, now)
	return err
}

// ---- checkpoints ----

type Checkpoint struct {
	ID, WorkspaceID, ParentID, AuthorKind, AuthorDevice, HeadRef, HeadOid, WorktreeTree, Kind string
	Seq, Epoch, CreatedAt                                                                     int64
}

const cpCols = `id, workspace_id, COALESCE(parent_id, ''), author_kind, COALESCE(author_device_id, ''), head_ref, head_oid, worktree_tree, kind, seq, epoch, created_at`

func scanCP(row interface{ Scan(...any) error }) (*Checkpoint, error) {
	var c Checkpoint
	if err := row.Scan(&c.ID, &c.WorkspaceID, &c.ParentID, &c.AuthorKind, &c.AuthorDevice, &c.HeadRef, &c.HeadOid, &c.WorktreeTree, &c.Kind, &c.Seq, &c.Epoch, &c.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (s *Store) CheckpointByID(wsID, id string) (*Checkpoint, error) {
	return scanCP(s.db.QueryRow(`SELECT `+cpCols+` FROM checkpoints WHERE workspace_id = ? AND id = ?`, wsID, id))
}

// CheckpointByIDTx is CheckpointByID inside a transaction.
func (s *Store) CheckpointByIDTx(tx *sql.Tx, wsID, id string) (*Checkpoint, error) {
	return scanCP(tx.QueryRow(`SELECT `+cpCols+` FROM checkpoints WHERE workspace_id = ? AND id = ?`, wsID, id))
}

func (s *Store) CheckpointBySeq(wsID string, seq int64) (*Checkpoint, error) {
	return scanCP(s.db.QueryRow(`SELECT `+cpCols+` FROM checkpoints WHERE workspace_id = ? AND seq = ?`, wsID, seq))
}

func (s *Store) Checkpoints(wsID string, limit int) ([]*Checkpoint, error) {
	rows, err := s.db.Query(`SELECT `+cpCols+` FROM checkpoints WHERE workspace_id = ? ORDER BY seq DESC LIMIT ?`, wsID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Checkpoint
	for rows.Next() {
		c, err := scanCP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) InsertCheckpoint(tx *sql.Tx, c *Checkpoint) error {
	var parent, dev any
	if c.ParentID != "" {
		parent = c.ParentID
	}
	if c.AuthorDevice != "" {
		dev = c.AuthorDevice
	}
	_, err := tx.Exec(`INSERT INTO checkpoints (id, workspace_id, seq, epoch, parent_id, author_kind, author_device_id, head_ref, head_oid, worktree_tree, kind, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.WorkspaceID, c.Seq, c.Epoch, parent, c.AuthorKind, dev, c.HeadRef, c.HeadOid, c.WorktreeTree, c.Kind, c.CreatedAt)
	return err
}

// AdvanceCurrent is the compare-and-swap on the workspace's current pointer.
func (s *Store) AdvanceCurrent(tx *sql.Tx, wsID, expectedParent, newID string, newSeq, now int64) (bool, error) {
	var res sql.Result
	var err error
	if expectedParent == "" {
		res, err = tx.Exec(`UPDATE workspaces SET current_checkpoint_id = ?, checkpoint_seq = ?, row_version = row_version + 1, updated_at = ?
			WHERE id = ? AND current_checkpoint_id IS NULL AND state = 'ready'`, newID, newSeq, now, wsID)
	} else {
		res, err = tx.Exec(`UPDATE workspaces SET current_checkpoint_id = ?, checkpoint_seq = ?, row_version = row_version + 1, updated_at = ?
			WHERE id = ? AND current_checkpoint_id = ? AND state = 'ready'`, newID, newSeq, now, wsID, expectedParent)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ---- quarantines ----

type Quarantine struct {
	ID, WorkspaceID, SourceKind, SourceDevice, CheckpointID, BaseCheckpointID, Reason string
	CreatedAt                                                                         int64
}

func (s *Store) InsertQuarantine(q *Quarantine) error {
	var dev, base any
	if q.SourceDevice != "" {
		dev = q.SourceDevice
	}
	if q.BaseCheckpointID != "" {
		base = q.BaseCheckpointID
	}
	_, err := s.db.Exec(`INSERT INTO quarantines (id, workspace_id, source_kind, source_device_id, checkpoint_id, base_checkpoint_id, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		q.ID, q.WorkspaceID, q.SourceKind, dev, q.CheckpointID, base, q.Reason, q.CreatedAt)
	return err
}

func (s *Store) Quarantines(wsID string) ([]*Quarantine, error) {
	rows, err := s.db.Query(`SELECT id, workspace_id, source_kind, COALESCE(source_device_id, ''), checkpoint_id, COALESCE(base_checkpoint_id, ''), reason, created_at
		FROM quarantines WHERE workspace_id = ? AND resolved_at IS NULL ORDER BY created_at DESC`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Quarantine
	for rows.Next() {
		var q Quarantine
		if err := rows.Scan(&q.ID, &q.WorkspaceID, &q.SourceKind, &q.SourceDevice, &q.CheckpointID, &q.BaseCheckpointID, &q.Reason, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &q)
	}
	return out, rows.Err()
}

// ---- events ----

type Event struct {
	ID, WorkspaceID, ActorKind, ActorID, Type string
	TS                                        int64
	Payload                                   json.RawMessage
}

func (s *Store) AddEvent(e *Event) error {
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	}
	var ws any
	if e.WorkspaceID != "" {
		ws = e.WorkspaceID
	}
	_, err := s.db.Exec(`INSERT INTO events (id, ts, workspace_id, actor_kind, actor_id, type, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TS, ws, e.ActorKind, e.ActorID, e.Type, string(e.Payload))
	return err
}

func (s *Store) Events(wsID string, limit int) ([]*Event, error) {
	rows, err := s.db.Query(`SELECT id, ts, COALESCE(workspace_id, ''), actor_kind, actor_id, type, payload FROM events WHERE workspace_id = ? ORDER BY ts DESC, id DESC LIMIT ?`, wsID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var p string
		if err := rows.Scan(&e.ID, &e.TS, &e.WorkspaceID, &e.ActorKind, &e.ActorID, &e.Type, &p); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(p)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *Store) QuarantineByID(wsID, id string) (*Quarantine, error) {
	var q Quarantine
	err := s.db.QueryRow(`SELECT id, workspace_id, source_kind, COALESCE(source_device_id, ''), checkpoint_id, COALESCE(base_checkpoint_id, ''), reason, created_at
		FROM quarantines WHERE workspace_id = ? AND id = ? AND resolved_at IS NULL`, wsID, id).
		Scan(&q.ID, &q.WorkspaceID, &q.SourceKind, &q.SourceDevice, &q.CheckpointID, &q.BaseCheckpointID, &q.Reason, &q.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &q, nil
}

// QuarantineByCheckpoint finds a device's uncleared quarantine of a
// checkpoint (quarantine uploads are retried and must be idempotent).
func (s *Store) QuarantineByCheckpoint(wsID, device, cp string) (*Quarantine, error) {
	var id string
	if err := s.db.QueryRow(`SELECT id FROM quarantines WHERE workspace_id = ? AND source_device_id = ? AND checkpoint_id = ? AND resolved_at IS NULL`,
		wsID, device, cp).Scan(&id); err != nil {
		return nil, notFound(err)
	}
	return s.QuarantineByID(wsID, id)
}

// ResolveQuarantine marks a quarantine cleared (decision Q6: quarantines
// are kept until explicitly cleared; nothing resolves them automatically).
func (s *Store) ResolveQuarantine(wsID, id, resolution string, now int64) (bool, error) {
	return one(s.db.Exec(`UPDATE quarantines SET resolved_at = ?, resolution = ? WHERE workspace_id = ? AND id = ? AND resolved_at IS NULL`,
		now, resolution, wsID, id))
}

// OpenQuarantinesOfDevice counts a device's uncleared quarantines in a
// workspace (the per-device quota, §6.7).
func (s *Store) OpenQuarantinesOfDevice(wsID, device string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM quarantines WHERE workspace_id = ? AND source_device_id = ? AND resolved_at IS NULL`, wsID, device).Scan(&n)
	return n, err
}
