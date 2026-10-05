package secretbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys", "data.key")
	k, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	id1, ct := k.Seal([]byte("ghp_secret"), []byte("row-1"))
	if bytes.Contains(ct, []byte("ghp_secret")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	if pt, err := k.Open(id1, ct, []byte("row-1")); err != nil || string(pt) != "ghp_secret" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	if _, err := k.Open(id1, ct, []byte("row-2")); err == nil {
		t.Fatal("ciphertext opened under another row's aad")
	}
	tampered := append([]byte{}, ct...)
	tampered[len(tampered)-1] ^= 1
	if _, err := k.Open(id1, tampered, []byte("row-1")); err == nil {
		t.Fatal("tampered ciphertext opened")
	}

	id2, err := k.Rotate()
	if err != nil || id2 == id1 || k.ActiveID() != id2 {
		t.Fatalf("rotate: %v %s %s", err, id1, id2)
	}
	// Old data still opens, new data uses the new key; both survive reload.
	k2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := k2.Open(id1, ct, []byte("row-1")); err != nil || string(pt) != "ghp_secret" {
		t.Fatalf("old key after rotate: %q %v", pt, err)
	}
	if got, _ := k2.Seal([]byte("x"), nil); got != id2 {
		t.Fatalf("sealed with %s, want %s", got, id2)
	}
	if err := k2.Retire(); err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Open(id1, ct, []byte("row-1")); err != ErrUnknownKey {
		t.Fatalf("retired key still opens: %v", err)
	}
	k3, _ := Open(p)
	if ids := k3.IDs(); len(ids) != 1 || ids[0] != id2 {
		t.Fatalf("after retire: %v", ids)
	}
	os.Chmod(p, 0o644)
	if _, err := Open(p); err == nil {
		t.Fatal("world-readable key file accepted")
	}
}
