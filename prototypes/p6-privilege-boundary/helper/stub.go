package helper

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// StubMain is the trampoline the helper execs as the workspace user. By the
// time it runs, the parent has already dropped to the ws-<id> uid/gid (via
// SysProcAttr.Credential) and, when cgroup v2 is available, placed it in the
// per-workspace cgroup. The stub then:
//
//   - sets PR_SET_NO_NEW_PRIVS so the final program and its children can never
//     gain privileges through setuid/setgid/file capabilities (contract §2.5);
//   - if a PTY was passed on fd 0, makes it the controlling terminal;
//   - execs the real argv.
//
// Invoked as:  p6 stub [--pty] -- <argv...>
// NO_NEW_PRIVS is deliberately set in the child, not the parent: the parent is
// root and still needs to spawn.
func StubMain(args []string) int {
	pty := false
	for len(args) > 0 && args[0] != "--" {
		if args[0] == "--pty" {
			pty = true
		}
		args = args[1:]
	}
	if len(args) < 2 {
		os.Stderr.WriteString("stub: missing -- argv\n")
		return 2
	}
	argv := args[1:]

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		os.Stderr.WriteString("stub: no_new_privs: " + err.Error() + "\n")
		return 2
	}
	if pty {
		// Fd 0 is the pty slave; start a new session and claim the tty.
		_, _ = unix.Setsid()
		_ = unix.IoctlSetInt(0, unix.TIOCSCTTY, 0)
	}
	// Resolve argv[0] against PATH the way exec.LookPath would, cheaply.
	path := argv[0]
	if err := unix.Exec(path, argv, os.Environ()); err != nil {
		os.Stderr.WriteString("stub: exec " + path + ": " + err.Error() + "\n")
		return 127
	}
	return 0
}

func atoiDefault(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
