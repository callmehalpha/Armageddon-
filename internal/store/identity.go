package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type User struct {
	ID, Username, DisplayName, PasswordHash, Role string
	CreatedAt                                     int64
	Disabled                                      bool
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var disabled sql.NullInt64
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.CreatedAt, &disabled); err != nil {
		return nil, notFound(err)
	}
	u.Disabled = disabled.Valid
	return &u, nil
}

const userCols = `id, username, display_name, password_hash, role, created_at, disabled_at`

func (s *Store) CreateUser(u *User) error {
	_, err := s.db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, u.CreatedAt)
	return err
}

func (s *Store) UserByID(id string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByName(name string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, name))
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- sessions ----

type Session struct {
	ID, UserID, TokenHash, CSRF                    string
	CreatedAt, LastSeenAt, ExpiresAt, ReauthAt int64
}

func (s *Store) CreateSession(x *Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions (id, user_id, token_hash, csrf_token, created_at, last_seen_at, expires_at, reauth_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		x.ID, x.UserID, x.TokenHash, x.CSRF, x.CreatedAt, x.LastSeenAt, x.ExpiresAt, x.ReauthAt)
	return err
}

func (s *Store) SessionByTokenHash(h string) (*Session, error) {
	var x Session
	err := s.db.QueryRow(`SELECT id, user_id, token_hash, csrf_token, created_at, last_seen_at, expires_at, reauth_at FROM sessions WHERE token_hash = ?`, h).
		Scan(&x.ID, &x.UserID, &x.TokenHash, &x.CSRF, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.ReauthAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &x, nil
}

func (s *Store) TouchSession(id string, now int64) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, now, id)
	return err
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ---- one-time tokens (setup URL, invites) ----

func (s *Store) CreateOneTimeToken(hash, kind, role, createdBy string, expires int64) error {
	_, err := s.db.Exec(`INSERT INTO one_time_tokens (token_hash, kind, role, created_by, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hash, kind, role, createdBy, expires)
	return err
}

// ConsumeOneTimeToken atomically marks a valid token used and returns its role.
func (s *Store) ConsumeOneTimeToken(tx *sql.Tx, hash, kind string, now int64) (string, error) {
	var role string
	err := tx.QueryRow(`SELECT role FROM one_time_tokens WHERE token_hash = ? AND kind = ? AND consumed_at IS NULL AND expires_at > ?`, hash, kind, now).Scan(&role)
	if err != nil {
		return "", notFound(err)
	}
	res, err := tx.Exec(`UPDATE one_time_tokens SET consumed_at = ? WHERE token_hash = ? AND consumed_at IS NULL`, now, hash)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", ErrNotFound
	}
	return role, nil
}

func (s *Store) CreateUserTx(tx *sql.Tx, u *User) error {
	_, err := tx.Exec(`INSERT INTO users (id, username, display_name, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, u.CreatedAt)
	return err
}

// ---- devices ----

type Device struct {
	ID, UserID, Name, Platform, PublicKey string
	CreatedAt, LastSeenAt                 int64
	Revoked                               bool
}

func scanDevice(row interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var revoked sql.NullInt64
	if err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.PublicKey, &d.CreatedAt, &d.LastSeenAt, &revoked); err != nil {
		return nil, notFound(err)
	}
	d.Revoked = revoked.Valid
	return &d, nil
}

const deviceCols = `id, user_id, name, platform, public_key, created_at, last_seen_at, revoked_at`

