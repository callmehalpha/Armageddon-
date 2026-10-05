// Package config holds the versioned server configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const Version = 1

type Server struct {
	Version   int    `json:"version"`
	DataDir   string `json:"data_dir"`
	Listen    string `json:"listen"`     // e.g. ":8443" or "127.0.0.1:8080"
	PublicURL string `json:"public_url"` // how clients reach the server, e.g. "https://dev.example.com"
	TLSCert   string `json:"tls_cert,omitempty"`
	TLSKey    string `json:"tls_key,omitempty"`
	// TLSMode is "" (plain HTTP, or TLSCert/TLSKey when set), "files"
	// (operator-supplied certificate), "self-signed" (IP-only mode, generated
	// by `server init --ip-only`) or "acme" (certmagic; contract §9.2).
	TLSMode string `json:"tls_mode,omitempty"`
	// Domain is the ACME domain name.
	Domain    string `json:"domain,omitempty"`
	ACMEEmail string `json:"acme_email,omitempty"`
	// ACMECA is the ACME directory URL; empty means Let's Encrypt production.
	ACMECA string `json:"acme_ca,omitempty"`
	// HTTPListen serves ACME HTTP-01 challenges and redirects to HTTPS
	// (acme mode only). Default ":80".
	HTTPListen string `json:"http_listen,omitempty"`
	// CaptureIntervalMS is how often the server seat is checked for changes
	// (polling stands in for the watcher in the MVP).
	CaptureIntervalMS int `json:"capture_interval_ms"`
}

func DefaultDataDir() string {
	if os.Geteuid() == 0 {
		return "/var/lib/armageddon"
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".local", "share", "armageddon-server")
	}
	return "armageddon-data"
}

func Default(dataDir string) *Server {
	return &Server{Version: Version, DataDir: dataDir, Listen: ":8080", PublicURL: "http://localhost:8080", CaptureIntervalMS: 2000}
}

func Path(dataDir string) string { return filepath.Join(dataDir, "server.json") }

func Load(dataDir string) (*Server, error) {
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return nil, err
	}
	c := Default(dataDir)
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(dataDir), err)
	}
	if c.Version > Version {
		return nil, fmt.Errorf("%s: config version %d is newer than this binary supports (%d)", Path(dataDir), c.Version, Version)
	}
	// Future: migrate older versions here, keeping a .bak (plan M1.3).
	c.Version = Version
	c.DataDir = dataDir
	if c.CaptureIntervalMS <= 0 {
		c.CaptureIntervalMS = 2000
	}
	return c, nil
}

func (c *Server) Save() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := Path(c.DataDir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(c.DataDir))
}
