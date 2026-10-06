package backup

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// randomPassphrase returns a passphrase generated for this test run, so no
// password literal lives in the repository.
func randomPassphrase(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return []byte(hex.EncodeToString(b))
}
