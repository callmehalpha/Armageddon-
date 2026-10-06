package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/callmehalpha/Armageddon-/internal/components"
)

// checkCodeServer is the code-server kind's program check. code-server runs
// as the workspace user, so the risk is not root: it is one workspace (or
// anyone else) replacing the program that every other workspace's IDE
// runs. The helper therefore accepts exactly one program, the one it
// resolves itself the way the server does (code_server.path from
// server.json, else code-server on the fixed workspace PATH, else the
// installed component), and only if neither it nor any directory above it
// is writable by anyone but root and the server user.
func (d *Daemon) checkCodeServer(prog string) error {
	want, err := d.codeServerPath()
	if err != nil {
		return fmt.Errorf("code-server: %w", err)
	}
	if prog != want {
		return fmt.Errorf("code-server: %s is not the configured code-server (%s)", prog, want)
	}
	if err := d.trustedProgram(prog); err != nil {
		return fmt.Errorf("code-server: %w", err)
	}
	return nil
}

// codeServerPath reads code_server.path from the server's config (opened
// beneath the data directory without following symlinks) and resolves it.
func (d *Daemon) codeServerPath() (string, error) {
	data, err := d.openData()
	if err != nil {
		return "", err
	}
	defer unix.Close(data)
	configured := ""
	fd, err := openBeneath(data, "server.json", unix.O_RDONLY)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return "", fmt.Errorf("server.json: %w", err)
	}
	if err == nil {
		f := os.NewFile(uintptr(fd), "server.json")
		var cfg struct {
			CodeServer struct {
				Path string `json:"path"`
			} `json:"code_server"`
		}
		err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&cfg)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("server.json: %w", err)
		}
		configured = cfg.CodeServer.Path
	}
	return components.ResolveCodeServer(configured, d.DataDir)
}

// trustedProgram checks that prog, the file it resolves to, and every
// directory above either are owned by root or the server user and are not
// writable by group or others (sticky directories excepted), and that the
// program is not inside a workspace.
func (d *Daemon) trustedProgram(prog string) error {
	real, err := filepath.EvalSymlinks(prog)
	if err != nil {
		return err
	}
	if within(filepath.Join(d.DataDir, "workspaces"), real) {
		return fmt.Errorf("%s is inside a workspace", real)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", real)
	}
	check := func(p string, leaf bool) error {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		if st.Uid != 0 && st.Uid != d.ServerUID {
			return fmt.Errorf("%s is owned by uid %d (only root or the server user may own it)", p, st.Uid)
		}
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil // a symlink's own mode means nothing; its target is checked
		}
		if st.Mode&0o022 != 0 && !(isDir && !leaf && st.Mode&unix.S_ISVTX != 0) {
			return fmt.Errorf("%s is writable by group or others (mode %o)", p, st.Mode&0o7777)
		}
		return nil
	}
	for _, p := range []string{filepath.Clean(prog), real} {
		if err := check(p, true); err != nil {
			return err
		}
		for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
			if err := check(dir, false); err != nil {
				return err
			}
			if dir == "/" {
				break
			}
		}
	}
	return nil
}
