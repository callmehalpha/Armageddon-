// Package secretbox encrypts small secrets at rest with the server data key
// (contract §7.5, plan M2.8). The key file holds a list of AES-256 keys; the
// first is active and seals new data, the others only open existing data
// until a rotation has re-encrypted everything and retired them.
//
// Ciphertext layout: 12-byte random nonce || AES-256-GCM(plaintext, aad).
// The caller binds each ciphertext to its row through aad, so a ciphertext
// copied into another row does not decrypt.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type key struct {
	ID      string `json:"id"`
	Key     []byte `json:"key"` // 32 bytes, base64 in JSON
	Created int64  `json:"created_at"`
}

// Keyring is the set of data keys stored in one 0600 file.
type Keyring struct {
	path string
	mu   sync.Mutex
	keys []key // keys[0] is active
}

var ErrUnknownKey = errors.New("secretbox: unknown data key")

// Open loads the keyring at path, creating it with one fresh key if absent.
func Open(path string) (*Keyring, error) {
	k := &Keyring{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		k.keys = []key{newKey()}
		return k, k.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &k.keys); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(k.keys) == 0 {
		return nil, fmt.Errorf("%s: no keys", path)
	}
	for _, x := range k.keys {
		if len(x.Key) != 32 {
			return nil, fmt.Errorf("%s: key %s is not 32 bytes", path, x.ID)
		}
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %v is readable by others; chmod 600 it", path, fi.Mode().Perm())
	}
	return k, nil
}

func newKey() key {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := make([]byte, 6)
	rand.Read(id)
	return key{ID: hex.EncodeToString(id), Key: b, Created: time.Now().UnixMilli()}
}

func (k *Keyring) save() error {
	b, err := json.MarshalIndent(k.keys, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

// ActiveID is the ID of the key that seals new data.
func (k *Keyring) ActiveID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keys[0].ID
}

// IDs lists every key ID, active first.
func (k *Keyring) IDs() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, x := range k.keys {
		out = append(out, x.ID)
	}
	return out
}

func aead(raw []byte) cipher.AEAD {
	b, err := aes.NewCipher(raw)
	if err != nil {
		panic(err) // keys are validated to 32 bytes
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

// Seal encrypts plaintext with the active key.
func (k *Keyring) Seal(plaintext, aad []byte) (keyID string, ciphertext []byte) {
	k.mu.Lock()
	active := k.keys[0]
	k.mu.Unlock()
	g := aead(active.Key)
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return active.ID, g.Seal(nonce, nonce, plaintext, aad)
}

// Open decrypts ciphertext sealed with key keyID.
func (k *Keyring) Open(keyID string, ciphertext, aad []byte) ([]byte, error) {
	k.mu.Lock()
	var raw []byte
	for _, x := range k.keys {
		if x.ID == keyID {
			raw = x.Key
		}
	}
	k.mu.Unlock()
	if raw == nil {
		return nil, ErrUnknownKey
	}
	g := aead(raw)
	if len(ciphertext) < g.NonceSize() {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	pt, err := g.Open(nil, ciphertext[:g.NonceSize()], ciphertext[g.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("secretbox: decryption failed")
	}
	return pt, nil
}

// Rotate adds a fresh active key. Existing keys stay usable for Open until
// Retire removes them.
func (k *Keyring) Rotate() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := newKey()
	k.keys = append([]key{n}, k.keys...)
	if err := k.save(); err != nil {
		k.keys = k.keys[1:]
		return "", err
	}
	return n.ID, nil
}

// Retire removes every key except the active one. Call it only after all
// data sealed with older keys has been re-sealed.
func (k *Keyring) Retire() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	old := k.keys
	k.keys = k.keys[:1]
	if err := k.save(); err != nil {
		k.keys = old
		return err
	}
	return nil
}
