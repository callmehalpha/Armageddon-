package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "correct horse") || VerifyPassword(h, "wrong horse") {
		t.Fatal("verify mismatch")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestChallenge(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, ChallengeMessage("dev", "n1")))
	if !VerifyChallenge(pub, "dev", "n1", sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyChallenge(pub, "dev", "n2", sig) || VerifyChallenge(pub, "other", "n1", sig) {
		t.Fatal("signature accepted for a different nonce or device")
	}
}
