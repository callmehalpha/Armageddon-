// Package compose is the E8 Compose-file validator: the parser only, per the
// contract §2.5 ComposeUp note. It rejects anything that would break the
// workspace sandbox — privileged, cap_add, host network/pid/ipc, devices,
// bind mounts outside the workspace directory, and any Docker-socket mount.
//
// The policy is allow-by-default for ordinary fields but deny-by-rule for the
// dangerous ones, and it fails closed: an unparseable file is rejected.
package compose

import (
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type service struct {
	Image       string   `yaml:"image"`
	Privileged  bool     `yaml:"privileged"`
	CapAdd      []string `yaml:"cap_add"`
	NetworkMode string   `yaml:"network_mode"`
	Pid         string   `yaml:"pid"`
	Ipc         string   `yaml:"ipc"`
	Devices     []string `yaml:"devices"`
	// Volumes may be short strings ("src:dst:mode") or long-form mappings.
	Volumes []yaml.Node `yaml:"volumes"`
}

type file struct {
	Services map[string]service `yaml:"services"`
}

// Validate parses the Compose YAML and returns every violation found. The
// workspace directory is the only path bind mounts may originate from.
func Validate(data []byte, workspaceDir string) []string {
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return []string{"unparseable compose file (rejected): " + err.Error()}
	}
	wd, _ := filepath.Abs(workspaceDir)
	var errs []string
	add := func(svc, msg string) { errs = append(errs, fmt.Sprintf("service %q: %s", svc, msg)) }

	for name, s := range f.Services {
		if s.Privileged {
			add(name, "privileged: true is forbidden")
		}
		if len(s.CapAdd) > 0 {
			add(name, "cap_add is forbidden: "+strings.Join(s.CapAdd, ","))
		}
		if isHost(s.NetworkMode) {
			add(name, "network_mode: "+s.NetworkMode+" is forbidden")
		}
		if isHost(s.Pid) {
			add(name, "pid: "+s.Pid+" is forbidden")
		}
		if isHost(s.Ipc) {
			add(name, "ipc: "+s.Ipc+" is forbidden")
		}
		if len(s.Devices) > 0 {
			add(name, "devices are forbidden: "+strings.Join(s.Devices, ","))
		}
		for _, v := range s.Volumes {
			if msg := checkVolume(v, wd); msg != "" {
				add(name, msg)
			}
		}
	}
	return errs
}

func isHost(mode string) bool {
	m := strings.ToLower(strings.TrimSpace(mode))
	return m == "host" || strings.HasPrefix(m, "host:") || m == "container:host"
}

// dockerSocket paths are rejected wherever they appear as a source.
var dockerSockets = []string{"/var/run/docker.sock", "/run/docker.sock", "docker.sock", "/var/run/docker.sock", "/run/containerd"}

func checkVolume(n yaml.Node, workspaceDir string) string {
	var src, typ string
	switch n.Kind {
	case yaml.ScalarNode:
		// Short form "src:dst[:mode]"; a leading named volume (no slash) is fine.
		parts := strings.SplitN(n.Value, ":", 3)
		src = parts[0]
		typ = "bind"
		if !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, ".") && !strings.HasPrefix(src, "~") {
			typ = "volume" // named volume, not a bind mount
		}
	case yaml.MappingNode:
		m := map[string]string{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = n.Content[i+1].Value
		}
		typ = m["type"]
		src = m["source"]
		if typ == "" {
			typ = "bind"
		}
	default:
		return "unrecognised volume entry (rejected)"
	}
	if isDockerSocket(src) {
		return "mount of the Docker/containerd socket is forbidden: " + src
	}
	if typ != "bind" {
		return ""
	}
	if src == "" {
		return "bind mount with no source (rejected)"
	}
	abs := src
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspaceDir, src)
	}
	abs = filepath.Clean(abs)
	if abs != workspaceDir && !strings.HasPrefix(abs, workspaceDir+string(filepath.Separator)) {
		return "bind mount outside the workspace directory is forbidden: " + src + " -> " + abs
	}
	return ""
}

func isDockerSocket(src string) bool {
	s := filepath.Clean(src)
	for _, d := range dockerSockets {
		if s == d || strings.HasSuffix(s, "/docker.sock") {
			return true
		}
	}
	return false
}
