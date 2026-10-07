package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Docker Compose for workspaces (contract §2.5, plan M8.4).
//
// Workspaces never get the Docker socket. The helper runs Compose for them
// in three steps, so what runs with the daemon's (root) authority is exactly
// what was checked:
//
//  1. As the workspace user, `docker compose config --format json` turns the
//     workspace's Compose file into one normalised document. Every file the
//     project refers to (extends, include, env_file, .env interpolation) is
//     read with the workspace's own permissions, and this step needs no
//     daemon.
//  2. As root, NormalizeCompose checks that document against an allowlist
//     and rewrites what it must: the project name is forced to
//     arm-<workspace>, published ports are bound to 127.0.0.1.
//  3. As root, `docker compose up` runs the checked document, from a
//     root-owned empty project directory, so nothing in the workspace is read
//     with root's permissions.
//
// Refused: privileged containers, added capabilities, host
// network/pid/ipc/uts/userns/cgroup namespaces, devices, security options,
// bind mounts of any kind (a bind source inside the workspace can be swapped
// for a symlink between the check and the mount, and Docker follows it as
// root), Docker-socket mounts, image builds (the build context would be
// read as root, with the same race), external or custom-named volumes and
// networks (another workspace's), volume driver options (`o: bind` is a
// bind mount), secrets and configs read from files, lifecycle hooks, and
// every key the allowlist does not name.

// ComposeProject is the Compose project name of a workspace.
func ComposeProject(wsID string) string { return "arm-" + strings.ToLower(wsID) }

// ComposeArgs are the parameters of ComposeUp.
type ComposeArgs struct {
	// File is the Compose file, relative to the workspace's tree/. Empty:
	// Compose's own default (compose.yaml, docker-compose.yml, …).
	File string `json:"file,omitempty"`
}

// ComposeService is one service as ComposePs reports it.
type ComposeService struct {
	Service string        `json:"service"`
	Name    string        `json:"name"`
	Image   string        `json:"image,omitempty"`
	State   string        `json:"state"`
	Status  string        `json:"status,omitempty"`
	Health  string        `json:"health,omitempty"`
	Ports   []ComposePort `json:"ports,omitempty"`
}

// ComposePort is a published port.
type ComposePort struct {
	HostIP    string `json:"host_ip,omitempty"`
	Published int    `json:"published"`
	Target    int    `json:"target"`
	Protocol  string `json:"protocol,omitempty"`
}

func validComposeFile(f string) error {
	if f == "" {
		return nil
	}
	if len(f) > 255 || strings.IndexByte(f, 0) >= 0 || filepath.IsAbs(f) || filepath.Clean(f) != f ||
		f == ".." || strings.HasPrefix(f, "../") || strings.HasPrefix(f, "-") {
		return errors.New("compose file must be a clean path inside the workspace tree")
	}
	return nil
}

// Allowlists. A key not listed is refused.
var (
	composeTopKeys = map[string]bool{"name": true, "services": true, "volumes": true, "networks": true}

	composeServiceKeys = map[string]bool{
		"image": true, "command": true, "entrypoint": true, "environment": true, "ports": true, "expose": true,
		"volumes": true, "tmpfs": true, "depends_on": true, "healthcheck": true, "restart": true,
		"working_dir": true, "user": true, "labels": true, "annotations": true, "networks": true, "hostname": true,
		"domainname": true, "stop_signal": true, "stop_grace_period": true, "tty": true, "stdin_open": true,
		"init": true, "profiles": true, "shm_size": true, "extra_hosts": true, "dns": true, "dns_search": true,
		"dns_opt": true, "read_only": true, "mem_limit": true, "mem_reservation": true, "memswap_limit": true,
		"cpus": true, "cpu_shares": true, "pids_limit": true, "platform": true, "pull_policy": true,
		"links": true, "volumes_from": true, "network_mode": true, "deploy": true, "logging": true,
		"scale": true, "attach": true, "cap_drop": true, "privileged": true, "mac_address": true,
	}

	composeVolumeKeys  = map[string]bool{"name": true, "labels": true, "driver": true}
	composeNetworkKeys = map[string]bool{"name": true, "labels": true, "driver": true, "internal": true,
		"enable_ipv6": true, "ipam": true, "attachable": true}
	composeDeployKeys    = map[string]bool{"resources": true, "restart_policy": true, "replicas": true, "mode": true, "labels": true}
	composeResourcesKeys = map[string]bool{"limits": true, "reservations": true}
	composeLimitKeys     = map[string]bool{"cpus": true, "memory": true, "pids": true}
	composeMountKeys     = map[string]bool{"type": true, "source": true, "target": true, "read_only": true,
		"volume": true, "tmpfs": true, "consistency": true, "bind": true}
	composePortKeys = map[string]bool{"mode": true, "target": true, "published": true, "protocol": true,
		"host_ip": true, "app_protocol": true, "name": true}
)

