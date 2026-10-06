// Package wsgit runs Git as a workspace's OS user for the offline admin
// commands (doctor, backup). Git on repo.git never runs as root or as the
// server user, because the workspace can plant command-running config
// there (contract §2.5).
package wsgit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"github.com/callmehalpha/Armageddon-/internal/helper"
)

// Func is a Git command builder for one workspace.
type Func func(wsID, osUser, dir string, args ...string) (*exec.Cmd, error)

// New returns how an admin command runs Git as a workspace user:
//   - as root (an administrator running the command): drop to the
//     existing workspace user directly. It never creates users;
//   - as the server user with a helper: through the helper, like the server;
//   - otherwise (development mode): as the current user, like the server.
func New(dataDir, sock string) Func {
	return func(wsID, osUser, dir string, args ...string) (*exec.Cmd, error) {
		if os.Geteuid() == 0 {
			u, err := user.Lookup(osUser)
			if err != nil {
				return nil, fmt.Errorf("no OS user %s: %w", osUser, err)
			}
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + u.HomeDir, "LANG=C.UTF-8"}
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
			return cmd, nil
		}
		if sock == "" {
			sock = helper.DefaultSocket
		}
		var client helper.Client = helper.NewDev(dataDir)
		if helper.Reachable(sock) {
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			client = &helper.Socket{Path: sock, DataDir: dataDir, Shim: exe}
		}
		acct, err := client.CreateWorkspaceUser(context.Background(), wsID)
		if err != nil {
			return nil, err
		}
		return acct.Command(dir, "git", args...), nil
	}
}
