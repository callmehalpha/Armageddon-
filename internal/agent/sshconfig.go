package agent

import (
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	sshBlockBegin = "# BEGIN armageddon (managed by `armageddon ssh-config`)"
	sshBlockEnd   = "# END armageddon"
)

// SSHConfig writes OpenSSH Host blocks for the server's SSH endpoint
// (plan M4.4): one per workspace, authenticated with this device's key and
// pinned to the host key the server reports over the authenticated API.
// With file == "-" the blocks are printed instead of written.
func (c *Client) SSHConfig(refs []string, file string, out io.Writer) error {
	var info struct {
		Enabled bool   `json:"enabled"`
		Addr    string `json:"addr"`
		HostKey string `json:"host_key"`
	}
	if err := c.Do("GET", "/api/ssh", nil, &info); err != nil {
		return err
	}
	if !info.Enabled {
		return errors.New("the server's SSH endpoint is not enabled (ssh.enabled in server.json)")
	}
	host, port, err := net.SplitHostPort(info.Addr)
	if err != nil {
		return fmt.Errorf("server reported a bad SSH address %q", info.Addr)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(info.HostKey)); err != nil {
		return fmt.Errorf("server reported a bad host key: %w", err)
	}
	ws, err := c.Workspaces()
	if err != nil {
		return err
	}
	slugs := map[string]int{}
	for _, w := range ws {
		slugs[w.Slug]++
	}
	var pick []Workspace
	if len(refs) == 0 {
		pick = ws
	}
	for _, ref := range refs {
		w, err := c.Resolve(ref)
		if err != nil {
			return err
		}
		pick = append(pick, *w)
	}

	// OpenSSH reads the device key as a regular private key file.
	dir := filepath.Join(configDir(), "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	blk, err := ssh.MarshalPrivateKey(c.key, "armageddon device "+c.Cfg.Name)
	if err != nil {
		return err
	}
	idFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(idFile, pem.EncodeToMemory(blk), 0o600); err != nil {
		return err
	}
	khFile := filepath.Join(dir, "known_hosts")
	khHost := host
	if port != "22" {
		khHost = "[" + host + "]:" + port
	}
	if err := os.WriteFile(khFile, []byte(khHost+" "+info.HostKey+"\n"), 0o600); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString(sshBlockBegin + "\n")
	for _, w := range pick {
		user := "ws-" + w.Slug
		if slugs[w.Slug] > 1 {
			user = strings.ToLower(w.ID)
		}
		fmt.Fprintf(&b, "Host armageddon-%s\n  HostName %s\n  Port %s\n  User %s\n  IdentityFile %q\n  IdentitiesOnly yes\n  UserKnownHostsFile %q\n  StrictHostKeyChecking yes\n\n",
			w.Slug, host, port, user, idFile, khFile)
	}
	b.WriteString(sshBlockEnd + "\n")
	if file == "-" {
		_, err := io.WriteString(out, b.String())
		return err
	}
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		file = filepath.Join(home, ".ssh", "config")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	old, _ := os.ReadFile(file)
	next := replaceBlock(string(old), b.String())
	if err := os.WriteFile(file, []byte(next), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %d host(s) to %s:\n", len(pick), file)
	for _, w := range pick {
		fmt.Fprintf(out, "  ssh armageddon-%s\n", w.Slug)
	}
	return nil
}

// replaceBlock swaps the managed block in an ssh config, or prepends it
// (OpenSSH uses the first matching value, so ours goes first).
func replaceBlock(cfg, block string) string {
	i := strings.Index(cfg, sshBlockBegin)
	j := strings.Index(cfg, sshBlockEnd)
	if i >= 0 && j > i {
		end := j + len(sshBlockEnd)
		if end < len(cfg) && cfg[end] == '\n' {
			end++
		}
		return cfg[:i] + block + cfg[end:]
	}
	if cfg == "" {
		return block
	}
	return block + "\n" + cfg
}