func (s *Store) DeviceByID(id string) (*Device, error) {
	return scanDevice(s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

func (s *Store) DevicesOfUser(userID string) ([]*Device, error) {
	rows, err := s.db.Query(`SELECT `+deviceCols+` FROM devices WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) RevokeDevice(id, userID string, now int64) error {
	return s.Tx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE devices SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, now, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(`DELETE FROM device_tokens WHERE device_id = ?`, id)
		return err
	})
}

func (s *Store) TouchDevice(id string, now int64) {
	s.db.Exec(`UPDATE devices SET last_seen_at = ? WHERE id = ?`, now, id)
}

// ---- pairing ----

type Pairing struct {
	ID, UserCodeHash, PollSecretHash, PublicKey, Name, Platform string
	ExpiresAt                                                   int64
	ApprovedBy, DeviceID                                        string
}

func (s *Store) CreatePairing(p *Pairing) error {
	_, err := s.db.Exec(`INSERT INTO device_pairings (id, user_code_hash, poll_secret_hash, public_key, name, platform, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UserCodeHash, p.PollSecretHash, p.PublicKey, p.Name, p.Platform, p.ExpiresAt)
	return err
}

func scanPairing(row interface{ Scan(...any) error }) (*Pairing, error) {
	var p Pairing
	var by, dev sql.NullString
	if err := row.Scan(&p.ID, &p.UserCodeHash, &p.PollSecretHash, &p.PublicKey, &p.Name, &p.Platform, &p.ExpiresAt, &by, &dev); err != nil {
		return nil, notFound(err)
	}
	p.ApprovedBy, p.DeviceID = by.String, dev.String
	return &p, nil
}

const pairingCols = `id, user_code_hash, poll_secret_hash, public_key, name, platform, expires_at, approved_by, device_id`

func (s *Store) PairingByCode(codeHash string) (*Pairing, error) {
	return scanPairing(s.db.QueryRow(`SELECT `+pairingCols+` FROM device_pairings WHERE user_code_hash = ?`, codeHash))
}

func (s *Store) PairingByID(id string) (*Pairing, error) {
	return scanPairing(s.db.QueryRow(`SELECT `+pairingCols+` FROM device_pairings WHERE id = ?`, id))
}

// ApprovePairing creates the device and links it to the pairing atomically.
func (s *Store) ApprovePairing(p *Pairing, userID, deviceID string, now int64) error {
	return s.Tx(context.Background(), func(tx *sql.Tx) error {
		// Device first: device_pairings.device_id references it and SQLite
		// checks foreign keys immediately.
		if _, err := tx.Exec(`INSERT INTO devices (id, user_id, name, platform, public_key, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			deviceID, userID, p.Name, p.Platform, p.PublicKey, now, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errors.New("this device key is already paired")
			}
			return err
		}
		res, err := tx.Exec(`UPDATE device_pairings SET approved_by = ?, approved_at = ?, device_id = ? WHERE id = ? AND approved_at IS NULL AND expires_at > ?`,
			userID, now, deviceID, p.ID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("pairing expired or already approved")
		}
		return nil
	})
}

// ---- device auth ----

func (s *Store) CreateNonce(nonce, deviceID string, expires int64) error {
	_, err := s.db.Exec(`INSERT INTO auth_nonces (nonce, device_id, expires_at) VALUES (?, ?, ?)`, nonce, deviceID, expires)
	return err
}

// ConsumeNonce deletes the nonce; it is valid only once and only before expiry.
func (s *Store) ConsumeNonce(nonce, deviceID string, now int64) error {
	res, err := s.db.Exec(`DELETE FROM auth_nonces WHERE nonce = ? AND device_id = ? AND expires_at > ?`, nonce, deviceID, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateDeviceToken(hash, deviceID string, expires int64) error {
	_, err := s.db.Exec(`INSERT INTO device_tokens (token_hash, device_id, expires_at) VALUES (?, ?, ?)`, hash, deviceID, expires)
	return err
}

// DeviceForToken resolves a live access token to a non-revoked device.
func (s *Store) DeviceForToken(hash string, now int64) (*Device, error) {
	return scanDevice(s.db.QueryRow(`SELECT d.id, d.user_id, d.name, d.platform, d.public_key, d.created_at, d.last_seen_at, d.revoked_at
		FROM device_tokens t JOIN devices d ON d.id = t.device_id
		WHERE t.token_hash = ? AND t.expires_at > ? AND d.revoked_at IS NULL`, hash, now))
}

// Prune removes expired sessions, nonces, tokens and pairings.
func (s *Store) Prune(now int64) {
	s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now)
	s.db.Exec(`DELETE FROM auth_nonces WHERE expires_at <= ?`, now)
	s.db.Exec(`DELETE FROM device_tokens WHERE expires_at <= ?`, now)
	s.db.Exec(`DELETE FROM device_pairings WHERE expires_at <= ? AND approved_at IS NULL`, now)
}
