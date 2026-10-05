// Package identity holds the credential primitives of contract §7:
// argon2id password hashing and Ed25519 device-key challenge verification.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (§7.1): m=64 MiB, t=3, p=1.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

var b64 = base64.RawStdEncoding

// HashPassword returns a self-describing argon2id hash.
func HashPassword(pw string) (string, error) {
	if len(pw) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash produced by HashPassword.
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ParsePublicKey decodes a base64 Ed25519 public key as sent by an agent.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// ChallengeMessage is what a device signs to obtain an access token. The
// domain prefix stops a signature being replayed in another protocol.
func ChallengeMessage(deviceID, nonce string) []byte {
	return []byte("armageddon-device-auth-v1\n" + deviceID + "\n" + nonce)
}

// VerifyChallenge checks a base64 signature over ChallengeMessage.
func VerifyChallenge(pub ed25519.PublicKey, deviceID, nonce, sigB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, ChallengeMessage(deviceID, nonce), sig)
}

// Fingerprint is a short human-comparable form of a public key, shown on the
// pairing approval page and by the CLI.
func Fingerprint(pubB64 string) string {
	b, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(b) < 8 {
		return "invalid"
	}
	var parts []string
	for _, x := range b[:8] {
		parts = append(parts, fmt.Sprintf("%02x", x))
	}
	return strings.Join(parts, ":")
}
