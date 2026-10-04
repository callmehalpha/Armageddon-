// Package agent is the device side: pairing, device tokens, the git
// credential helper, and follower replicas (plan M6).
package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/identity"
)

// Config is the device's local identity. The private key lives in a
// separate 0600 file (OS keychain integration is plan M6.1).
type Config struct {
	Server   string `json:"server"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

func configDir() string {
	if d := os.Getenv("ARMAGEDDON_CONFIG_DIR"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "armageddon")
	}
	return ".armageddon"
}

func dataDir() string {
	if d := os.Getenv("ARMAGEDDON_DATA_DIR"); d != "" {
		return d
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".local", "share", "armageddon")
	}
	return ".armageddon-data"
}

func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil {
		return nil, errors.New("not logged in: run `armageddon login <server-url>`")
	}
	var c Config
	return &c, json.Unmarshal(b, &c)
}

func (c *Config) save() error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(configDir(), "config.json"), b, 0o600)
}

func keyPath() string { return filepath.Join(configDir(), "device.key") }

func loadOrCreateKey() (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(keyPath()); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, errors.New("corrupt device key at " + keyPath())
		}
		return ed25519.PrivateKey(raw), nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return nil, err
	}
	return priv, os.WriteFile(keyPath(), []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600)
}

// Client talks to the server as this device.
type Client struct {
	Cfg  *Config
	key  ed25519.PrivateKey
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewClient() (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey()
	if err != nil {
		return nil, err
	}
	return &Client{Cfg: cfg, key: key, http: &http.Client{Timeout: 90 * time.Second}}, nil
}

func (c *Client) url(p string) string { return strings.TrimRight(c.Cfg.Server, "/") + p }

func postJSON(hc *http.Client, url string, in, out any) error {
	b, _ := json.Marshal(in)
	resp, err := hc.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// Token returns a valid access token, signing a fresh challenge when needed
// (§7.3: nothing reusable is stored on disk).
func (c *Client) Token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	var ch struct {
		Nonce string `json:"nonce"`
	}
	if err := postJSON(c.http, c.url("/api/auth/challenge"), map[string]string{"device_id": c.Cfg.DeviceID}, &ch); err != nil {
		return "", fmt.Errorf("device auth: %w", err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(c.key, identity.ChallengeMessage(c.Cfg.DeviceID, ch.Nonce)))
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := postJSON(c.http, c.url("/api/auth/token"), map[string]string{"device_id": c.Cfg.DeviceID, "nonce": ch.Nonce, "signature": sig}, &tok); err != nil {
		return "", fmt.Errorf("device auth: %w", err)
	}
	c.token, c.expires = tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
	return c.token, nil
}

// Do performs an authenticated request.
func (c *Client) Do(method, path string, body io.Reader, out any) error {
	resp, err := c.Raw(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

// Raw performs an authenticated request and returns the response; the
// caller closes the body.
func (c *Client) Raw(method, path string, body io.Reader) (*http.Response, error) {
	tok, err := c.Token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, c.url(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return c.http.Do(req)
}

// Login pairs this machine with a server using the device-code flow.
func Login(server, name string, out io.Writer) error {
	server = strings.TrimRight(server, "/")
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		server = "https://" + server
	}
	key, err := loadOrCreateKey()
	if err != nil {
		return err
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	pub := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	var start struct {
		PairingID       string `json:"pairing_id"`
		PollSecret      string `json:"poll_secret"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if err := postJSON(hc, server+"/api/pair/start", map[string]string{"public_key": pub, "name": name, "platform": platform()}, &start); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nTo approve this device, open:\n\n    %s\n\nand check that it shows code %s and key fingerprint %s\n\nWaiting for approval…",
		start.VerificationURL, start.UserCode, identity.Fingerprint(pub))
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var poll struct {
			Status   string `json:"status"`
			DeviceID string `json:"device_id"`
		}
		if err := postJSON(hc, server+"/api/pair/poll", map[string]string{"pairing_id": start.PairingID, "poll_secret": start.PollSecret}, &poll); err != nil {
			return err
		}
		switch poll.Status {
		case "approved":
			cfg := &Config{Server: server, DeviceID: poll.DeviceID, Name: name}
			if err := cfg.save(); err != nil {
				return err
			}
			c := &Client{Cfg: cfg, key: key, http: hc}
			if _, err := c.Token(); err != nil {
				return err
			}
			fmt.Fprintf(out, " approved.\nLogged in to %s as device %q.\n", server, name)
			return nil
		case "expired":
			return errors.New("pairing expired; run login again")
		}
	}
	return errors.New("pairing timed out")
}

func platform() string {
	b, _ := os.ReadFile("/etc/os-release")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`)
		}
	}
	return "unknown"
}

// CredentialHelper implements `git credential` "get" for the server's
// /git/ URLs: username "device", password a fresh access token.
func CredentialHelper(op string, in io.Reader, out io.Writer) error {
	if op != "get" {
		return nil // store/erase: nothing is ever stored
	}
	attrs := map[string]string{}
	b, _ := io.ReadAll(in)
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			attrs[k] = v
		}
	}
	c, err := NewClient()
	if err != nil {
		return err
	}
	srv := strings.TrimPrefix(strings.TrimPrefix(c.Cfg.Server, "https://"), "http://")
	if attrs["host"] != "" && attrs["host"] != srv {
		return nil // not ours
	}
	tok, err := c.Token()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "username=device\npassword=%s\n", tok)
	return nil
}