// NormalizeCompose checks a `docker compose config --format json` document
// for workspace project and returns the document to run. Every violation
// is reported, not just the first.
func NormalizeCompose(raw []byte, project string) ([]byte, []string, error) {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("compose: the normalised file is not JSON: %w", err)
	}
	v := &composeCheck{project: project}
	v.check(doc)
	if len(v.errs) > 0 {
		sort.Strings(v.errs)
		return nil, nil, fmt.Errorf("compose file refused: %s", strings.Join(v.errs, "; "))
	}
	out, err := json.Marshal(doc)
	return out, v.warns, err
}

type composeCheck struct {
	project string
	errs    []string
	warns   []string
	// declared top-level volumes and networks
	volumes, networks map[string]bool
}

func (v *composeCheck) fail(format string, args ...any) {
	v.errs = append(v.errs, fmt.Sprintf(format, args...))
}

func asMap(x any) (map[string]any, bool) { m, ok := x.(map[string]any); return m, ok }

func truthy(x any) bool {
	switch t := x.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "false" && t != "0"
	case json.Number:
		return t.String() != "0"
	}
	return true
}

func (v *composeCheck) unknown(where string, m map[string]any, allowed map[string]bool) {
	for k := range m {
		if strings.HasPrefix(k, "x-") {
			delete(m, k) // extension fields are inert; drop them
			continue
		}
		if !allowed[k] {
			v.fail("%s: %q is not allowed", where, k)
		}
	}
}

// labels refuses Compose's own labels, which Compose uses to find a
// project's containers, volumes and networks: with them a workspace could
// make its resources part of another workspace's project.
func (v *composeCheck) labels(where string, x any) {
	var keys []string
	switch t := x.(type) {
	case map[string]any:
		for k := range t {
			keys = append(keys, k)
		}
	case []any:
		for _, e := range t {
			s, _ := e.(string)
			k, _, _ := strings.Cut(s, "=")
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "com.docker.compose.") {
			v.fail("%s: label %q is reserved for Compose", where, k)
		}
	}
}

func (v *composeCheck) check(doc map[string]any) {
	v.unknown("top level", doc, composeTopKeys)
	doc["name"] = v.project
	v.volumes, v.networks = map[string]bool{}, map[string]bool{}
	if vols, ok := asMap(doc["volumes"]); ok {
		for name, x := range vols {
			v.volumes[name] = true
			m, _ := asMap(x)
			if m == nil {
				m = map[string]any{}
				vols[name] = m
			}
			where := fmt.Sprintf("volume %q", name)
			if truthy(m["external"]) {
				v.fail("%s: external volumes are not allowed (they may belong to another workspace)", where)
				delete(m, "external")
			}
			v.unknown(where, m, composeVolumeKeys)
			v.labels(where, m["labels"])
			if d, _ := m["driver"].(string); d != "" && d != "local" {
				v.fail("%s: volume driver %q is not allowed (only local)", where, d)
			}
			v.ownName(where, m, name)
		}
	} else if doc["volumes"] != nil {
		v.fail("volumes: not a mapping")
	}
	if nets, ok := asMap(doc["networks"]); ok {
		for name, x := range nets {
			v.networks[name] = true
			m, _ := asMap(x)
			if m == nil {
				m = map[string]any{}
				nets[name] = m
			}
			where := fmt.Sprintf("network %q", name)
			if truthy(m["external"]) {
				v.fail("%s: external networks are not allowed (they may belong to another workspace)", where)
				delete(m, "external")
			}
			v.unknown(where, m, composeNetworkKeys)
			v.labels(where, m["labels"])
			if d, _ := m["driver"].(string); d != "" && d != "bridge" {
				v.fail("%s: network driver %q is not allowed (only bridge)", where, d)
			}
			// Custom subnets could shadow the host's routes.
			if ipam, ok := asMap(m["ipam"]); ok && len(ipam) > 0 {
				v.fail("%s: ipam settings are not allowed (Docker assigns the subnet)", where)
			}
			v.ownName(where, m, name)
		}
	} else if doc["networks"] != nil {
		v.fail("networks: not a mapping")
	}
	svcs, ok := asMap(doc["services"])
	if !ok || len(svcs) == 0 {
		v.fail("no services")
		return
	}
	for name, x := range svcs {
		m, ok := asMap(x)
		if !ok {
			v.fail("service %q: not a mapping", name)
			continue
		}
		v.service(name, m, svcs)
	}
}

