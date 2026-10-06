// Package config holds the versioned server configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	// LeaseStaleMS is T_stale: a device holding the lease with no heartbeat
	// for this long is shown as STALE (contract §3.2; default 2 min).
	LeaseStaleMS int `json:"lease_stale_ms,omitempty"`
	// HandoffTimeoutMS is T_handoff: how long a holder has to flush and
	// release before the handoff fails (contract §3.2; default 30 s).
	HandoffTimeoutMS int `json:"handoff_timeout_ms,omitempty"`
	// RunDir holds the per-workspace authority sockets (P-14). Default:
	// the directory of the helper socket (/run/armageddon), or a private
	// temp directory without a helper.
	RunDir string `json:"run_dir,omitempty"`

	CodeServer CodeServer `json:"code_server"`
	SSH        SSH        `json:"ssh"`
}

// TStale and THandoff return the lease timers with their defaults applied.
func (c *Server) TStale() time.Duration {
	if c.LeaseStaleMS > 0 {
		return time.Duration(c.LeaseStaleMS) * time.Millisecond
	}
	return 2 * time.Minute
}

func (c *Server) THandoff() time.Duration {
	if c.HandoffTimeoutMS > 0 {
		return time.Duration(c.HandoffTimeoutMS) * time.Millisecond
	}
	return 30 * time.Second
}

// CodeServer configures the browser IDE (plan M4.3).
type CodeServer struct {
	// Path to the code-server executable. Empty: look in PATH, then in the
	// data directory's components/ (`armageddon server components install`).
	Path string `json:"path,omitempty"`
	// IdleTimeoutMin stops an instance with no traffic for this long
	// (default 30).
	IdleTimeoutMin int `json:"idle_timeout_min,omitempty"`
}

// SSH configures the embedded SSH endpoint (plan M4.4). Off by default until
// the P8 spike shows VS Code Remote-SSH and JetBrains Gateway work with it.
type SSH struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen,omitempty"` // default ":2222"
	// PublicAddr is host:port as clients reach it; `armageddon ssh-config`
	// writes it. Default: the public URL's host and the listen port.
	PublicAddr string `json:"public_addr,omitempty"`
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
	return &Server{Version: Version, DataDir: dataDir, Listen: ":8080", PublicURL: "http://localhost:8080", CaptureIntervalMS: 2000,
		CodeServer: CodeServer{IdleTimeoutMin: 30}, SSH: SSH{Listen: ":2222"}}
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
	if c.CodeServer.IdleTimeoutMin <= 0 {
		c.CodeServer.IdleTimeoutMin = 30
	}
	if c.SSH.Listen == "" {
		c.SSH.Listen = ":2222"
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
