package helper

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// cgroupRoot is the cgroup v2 mount point.
const cgroupRoot = "/sys/fs/cgroup"

// cgroupSlice holds one cgroup per workspace user.
const cgroupSlice = "armageddon.slice"

// cgroups manages per-workspace cgroup v2 groups under armageddon.slice.
// When cgroup v2 is not available (cgroup v1 or hybrid hosts, containers
// without a delegated hierarchy) it degrades to no cgroups with a logged
// warning; limits are then refused.
type cgroups struct {
	mu      sync.Mutex
	base    string // "" when disabled
	warning string
	open    map[*exec.Cmd]int
}

func newCgroups() *cgroups {
	c := &cgroups{open: map[*exec.Cmd]int{}}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		c.warning = "cgroup v2 is not mounted at " + cgroupRoot + ": workspaces run without per-workspace cgroups or limits"
		return c
	}
	base := filepath.Join(cgroupRoot, cgroupSlice)
	if err := os.Mkdir(base, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		c.warning = fmt.Sprintf("cannot create %s (%v): workspaces run without per-workspace cgroups or limits", base, err)
		return c
	}
	// Delegate the controllers we use; missing ones only disable limits.
	for _, dir := range []string{cgroupRoot, base} {
		for _, ctl := range []string{"+cpu", "+memory", "+pids"} {
			os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(ctl), 0)
		}
	}
	c.base = base
	return c
}

func (c *cgroups) dir(user string) string { return filepath.Join(c.base, user) }

// attach places cmd into the workspace user's cgroup at clone time
// (CLONE_INTO_CGROUP). It returns a warning when cgroups are unavailable.
func (c *cgroups) attach(cmd *exec.Cmd, user string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == "" {
		return c.warning
	}
	dir := c.dir(user)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Sprintf("cgroup %s: %v", dir, err)
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Sprintf("cgroup %s: %v", dir, err)
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = fd
	c.open[cmd] = fd
	return ""
}

func (c *cgroups) release(cmd *exec.Cmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fd, ok := c.open[cmd]; ok {
		unix.Close(fd)
		delete(c.open, cmd)
	}
}

func (c *cgroups) disable(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base != "" {
		log.Printf("WARNING: %s", why)
	}
	c.base, c.warning = "", why
}

// pids lists the processes in the user's cgroup.
func (c *cgroups) pids(user string) []int {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	if base == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(base, user, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func (c *cgroups) remove(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base != "" {
		os.Remove(c.dir(user))
	}
}

func (c *cgroups) setLimits(user string, l Limits) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == "" {
		return errors.New("workspace limits need cgroup v2: " + c.warning)
	}
	dir := c.dir(user)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	const period = 100000
	cpu := "max " + strconv.Itoa(period)
	if l.CPUMilli > 0 {
		cpu = fmt.Sprintf("%d %d", l.CPUMilli*period/1000, period)
	}
	mem, pids := "max", "max"
	if l.MemoryBytes > 0 {
		mem = strconv.FormatInt(l.MemoryBytes, 10)
	}
	if l.Pids > 0 {
		pids = strconv.FormatInt(l.Pids, 10)
	}
	for _, kv := range [][2]string{{"cpu.max", cpu}, {"memory.max", mem}, {"pids.max", pids}} {
		if err := os.WriteFile(filepath.Join(dir, kv[0]), []byte(kv[1]), 0); err != nil {
			return fmt.Errorf("%s: %w", kv[0], err)
		}
	}
	return nil
}
