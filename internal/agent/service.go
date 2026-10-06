package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
)

// RunAgent is `armageddon agent run`: one long-running process per user
// that keeps every registered replica following or writing (plan M6.1).
// Replicas cloned later are picked up within seconds. A replica already
// held by a foreground `armageddon follow` is skipped until it is free.
func (c *Client) RunAgent(ctx context.Context, out io.Writer) error {
	fmt.Fprintf(out, "%s armageddon agent: managing replicas under %s\n", ts(), filepath.Join(dataDir(), "replicas"))
	var mu sync.Mutex
	running := map[string]bool{}
	var wg sync.WaitGroup
	scan := func() {
		ents, _ := os.ReadDir(filepath.Join(dataDir(), "replicas"))
		for _, e := range ents {
			id := e.Name()
			mu.Lock()
			busy := running[id]
			mu.Unlock()
			if busy {
				continue
			}
			st, err := loadState(id)
			if err != nil {
				continue
			}
			if _, err := os.Stat(st.Path); err != nil {
				continue // replica directory deleted
			}
			lk, err := lockReplica(id)
			if err != nil {
				continue // a foreground follow has it
			}
			st.normalize()
			mu.Lock()
			running[id] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer lk.release()
				w := &prefixWriter{w: out, prefix: "[" + filepath.Base(st.Path) + "] "}
				r := &replica{c: c, st: st, sh: shadowFor(id, st.Path), gitDir: gitshadow.GitDirOf(st.Path), out: w}
				if err := r.run(ctx); err != nil {
					fmt.Fprintf(w, "%s stopped: %v\n", ts(), err)
				}
				mu.Lock()
				delete(running, id)
				mu.Unlock()
			}()
		}
	}
	scan()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-t.C:
			scan()
		}
	}
}

// prefixWriter prefixes each line with the replica's name.
type prefixWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range strings.SplitAfter(string(b), "\n") {
		if l != "" {
			io.WriteString(p.w, p.prefix+l)
		}
	}
	return len(b), nil
}

// ---- agent install (plan M6.1): launchd on macOS, systemd --user on Linux ----

// ServiceFile is a generated per-user service definition.
type ServiceFile struct {
	Path, Content, Enable string
}

// ServiceFor returns the service definition that runs `bin agent run` for
// goos. env carries ARMAGEDDON_CONFIG_DIR / ARMAGEDDON_DATA_DIR when set.
func ServiceFor(goos, home, bin string, env map[string]string) (*ServiceFile, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch goos {
	case "linux":
		var b strings.Builder
		b.WriteString("[Unit]\nDescription=Armageddon agent (replica follow and write)\nAfter=network-online.target\n\n[Service]\n")
		fmt.Fprintf(&b, "ExecStart=%s agent run\n", systemdQuote(bin))
		for _, k := range keys {
			fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(k+"="+env[k]))
		}
		b.WriteString("Restart=always\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
		return &ServiceFile{Path: filepath.Join(home, ".config", "systemd", "user", "armageddon-agent.service"), Content: b.String(),
			Enable: "systemctl --user daemon-reload && systemctl --user enable --now armageddon-agent"}, nil
	case "darwin":
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.armageddon.agent</string>
  <key>ProgramArguments</key>
  <array><string>` + xmlEscape(bin) + `</string><string>agent</string><string>run</string></array>
`)
		if len(keys) > 0 {
			b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
			for _, k := range keys {
				fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", xmlEscape(k), xmlEscape(env[k]))
			}
			b.WriteString("  </dict>\n")
		}
		logp := filepath.Join(home, "Library", "Logs", "armageddon-agent.log")
		b.WriteString(`  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + xmlEscape(logp) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(logp) + `</string>
</dict>
</plist>
`)
		p := filepath.Join(home, "Library", "LaunchAgents", "dev.armageddon.agent.plist")
		return &ServiceFile{Path: p, Content: b.String(), Enable: "launchctl bootstrap gui/$(id -u) " + p}, nil
	}
	return nil, fmt.Errorf("agent install: unsupported OS %q (run `armageddon agent run` yourself)", goos)
}

// InstallService writes the service file for this machine.
func InstallService(out io.Writer) error {
	if _, err := LoadConfig(); err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	bin, _ = filepath.EvalSymlinks(bin)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	env := map[string]string{}
	for _, k := range []string{"ARMAGEDDON_CONFIG_DIR", "ARMAGEDDON_DATA_DIR"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	sf, err := ServiceFor(runtime.GOOS, home, bin, env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sf.Path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(sf.Path, []byte(sf.Content), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s\nStart it now and at every login with:\n\n    %s\n", sf.Path, sf.Enable)
	return nil
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
