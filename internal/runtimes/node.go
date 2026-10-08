package runtimes

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// nodeProvider: package.json projects (plan M8.2). The Node version comes
// from .nvmrc, .node-version or engines.node; the package manager from the
// packageManager field or the lockfile. A version the server does not have
// is downloaded from the Node.js distribution, checked against its
// SHASUMS256.txt, into ~/.armageddon/runtimes/node/<version> (one install
// location per workspace).
type nodeProvider struct{}

func (nodeProvider) Name() string { return "node" }

type packageJSON struct {
	Name            string            `json:"name"`
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]string `json:"engines"`
	PackageManager  string            `json:"packageManager"`
}

func (pj *packageJSON) has(dep string) bool {
	_, a := pj.Dependencies[dep]
	_, b := pj.DevDependencies[dep]
	return a || b
}

func readPackageJSON(dir string) (*packageJSON, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var pj packageJSON
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	return &pj, nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// nodeFrameworks maps a dependency to (framework, default dev port).
var nodeFrameworks = []struct {
	dep, name string
	port      int
}{
	{"next", "next", 3000}, {"nuxt", "nuxt", 3000}, {"@remix-run/dev", "remix", 3000},
	{"@sveltejs/kit", "sveltekit", 5173}, {"astro", "astro", 4321}, {"vite", "vite", 5173},
	{"react-scripts", "create-react-app", 3000}, {"express", "express", 3000},
}

func (nodeProvider) Detect(dir string) (*Detection, error) {
	if !exists(dir, "package.json") {
		return nil, nil
	}
	pj, err := readPackageJSON(dir)
	if err != nil {
		return nil, err
	}
	d := &Detection{Evidence: []string{"package.json"}, Scripts: pj.Scripts}
	for _, f := range []string{".nvmrc", ".node-version"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if v := firstLine(string(b)); v != "" {
				d.Version, d.VersionFrom = v, f
				d.Evidence = append(d.Evidence, f)
				break
			}
		}
	}
	if d.Version == "" && pj.Engines["node"] != "" {
		d.Version, d.VersionFrom = pj.Engines["node"], "package.json engines.node"
	}
	// Package manager: the packageManager field (corepack), else the lockfile.
	if name, ver, ok := strings.Cut(pj.PackageManager, "@"); ok || name != "" {
		d.PackageManager, d.PMVersion = name, strings.SplitN(ver, "+", 2)[0]
		d.Evidence = append(d.Evidence, "package.json packageManager")
	} else {
		for _, lf := range []struct{ file, pm string }{{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"},
			{"bun.lockb", "bun"}, {"bun.lock", "bun"}, {"package-lock.json", "npm"}, {"npm-shrinkwrap.json", "npm"}} {
			if exists(dir, lf.file) {
				d.PackageManager = lf.pm
				d.Evidence = append(d.Evidence, lf.file)
				break
			}
		}
	}
	if d.PackageManager == "" {
		d.PackageManager = "npm"
	}
	for _, f := range nodeFrameworks {
		if pj.has(f.dep) {
			d.Framework = f.name
			break
		}
	}
	return d, nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

func (nodeProvider) Plan(ctx context.Context, d *Detection, h *Host) (*Plan, error) {
	pj, err := readPackageJSON(d.Dir)
	if err != nil {
		return nil, err
	}
	p := &Plan{Detection: *d, Toolchain: Toolchain{Name: "node"}}
	if err := resolveNode(ctx, p, h); err != nil {
		return nil, err
	}
	pm := d.PackageManager
	if pm == "bun" {
		p.Notes = append(p.Notes, "bun is not supported in v0.1; using npm (the bun lockfile is ignored)")
		pm = "npm"
		p.PackageManager = "npm"
	}
	// pm is the argv prefix that runs the package manager: npm directly,
	// pnpm and yarn through corepack (bundled with Node up to 24) or npx.
	run := pmArgv(pm, d.PMVersion, h)
	switch pm {
	case "npm":
		if exists(d.Dir, "package-lock.json") || exists(d.Dir, "npm-shrinkwrap.json") {
			p.Install = [][]string{{"npm", "ci"}}
		} else {
			p.Install = [][]string{{"npm", "install"}}
		}
	case "pnpm":
		args := []string{"install"}
		if exists(d.Dir, "pnpm-lock.yaml") {
			args = append(args, "--frozen-lockfile")
		}
		p.Install = [][]string{append(append([]string(nil), run...), args...)}
	case "yarn":
		p.Install = [][]string{append(append([]string(nil), run...), "install")}
	default:
		return nil, fmt.Errorf("package manager %q is not supported (npm, pnpm and yarn are)", pm)
	}
	p.Port = 3000
	for _, f := range nodeFrameworks {
		if f.name == d.Framework {
			p.Port = f.port
		}
	}
	switch {
	case pj.Scripts["dev"] != "":
		p.Start = append(append([]string(nil), run...), "run", "dev")
	case pj.Scripts["start"] != "":
		p.Start = append(append([]string(nil), run...), "run", "start")
	case pj.Main != "" || exists(d.Dir, "index.js") || exists(d.Dir, "server.js"):
		main := pj.Main
		if main == "" {
			main = "index.js"
			if exists(d.Dir, "server.js") {
				main = "server.js"
			}
		}
		p.Start = []string{"node", main}
	default:
		p.Notes = append(p.Notes, "no dev or start script: add one to package.json to start the app with `armageddon runtime start`")
	}
	p.Env = []string{fmt.Sprintf("PORT=%d", p.Port)}
	return p, nil
}

// pmArgv is how to invoke a package manager: corepack when the toolchain
// has it (it honours the packageManager version), else npx.
func pmArgv(pm, ver string, h *Host) []string {
	if pm == "npm" {
		return []string{"npm"}
	}
	if _, err := h.LookPath("corepack"); err == nil {
		return []string{"corepack", pm}
	}
	spec := pm
	if ver != "" {
		spec += "@" + ver
	}
	return []string{"npx", "--yes", spec}
}

// resolveNode picks the Node toolchain: one already installed in the
// workspace, the server's own (on the system PATH), or a release to
// download.
func resolveNode(ctx context.Context, p *Plan, h *Host) error {
	spec := strings.TrimSpace(p.Version)
	// Already installed in the workspace.
	installed := installedNode(h)
	sys, sysVer := systemNode(ctx, h)
	switch strings.ToLower(spec) {
	case "", "node", "stable", "latest", "current", "lts/*", "lts":
		if spec == "" {
			if len(installed) > 0 {
				v := installed[len(installed)-1]
				p.Toolchain = Toolchain{Name: "node", Version: v.String(), Source: "workspace", Path: nodeDir(h, v)}
				return nil
			}
			if sys != "" {
				p.Toolchain = Toolchain{Name: "node", Version: sysVer.String(), Source: "system", Path: sys}
				return nil
			}
			spec = "lts/*"
		}
	}
	if !strings.HasPrefix(strings.ToLower(spec), "lts") && !isAlias(spec) {
		r, err := ParseRange(spec)
		if err != nil {
			return err
		}
		for i := len(installed) - 1; i >= 0; i-- {
			if r.Match(installed[i]) {
				p.Toolchain = Toolchain{Name: "node", Version: installed[i].String(), Source: "workspace", Path: nodeDir(h, installed[i])}
				return nil
			}
		}
		if sys != "" && r.Match(sysVer) {
			p.Toolchain = Toolchain{Name: "node", Version: sysVer.String(), Source: "system", Path: sys}
			return nil
		}
	}
	v, err := resolveNodeRelease(ctx, h, spec)
	if err != nil {
		return err
	}
	for _, iv := range installed {
		if iv == v {
			p.Toolchain = Toolchain{Name: "node", Version: v.String(), Source: "workspace", Path: nodeDir(h, v)}
			return nil
		}
	}
	p.Toolchain = Toolchain{Name: "node", Version: v.String(), Source: "install", Path: nodeDir(h, v)}
	return nil
}

func isAlias(spec string) bool {
	switch strings.ToLower(spec) {
	case "node", "stable", "latest", "current":
		return true
	}
	return false
}

func nodeDir(h *Host, v Version) string {
	return filepath.Join(h.Base(), "runtimes", "node", v.String())
}

// installedNode lists the workspace's Node installs, oldest first.
func installedNode(h *Host) []Version {
	ents, _ := os.ReadDir(filepath.Join(h.Base(), "runtimes", "node"))
	var out []Version
	for _, e := range ents {
		if v, ok := ParseVersion(e.Name()); ok && e.IsDir() && exists(filepath.Join(h.Base(), "runtimes", "node", e.Name()), "bin/node") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// systemNode is the server's node, outside the workspace's own bin
// directory.
func systemNode(ctx context.Context, h *Host) (string, Version) {
	for _, dir := range filepath.SplitList(SystemPath) {
		p := filepath.Join(dir, "node")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out, err := (&Host{Home: h.Home}).Output(ctx, p, "--version")
			if v, ok := ParseVersion(out); err == nil && ok {
				return p, v
			}
		}
	}
	return "", Version{}
}

type nodeRelease struct {
	Version string `json:"version"`
	LTS     any    `json:"lts"` // false or the codename
}

// resolveNodeRelease picks the newest release matching spec from the
// distribution index.
func resolveNodeRelease(ctx context.Context, h *Host, spec string) (Version, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", h.nodeMirror()+"/index.json", nil)
	resp, err := h.client().Do(req)
	if err != nil {
		return Version{}, fmt.Errorf("Node.js release index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Version{}, fmt.Errorf("Node.js release index: %s", resp.Status)
	}
	var rels []nodeRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&rels); err != nil {
		return Version{}, fmt.Errorf("Node.js release index: %w", err)
	}
	lower := strings.ToLower(spec)
	var match func(v Version, r nodeRelease) bool
	switch {
	case lower == "lts/*" || lower == "lts":
		match = func(_ Version, r nodeRelease) bool { _, ok := r.LTS.(string); return ok }
	case strings.HasPrefix(lower, "lts/"):
		name := strings.TrimPrefix(lower, "lts/")
		match = func(_ Version, r nodeRelease) bool { s, ok := r.LTS.(string); return ok && strings.EqualFold(s, name) }
	case isAlias(spec):
		match = func(Version, nodeRelease) bool { return true }
	default:
		rng, err := ParseRange(spec)
		if err != nil {
			return Version{}, err
		}
		match = func(v Version, _ nodeRelease) bool { return rng.Match(v) }
	}
	var best Version
	found := false
	for _, r := range rels {
		v, ok := ParseVersion(r.Version)
		if !ok || strings.Contains(r.Version, "-") {
			continue
		}
		if match(v, r) && (!found || best.Less(v)) {
			best, found = v, true
		}
	}
	if !found {
		return Version{}, fmt.Errorf("no Node.js release matches %q", spec)
	}
	return best, nil
}

func nodePlatform(h *Host) (string, error) {
	osName := map[string]string{"linux": "linux", "darwin": "darwin"}[h.OS]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "ppc64le": "ppc64le", "s390x": "s390x"}[h.Arch]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("no Node.js builds for %s/%s", h.OS, h.Arch)
	}
	return osName + "-" + arch, nil
}

func (nodeProvider) Install(ctx context.Context, p *Plan, h *Host, log io.Writer) error {
	if p.Toolchain.Source == "install" {
		v, _ := ParseVersion(p.Toolchain.Version)
		if err := installNode(ctx, h, v, log); err != nil {
			return err
		}
		p.Toolchain.Source = "workspace"
	}
	if p.Toolchain.Source == "workspace" {
		// The workspace's own install comes first on PATH.
		for _, bin := range []string{"node", "npm", "npx", "corepack"} {
			target := filepath.Join(p.Toolchain.Path, "bin", bin)
			if _, err := os.Lstat(target); err == nil {
				if err := h.link(bin, target); err != nil {
					return err
				}
			}
		}
		if err := h.EnsureProfile(); err != nil {
			return fmt.Errorf("add the runtime to the shell profile: %w", err)
		}
	}
	fmt.Fprintf(log, "Node.js %s (%s)\n", p.Toolchain.Version, p.Toolchain.Source)
	for _, n := range p.Notes {
		fmt.Fprintln(log, "note:", n)
	}
	// corepack may only now be on PATH: re-plan the package-manager argv.
	if p.PackageManager != "npm" {
		run := pmArgv(p.PackageManager, p.PMVersion, h)
		for i, step := range p.Install {
			for j, a := range step {
				if a == "install" {
					p.Install[i] = append(append([]string(nil), run...), step[j:]...)
					break
				}
			}
		}
	}
	for _, step := range p.Install {
		if err := h.Run(ctx, p.Dir, log, step...); err != nil {
			return err
		}
	}
	return nil
}

// installNode downloads a release tarball, verifies it against the
// release's SHASUMS256.txt and unpacks it into the workspace's install
// location.
func installNode(ctx context.Context, h *Host, v Version, log io.Writer) error {
	plat, err := nodePlatform(h)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("node-v%s-%s.tar.gz", v, plat)
	base := fmt.Sprintf("%s/v%s/", h.nodeMirror(), v)
	fmt.Fprintf(log, "Downloading Node.js %s (%s)…\n", v, plat)
	sums, err := fetch(ctx, h, base+"SHASUMS256.txt", 1<<20)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("%s is not listed in SHASUMS256.txt", name)
	}
	dest := nodeDir(h, v)
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+name, nil)
	resp, err := h.client().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: %s", name, resp.Status)
	}
	// Hash while unpacking, and only move the result into place if the
	// hash matches.
	sum := sha256.New()
	if err := untarGz(io.TeeReader(resp.Body, sum), tmp); err != nil {
		return fmt.Errorf("unpack %s: %w", name, err)
	}
	io.Copy(sum, resp.Body) // trailing bytes after the archive
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("%s: checksum mismatch (got %s, want %s): refused", name, got, want)
	}
	if !exists(tmp, "bin/node") {
		return fmt.Errorf("%s has no bin/node", name)
	}
	os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	fmt.Fprintf(log, "Installed Node.js %s in %s\n", v, dest)
	return nil
}

func fetch(ctx context.Context, h *Host, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// untarGz unpacks a release tarball into dest, dropping its top-level
// directory. Entries that would land outside dest are refused.
func untarGz(r io.Reader, dest string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hd.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in archive", hd.Name)
		}
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		} else {
			continue // the top-level directory itself
		}
		if name == "" || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in archive", hd.Name)
		}
		p := filepath.Join(dest, name)
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hd.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			t := hd.Linkname
			if filepath.IsAbs(t) || !within(dest, filepath.Join(filepath.Dir(p), t)) {
				return fmt.Errorf("unsafe link %q -> %q in archive", hd.Name, t)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Symlink(t, p); err != nil {
				return err
			}
		}
	}
}

func within(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}
