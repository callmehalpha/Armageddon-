// Package runtimes is the runtime provider layer of plan M8 (original plan
// §45, server side only): it finds out what a workspace needs to run (Node,
// PHP), installs the toolchain and the dependencies, and says how to start
// the dev server.
//
// All of it runs as the workspace user, never as the server. The server
// starts `armageddon hook runtime plan|install|exec <tree>` through the
// helper (SpawnInWorkspace, kind runtime-command), so reading tree/,
// downloading toolchains and running package managers happen with the
// workspace's own privileges and PATH (contract §2.5). The server only
// orchestrates: it keeps the processes, their logs and their state.
//
// # The provider interface
//
// A Provider covers one ecosystem. Providers are registered statically in
// v0.1 (Providers); there is no plugin loading.
//
//   - Detect reads the working tree and says whether the provider applies,
//     with the evidence (package.json, .nvmrc, composer.json, artisan, …).
//   - Plan turns a detection into concrete steps for this host: which
//     toolchain version (already present or to be installed), the
//     dependency install commands, the start command and its port.
//   - Install makes the plan true: toolchain into the workspace's install
//     location (~/.armageddon/runtimes, on PATH through ~/.armageddon/bin),
//     then dependencies in the tree.
//
// Start, stop, health and logs are the same for every provider, so they
// are not provider methods: the plan names the start command, port and
// health path, and the server's process manager starts the command (as
// `hook runtime exec`), stops it, probes the port and keeps the logs.
package runtimes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var execve = syscall.Exec

// Provider is one runtime ecosystem (see the package documentation).
type Provider interface {
	Name() string
	// Detect inspects the tree at dir. It returns nil, nil when the
	// provider does not apply.
	Detect(dir string) (*Detection, error)
	// Plan resolves a detection against the host.
	Plan(ctx context.Context, d *Detection, h *Host) (*Plan, error)
	// Install provides the toolchain and installs dependencies, logging
	// progress to log. It is idempotent.
	Install(ctx context.Context, p *Plan, h *Host, log io.Writer) error
}

// Providers are the providers of v0.1, in detection order. PHP comes
// first: a Laravel or Symfony app also has a package.json (for its asset
// build), but its dev server is PHP's.
var Providers = []Provider{phpProvider{}, nodeProvider{}}

// Detection is what a provider found in the tree.
type Detection struct {
	Provider  string `json:"provider"`
	Framework string `json:"framework,omitempty"` // next, vite, laravel, …
	// Version is the requested toolchain version, as written, and
	// VersionFrom where it was found.
	Version        string            `json:"version,omitempty"`
	VersionFrom    string            `json:"version_from,omitempty"`
	PackageManager string            `json:"package_manager,omitempty"` // npm, pnpm, yarn, composer
	PMVersion      string            `json:"package_manager_version,omitempty"`
	Scripts        map[string]string `json:"scripts,omitempty"`
	Evidence       []string          `json:"evidence"`
	Dir            string            `json:"dir"`
}

// Plan is a detection resolved for this host.
type Plan struct {
	Detection
	Toolchain Toolchain `json:"toolchain"`
	// Install are the dependency install commands, run in the tree after
	// the toolchain is in place.
	Install [][]string `json:"install,omitempty"`
	// Start is the dev server command, run in the tree with Env added.
	Start []string `json:"start,omitempty"`
	Env   []string `json:"env,omitempty"`
	// Port is where the dev server listens (127.0.0.1 or all interfaces),
	// HealthPath what the health probe requests there.
	Port       int    `json:"port,omitempty"`
	HealthPath string `json:"health_path,omitempty"`
	// Notes are warnings for the user (version mismatches, unsupported
	// package managers).
	Notes []string `json:"notes,omitempty"`
}

// Toolchain is the interpreter a plan uses.
type Toolchain struct {
	Name    string `json:"name"`    // node, php
	Version string `json:"version"` // resolved version, or the request when missing
	// Source is "system" (on the server's PATH), "workspace" (installed
	// under the workspace's home), "install" (to be downloaded by Install)
	// or "missing" (cannot be provided; Notes say what to do).
	Source string `json:"source"`
	Path   string `json:"path,omitempty"`
}

