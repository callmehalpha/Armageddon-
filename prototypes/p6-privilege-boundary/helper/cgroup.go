package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// cgroupV2Root locates the cgroup v2 mount and reports whether it delegates
// the controllers the helper needs (memory, pids). In hybrid-hierarchy
// containers the v2 mount exists but carries only a subset of controllers; in
// that case cgroup v2 limits are unavailable and the caller documents it
// (contract §11 P6, E9).
func cgroupV2Root() (root string, have bool) {
	for _, cand := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/unified"} {
		ctrl, err := os.ReadFile(filepath.Join(cand, "cgroup.controllers"))
		if err != nil {
			continue
		}
		set := map[string]bool{}
		for _, c := range strings.Fields(string(ctrl)) {
			set[c] = true
		}
		if set["memory"] && set["pids"] {
			return cand, true
		}
		// A v2 mount with the wrong controllers still identifies the root.
		if root == "" {
			root = cand
		}
	}
	return root, false
}

// cgroupV2 is a per-workspace cgroup v2 node.
type cgroupV2 struct {
	path string // directory under the v2 root
}

// ensureCgroupV2 creates (or reuses) a per-workspace cgroup v2 and enables the
// controllers on its parent. It returns nil if v2 delegation is unavailable.
func ensureCgroupV2(ws string) (*cgroupV2, error) {
	root, have := cgroupV2Root()
	if !have {
		return nil, nil
	}
	parent := filepath.Join(root, "armageddon")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	// Delegate memory+pids+cpu down to the workspace nodes.
	_ = os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory +pids +cpu"), 0o644)
	path := filepath.Join(parent, "ws-"+ws)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	return &cgroupV2{path: path}, nil
}

// FD opens the cgroup directory for UseCgroupFD so a child is placed in the
// cgroup atomically at clone time (no post-fork race).
func (c *cgroupV2) FD() (int, error) {
	return unix.Open(c.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

func (c *cgroupV2) setLimits(memBytes, pidsMax, cpuWeight int64) error {
	if memBytes > 0 {
		if err := os.WriteFile(filepath.Join(c.path, "memory.max"), []byte(strconv.FormatInt(memBytes, 10)), 0o644); err != nil {
			return err
		}
		_ = os.WriteFile(filepath.Join(c.path, "memory.swap.max"), []byte("0"), 0o644)
	}
	if pidsMax > 0 {
		if err := os.WriteFile(filepath.Join(c.path, "pids.max"), []byte(strconv.FormatInt(pidsMax, 10)), 0o644); err != nil {
			return err
		}
	}
	if cpuWeight > 0 {
		_ = os.WriteFile(filepath.Join(c.path, "cpu.weight"), []byte(strconv.FormatInt(cpuWeight, 10)), 0o644)
	}
	return nil
}

func (c *cgroupV2) pids() []int {
	b, err := os.ReadFile(filepath.Join(c.path, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, l := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(l); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (c *cgroupV2) kill(sig int) error {
	// cgroup.kill (SIGKILL only) is cleanest when available.
	if sig == int(unix.SIGKILL) {
		if err := os.WriteFile(filepath.Join(c.path, "cgroup.kill"), []byte("1"), 0o644); err == nil {
			return nil
		}
	}
	var firstErr error
	for _, pid := range c.pids() {
		if err := unix.Kill(pid, unix.Signal(sig)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *cgroupV2) remove() { _ = os.Remove(c.path) }

// describeCgroups returns a human-readable line for the write-up / doctor.
func describeCgroups() string {
	root, have := cgroupV2Root()
	if have {
		return fmt.Sprintf("cgroup v2 with memory+pids controllers at %s (per-workspace limits available)", root)
	}
	if root != "" {
		ctrl, _ := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
		return fmt.Sprintf("cgroup v2 mount at %s but controllers=%q lack memory/pids (per-workspace v2 limits UNAVAILABLE)", root, strings.TrimSpace(string(ctrl)))
	}
	return "no cgroup v2 mount found (per-workspace limits unavailable)"
}
