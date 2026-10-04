package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	for i := 0; i < 2; i++ {
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestOneTimeTokenSingleUse(t *testing.T) {
	s := open(t)
	if err := s.CreateOneTimeToken("h", "setup", "admin", "", Now()+60000); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *sql.Tx) error { _, err := s.ConsumeOneTimeToken(tx, "h", "setup", Now()); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error { _, err := s.ConsumeOneTimeToken(tx, "h", "setup", Now()); return err }); err != ErrNotFound {
		t.Fatalf("second use: %v", err)
	}
}

func TestCheckpointCAS(t *testing.T) {
	s := open(t)
	now := Now()
	if err := s.CreateUser(&User{ID: "u", Username: "u", PasswordHash: "x", Role: "admin", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *sql.Tx) error {
		return s.CreateWorkspace(tx, &Workspace{ID: "w", OwnerID: "u", Name: "w", Slug: "w", State: "ready", SourceKind: "empty", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	var ok bool
	s.Tx(ctx, func(tx *sql.Tx) (err error) { ok, err = s.AdvanceCurrent(tx, "w", "", "c1", 1, now); return })
	if !ok {
		t.Fatal("first advance should succeed")
	}
	s.Tx(ctx, func(tx *sql.Tx) (err error) { ok, err = s.AdvanceCurrent(tx, "w", "", "c2", 2, now); return })
	if ok {
		t.Fatal("advance with stale parent must fail")
	}
	s.Tx(ctx, func(tx *sql.Tx) (err error) { ok, err = s.AdvanceCurrent(tx, "w", "c1", "c2", 2, now); return })
	if !ok {
		t.Fatal("advance with correct parent should succeed")
	}
	if _, _, err := s.WorkspaceForMember("w", "nobody"); err != ErrNotFound {
		t.Fatalf("non-member must get ErrNotFound, got %v", err)
	}
}