// ownName keeps volume and network names inside the project: Compose names
// them <project>_<key> unless the file says otherwise.
func (v *composeCheck) ownName(where string, m map[string]any, key string) {
	want := v.project + "_" + key
	if n, ok := m["name"].(string); ok && n != want {
		v.fail("%s: custom name %q is not allowed (it could name another workspace's resource)", where, n)
	}
	m["name"] = want
}

func (v *composeCheck) service(name string, m map[string]any, svcs map[string]any) {
	where := fmt.Sprintf("service %q", name)
	explain := map[string]string{
		"build":               "image builds are not supported (the build context would be read as root); use a prebuilt image",
		"cap_add":             "adding capabilities is not allowed",
		"devices":             "host devices are not allowed",
		"security_opt":        "security options are not allowed",
		"pid":                 "sharing a PID namespace is not allowed",
		"ipc":                 "sharing an IPC namespace is not allowed",
		"uts":                 "sharing the host UTS namespace is not allowed",
		"userns_mode":         "user namespace modes are not allowed",
		"cgroup":              "cgroup namespace modes are not allowed",
		"cgroup_parent":       "cgroup_parent is not allowed",
		"container_name":      "container_name is not allowed (names are per workspace)",
		"secrets":             "secrets are not supported (they are read from files as root)",
		"configs":             "configs are not supported (they are read from files as root)",
		"env_file":            "env_file must be resolved by the workspace (use environment)",
		"external_links":      "external_links are not allowed (they reach other projects)",
		"sysctls":             "sysctls are not allowed",
		"runtime":             "container runtimes are not selectable",
		"gpus":                "GPUs are not available to workspaces",
		"device_cgroup_rules": "device cgroup rules are not allowed",
		"post_start":          "lifecycle hooks are not allowed",
		"pre_stop":            "lifecycle hooks are not allowed",
		"develop":             "compose watch is not supported",
		"provider":            "compose providers are not allowed (they run programs on the host)",
	}
	for k, why := range explain {
		if _, ok := m[k]; ok {
			v.fail("%s: %s is refused: %s", where, k, why)
			delete(m, k)
		}
	}
	v.unknown(where, m, composeServiceKeys)
	v.labels(where, m["labels"])
	if img, _ := m["image"].(string); img == "" {
		v.fail("%s: an image is required", where)
	}
	if truthy(m["privileged"]) {
		v.fail("%s: privileged containers are not allowed", where)
	}
	delete(m, "privileged")
	if nm, ok := m["network_mode"].(string); ok && nm != "" {
		svc, isSvc := strings.CutPrefix(nm, "service:")
		switch {
		case nm == "none" || nm == "bridge":
		case isSvc && svcs[svc] != nil:
		default:
			v.fail("%s: network_mode %q is not allowed (host and other containers' namespaces are refused)", where, nm)
		}
	}
	if vf, ok := m["volumes_from"].([]any); ok {
		for _, x := range vf {
			s, _ := x.(string)
			src, _, _ := strings.Cut(s, ":")
			if src == "container" || svcs[src] == nil {
				v.fail("%s: volumes_from %q must name a service of this project", where, s)
			}
		}
	}
	if links, ok := m["links"].([]any); ok {
		for _, x := range links {
			s, _ := x.(string)
			src, _, _ := strings.Cut(s, ":")
			if svcs[src] == nil {
				v.fail("%s: link %q must name a service of this project", where, s)
			}
		}
	}
	if nets, ok := asMap(m["networks"]); ok {
		for n := range nets {
			if !v.networks[n] {
				v.fail("%s: network %q is not declared in this project", where, n)
			}
		}
	}
	if lg, ok := asMap(m["logging"]); ok {
		if d, _ := lg["driver"].(string); d != "" && d != "json-file" && d != "local" {
			v.fail("%s: logging driver %q is not allowed (json-file or local)", where, d)
		}
		for k := range lg {
			if k != "driver" && k != "options" {
				v.fail("%s: logging: %q is not allowed", where, k)
			}
		}
	}
	if dp, ok := asMap(m["deploy"]); ok {
		v.unknown(where+": deploy", dp, composeDeployKeys)
		if rs, ok := asMap(dp["resources"]); ok {
			v.unknown(where+": deploy.resources", rs, composeResourcesKeys)
			for _, k := range []string{"limits", "reservations"} {
				if lm, ok := asMap(rs[k]); ok {
					v.unknown(where+": deploy.resources."+k, lm, composeLimitKeys)
				}
			}
		}
	}
	if vols, ok := m["volumes"].([]any); ok {
		for i, x := range vols {
			mt, ok := asMap(x)
			if !ok {
				v.fail("%s: volume %d: not a mapping", where, i)
				continue
			}
			v.mount(where, mt)
		}
	}
	if ports, ok := m["ports"].([]any); ok {
		for _, x := range ports {
			p, ok := asMap(x)
			if !ok {
				v.fail("%s: port: not a mapping", where)
				continue
			}
			v.unknown(where+": port", p, composePortKeys)
			if ip, _ := p["host_ip"].(string); ip != "" && ip != "127.0.0.1" {
				v.warns = append(v.warns, fmt.Sprintf("%s: port %v is published on 127.0.0.1, not %s (reach it through Armageddon)", where, p["published"], ip))
			}
			p["host_ip"] = "127.0.0.1"
		}
	}
}

