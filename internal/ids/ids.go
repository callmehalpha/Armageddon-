// Package ids generates identifiers and secrets.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"
)

var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// New returns a ULID-style identifier: 48-bit millisecond time + 80 random
// bits, Crockford base32, lexically sortable by creation time.
func New() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	return crockford.EncodeToString(b[:])
}

// Secret returns n random bytes encoded for use in URLs and headers.
func Secret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(crockford.EncodeToString(b))
}

// Hash is the one-way form of a secret as stored in the database.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// UserCode returns a short human-typable pairing code like "WDJB-MJHT".
func UserCode() string {
	const alphabet = "BCDFGHJKLMNPQRSTVWXZ" // no vowels: avoids words, no 0/O 1/I confusion
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	out := make([]byte, 0, 9)
	for i, v := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(v)%len(alphabet)])
	}
	return string(out)
}