// Host is the environment providers run in: the workspace user's home and
// the download locations.
type Host struct {
	Home string
	// NodeMirror is the base of the Node.js distribution
	// (https://nodejs.org/dist by default).
	NodeMirror string
	// ComposerURL is where composer.phar is downloaded from when the server
	// has no composer; ComposerURL+".sha256" carries its checksum.
	ComposerURL string
	HTTP        *http.Client
	OS, Arch    string // GOOS/GOARCH values
}

// DefaultNodeMirror and DefaultComposerURL are the upstream locations.
const (
	DefaultNodeMirror  = "https://nodejs.org/dist"
	DefaultComposerURL = "https://getcomposer.org/download/latest-stable/composer.phar"
)

// SystemPath is the PATH the helper gives workspace processes (helper.SafePath).
const SystemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// HostFromEnv builds the host of the current process: $HOME, and the
// download locations from ARMAGEDDON_NODE_MIRROR and ARMAGEDDON_COMPOSER_URL.
func HostFromEnv() *Host {
	h := &Host{Home: os.Getenv("HOME"), NodeMirror: os.Getenv("ARMAGEDDON_NODE_MIRROR"),
		ComposerURL: os.Getenv("ARMAGEDDON_COMPOSER_URL"), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if h.Home == "" {
		h.Home, _ = os.UserHomeDir()
	}
	return h
}

func (h *Host) client() *http.Client {
	if h.HTTP != nil {
		return h.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (h *Host) nodeMirror() string {
	if h.NodeMirror != "" {
		return strings.TrimRight(h.NodeMirror, "/")
	}
	return DefaultNodeMirror
}

func (h *Host) composerURL() string {
	if h.ComposerURL != "" {
		return h.ComposerURL
	}
	return DefaultComposerURL
}

// Base is the workspace's runtime directory, ~/.armageddon.
func (h *Host) Base() string { return filepath.Join(h.Home, ".armageddon") }

// BinDir holds links to the workspace's installed toolchains. It comes
// first on the PATH of runtime processes and, through the profile block,
// of terminals.
func (h *Host) BinDir() string { return filepath.Join(h.Base(), "bin") }

// Path is the PATH runtime processes use.
func (h *Host) Path() string { return h.BinDir() + ":" + SystemPath }

// Environ is the process environment with PATH replaced by Path and HOME
// by Home.
func (h *Host) Environ(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	return append(append(env, "PATH="+h.Path(), "HOME="+h.Home), extra...)
}

// LookPath finds a program on Path.
func (h *Host) LookPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(h.Path()) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found", name)
}

// Output runs a program on Path and returns its trimmed standard output.
func (h *Host) Output(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prog, err := h.LookPath(argv[0])
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, prog, argv[1:]...)
	cmd.Env = h.Environ()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Run runs argv in dir on Path, with its output going to log.
func (h *Host) Run(ctx context.Context, dir string, log io.Writer, argv ...string) error {
	prog, err := h.LookPath(argv[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "$ %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, prog, argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, h.Environ(), log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// Detect runs every provider and returns the first match.
func Detect(dir string) (Provider, *Detection, error) {
	for _, p := range Providers {
		d, err := p.Detect(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p.Name(), err)
		}
		if d != nil {
			d.Provider, d.Dir = p.Name(), dir
			return p, d, nil
		}
	}
	return nil, nil, ErrNothingDetected
}

// ErrNothingDetected: no provider recognised the tree.
var ErrNothingDetected = errors.New("no runtime detected: Armageddon v0.1 recognises Node.js (package.json) and PHP (composer.json, index.php)")

// PlanFor detects and plans in one step.
func PlanFor(ctx context.Context, dir string, h *Host) (Provider, *Plan, error) {
	p, d, err := Detect(dir)
	if err != nil {
		return nil, nil, err
	}
	plan, err := p.Plan(ctx, d, h)
	if err != nil {
		return nil, nil, err
	}
	if plan.HealthPath == "" {
		plan.HealthPath = "/"
	}
	return p, plan, nil
}

// ---- the workspace's PATH in terminals ----

const (
	profileBegin = "# >>> armageddon runtimes >>>"
	profileEnd   = "# <<< armageddon runtimes <<<"
)

// profileBlock puts ~/.armageddon/bin first on PATH. Login shells reset
// PATH in /etc/profile, so terminals need it in the user's own profile.
const profileBlock = profileBegin + `
case ":$PATH:" in
  *":$HOME/.armageddon/bin:"*) ;;
  *) PATH="$HOME/.armageddon/bin:$PATH"; export PATH ;;
esac
` + profileEnd + "\n"

// EnsureProfile adds the PATH block to the login profile bash reads
// (~/.bash_profile if present, else ~/.profile) and to ~/.bashrc, once.
func (h *Host) EnsureProfile() error {
	login := filepath.Join(h.Home, ".profile")
	if _, err := os.Stat(filepath.Join(h.Home, ".bash_profile")); err == nil {
		login = filepath.Join(h.Home, ".bash_profile")
	}
	for _, p := range []string{login, filepath.Join(h.Home, ".bashrc")} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if strings.Contains(string(b), profileBegin) {
			continue
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		prefix := ""
		if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
			prefix = "\n"
		}
		_, err = io.WriteString(f, prefix+profileBlock)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// link points BinDir/name at target, replacing an older link.
func (h *Host) link(name, target string) error {
	if err := os.MkdirAll(h.BinDir(), 0o755); err != nil {
		return err
	}
	p := filepath.Join(h.BinDir(), name)
	tmp := p + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ---- `armageddon hook runtime …` ----

// Main is `armageddon hook runtime plan|install|exec [--port N] DIR`, run
// by the server as the workspace user.
//
//   - plan prints the plan as JSON.
//   - install installs the toolchain and dependencies, logging to stdout.
//   - exec replaces itself with the start command (the dev process).
func Main(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: armageddon hook runtime plan|install|exec [--port N] DIR")
		return 2
	}
	verb, port := args[0], 0
	rest := args[1:]
	if len(rest) == 3 && rest[0] == "--port" {
		fmt.Sscanf(rest[1], "%d", &port)
		rest = rest[2:]
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: armageddon hook runtime plan|install|exec [--port N] DIR")
		return 2
	}
	dir := rest[0]
	ctx := context.Background()
	h := HostFromEnv()
	prov, plan, err := PlanFor(ctx, dir, h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "armageddon:", err)
		return 1
	}
	if port > 0 {
		plan.SetPort(port)
	}
	switch verb {
	case "plan":
		json.NewEncoder(os.Stdout).Encode(plan)
		return 0
	case "install":
		if err := prov.Install(ctx, plan, h, os.Stdout); err != nil {
			fmt.Fprintln(os.Stdout, "armageddon: install failed:", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "armageddon: runtime ready.")
		return 0
	case "exec":
		return execStart(dir, plan, h)
	}
	fmt.Fprintf(os.Stderr, "unknown runtime command %q\n", verb)
	return 2
}

// SetPort moves the plan to another port.
func (p *Plan) SetPort(port int) {
	if p.Port == port {
		return
	}
	for i, kv := range p.Env {
		if strings.HasPrefix(kv, "PORT=") {
			p.Env[i] = fmt.Sprintf("PORT=%d", port)
		}
	}
	old := fmt.Sprint(p.Port)
	for i, a := range p.Start {
		if a == old || strings.HasSuffix(a, ":"+old) || strings.HasSuffix(a, "="+old) {
			p.Start[i] = strings.TrimSuffix(a, old) + fmt.Sprint(port)
		}
	}
	p.Port = port
}

func execStart(dir string, p *Plan, h *Host) int {
	if len(p.Start) == 0 {
		fmt.Fprintln(os.Stderr, "armageddon: nothing to start: add a \"dev\" or \"start\" script")
		return 1
	}
	if p.Toolchain.Source == "install" || p.Toolchain.Source == "missing" {
		fmt.Fprintf(os.Stderr, "armageddon: %s %s is not installed yet: run `armageddon runtime install`\n", p.Toolchain.Name, p.Toolchain.Version)
		return 1
	}
	prog, err := h.LookPath(p.Start[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "armageddon:", err)
		return 127
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, "armageddon:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "$ %s\n", strings.Join(p.Start, " "))
	err = execve(prog, p.Start, h.Environ(p.Env...))
	fmt.Fprintln(os.Stderr, "armageddon: exec:", err)
	return 127
}