func (v *composeCheck) mount(where string, mt map[string]any) {
	typ, _ := mt["type"].(string)
	src, _ := mt["source"].(string)
	target, _ := mt["target"].(string)
	desc := fmt.Sprintf("%s: mount %q", where, target)
	if strings.Contains(src, "docker.sock") || strings.Contains(target, "docker.sock") {
		v.fail("%s: the Docker socket is never available to workspaces", desc)
	}
	switch typ {
	case "volume":
		if src != "" && !v.volumes[src] {
			v.fail("%s: volume %q is not declared in this project", desc, src)
		}
		if vo, ok := asMap(mt["volume"]); ok {
			for k := range vo {
				if k != "nocopy" {
					v.fail("%s: volume option %q is not allowed", desc, k)
				}
			}
		}
	case "tmpfs":
	case "bind":
		v.fail("%s: bind mounts (%s) are not allowed; use a named volume", desc, src)
	default:
		v.fail("%s: mount type %q is not allowed", desc, typ)
	}
	for k := range mt {
		if !composeMountKeys[k] {
			v.fail("%s: %q is not allowed", desc, k)
		}
	}
}

// ---- running Compose ----

// composeRunner is the part of ComposeUp/Down/Ps shared by the helper and
// the dev client. config runs `docker compose … config` as the workspace
// user in the workspace's tree/; everything else runs as the caller.
type composeRunner struct {
	projectDir string // root-owned, empty: Compose's project directory for up
	config     func(ctx context.Context, wsID string, argv []string) ([]byte, error)

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (c *composeRunner) lock(wsID string) func() {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	l := c.locks[wsID]
	if l == nil {
		l = &sync.Mutex{}
		c.locks[wsID] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func dockerPath() (string, error) {
	p, err := resolveProgram("docker")
	if err != nil {
		return "", errors.New("Docker is not installed on the server (docker not found on the system PATH)")
	}
	return p, nil
}

// dockerEnv is the environment of root's docker invocations: the daemon
// address may come from the helper's environment, nothing else does.
func dockerEnv() []string {
	env := []string{"PATH=" + SafePath}
	for _, k := range []string{"HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func runDocker(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	docker, err := dockerPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = dockerEnv()
	cmd.Dir = "/"
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &limitedBuffer{max: 16 << 10, buf: &errb}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 2000 {
			msg = "…" + msg[len(msg)-2000:]
		}
		verb := "compose"
		for _, a := range args {
			if a == "up" || a == "down" || a == "ps" {
				verb += " " + a
			}
		}
		return nil, fmt.Errorf("docker %s: %v: %s", verb, err, msg)
	}
	return out.Bytes(), nil
}

// limitedBuffer keeps the last max bytes written (strict: refuses more
// than max bytes instead).
type limitedBuffer struct {
	max    int
	buf    *bytes.Buffer
	strict bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.strict && l.buf.Len()+len(p) > l.max {
		return 0, fmt.Errorf("output larger than %d bytes", l.max)
	}
	l.buf.Write(p)
	if l.buf.Len() > 2*l.max {
		b := append([]byte(nil), l.buf.Bytes()[l.buf.Len()-l.max:]...)
		l.buf.Reset()
		l.buf.Write(b)
	}
	return len(p), nil
}

func (c *composeRunner) up(ctx context.Context, wsID string, args ComposeArgs) (string, error) {
	if err := validComposeFile(args.File); err != nil {
		return "", err
	}
	defer c.lock(wsID)()
	project := ComposeProject(wsID)
	argv := []string{"docker", "compose", "-p", project}
	if args.File != "" {
		argv = append(argv, "-f", args.File)
	}
	argv = append(argv, "config", "--format", "json")
	raw, err := c.config(ctx, wsID, argv)
	if err != nil {
		return "", err
	}
	doc, warns, err := NormalizeCompose(raw, project)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(c.projectDir, 0o700); err != nil {
		return "", err
	}
	file := filepath.Join(c.projectDir, project+".json")
	if err := os.WriteFile(file, doc, 0o600); err != nil {
		return "", err
	}
	if _, err := runDocker(ctx, 20*time.Minute, "compose", "-p", project, "-f", file, "--project-directory", c.projectDir,
		"up", "--detach", "--remove-orphans", "--quiet-pull"); err != nil {
		return strings.Join(warns, "\n"), err
	}
	return strings.Join(warns, "\n"), nil
}

func (c *composeRunner) down(ctx context.Context, wsID string) error {
	defer c.lock(wsID)()
	project := ComposeProject(wsID)
	_, err := runDocker(ctx, 5*time.Minute, "compose", "-p", project, "down", "--remove-orphans")
	return err
}

func (c *composeRunner) ps(ctx context.Context, wsID string) ([]ComposeService, error) {
	project := ComposeProject(wsID)
	out, err := runDocker(ctx, time.Minute, "compose", "-p", project, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseComposePs(out)
}

// parseComposePs reads `docker compose ps --format json`: a JSON array in
// older Compose releases, one object per line in newer ones.
func parseComposePs(out []byte) ([]ComposeService, error) {
	type publisher struct {
		URL           string
		TargetPort    int
		PublishedPort int
		Protocol      string
	}
	type row struct {
		Service, Name, Image, State, Status, Health string
		Publishers                                  []publisher
	}
	var rows []row
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return []ComposeService{}, nil
	}
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("compose ps: %w", err)
		}
	} else {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var r row
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("compose ps: %w", err)
			}
			rows = append(rows, r)
		}
	}
	out2 := []ComposeService{}
	for _, r := range rows {
		s := ComposeService{Service: r.Service, Name: r.Name, Image: r.Image, State: r.State, Status: r.Status, Health: r.Health}
		for _, p := range r.Publishers {
			if p.PublishedPort == 0 {
				continue
			}
			s.Ports = append(s.Ports, ComposePort{HostIP: p.URL, Published: p.PublishedPort, Target: p.TargetPort, Protocol: p.Protocol})
		}
		out2 = append(out2, s)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Service < out2[j].Service })
	return out2, nil
}

// composeConfigOutput runs a prepared `compose config` command and returns
// its standard output, bounded.
func composeConfigOutput(cmd *exec.Cmd) ([]byte, error) {
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitedBuffer{max: 8 << 20, buf: &out, strict: true}, &limitedBuffer{max: 16 << 10, buf: &errb}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 2000 {
			msg = "…" + msg[len(msg)-2000:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("compose file: %s", msg)
	}
	return out.Bytes(), nil
}
