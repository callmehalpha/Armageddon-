package store

import (
	"context"
	"database/sql"
)

// ProviderCredential is a stored Git-provider credential. Ciphertext is
// base64 of the sealed secret; it never leaves the server.
type ProviderCredential struct {
	ID, UserID, Host, Kind, Username, PublicKey, KeyID, Ciphertext string
	CreatedAt                                                      int64
}

const credCols = `id, user_id, host, kind, username, public_key, key_id, ciphertext, created_at`

func scanCred(row interface{ Scan(...any) error }) (*ProviderCredential, error) {
	var c ProviderCredential
	if err := row.Scan(&c.ID, &c.UserID, &c.Host, &c.Kind, &c.Username, &c.PublicKey, &c.KeyID, &c.Ciphertext, &c.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (s *Store) InsertCredential(c *ProviderCredential) error {
	_, err := s.db.Exec(`INSERT INTO provider_credentials (`+credCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.UserID, c.Host, c.Kind, c.Username, c.PublicKey, c.KeyID, c.Ciphertext, c.CreatedAt)
	return err
}

func (s *Store) CredentialsOfUser(userID string) ([]*ProviderCredential, error) {
	return s.queryCreds(`SELECT `+credCols+` FROM provider_credentials WHERE user_id = ? ORDER BY host, kind`, userID)
}

func (s *Store) AllCredentials() ([]*ProviderCredential, error) {
	return s.queryCreds(`SELECT ` + credCols + ` FROM provider_credentials ORDER BY id`)
}

func (s *Store) queryCreds(q string, args ...any) ([]*ProviderCredential, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProviderCredential
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CredentialFor returns the user's credential of kind for host.
func (s *Store) CredentialFor(userID, host, kind string) (*ProviderCredential, error) {
	return scanCred(s.db.QueryRow(`SELECT `+credCols+` FROM provider_credentials WHERE user_id = ? AND host = ? AND kind = ?`, userID, host, kind))
}

func (s *Store) DeleteCredential(id, userID string) error {
	res, err := s.db.Exec(`DELETE FROM provider_credentials WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResealCredentials rewrites every credential's ciphertext in one
// transaction (data-key rotation). reseal returns the new key ID and
// ciphertext for a row.
func (s *Store) ResealCredentials(reseal func(*ProviderCredential) (keyID, ciphertext string, err error)) (int, error) {
	all, err := s.AllCredentials()
	if err != nil {
		return 0, err
	}
	err = s.Tx(context.Background(), func(tx *sql.Tx) error {
		for _, c := range all {
			kid, ct, err := reseal(c)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE provider_credentials SET key_id = ?, ciphertext = ? WHERE id = ?`, kid, ct, c.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return len(all), err
}
