package helper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Per-kind argv rules of SpawnInWorkspace (contract §2.5). Every kind runs
// as the workspace user with no_new_privs, so these rules are not what
// keeps root safe; they keep each kind to the programs it exists for, so a
// request of one kind cannot be used to start something else under its
// name, and they pin the programs the helper itself chooses.

// SFTPServerArgv is the complete argv of an ssh-session that serves SFTP.
// The helper replaces it with its own binary's `sftp-server` subcommand,
// so the server never names the program.
const SFTPServerArgv = "sftp-server"

var shells = map[string]bool{"/bin/bash": true, "/bin/sh": true, "/usr/bin/bash": true, "/usr/bin/sh": true,
	"/bin/zsh": true, "/usr/bin/zsh": true, "/usr/bin/fish": true}

// resolveProgram finds argv[0] on the fixed workspace PATH. The program
// runs as the workspace user, so this is about predictability, not
// privilege.
func resolveProgram(name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(SafePath) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found on the workspace PATH", name)
}

// programFor checks argv against the kind's rules and returns the program
// to execute and the argv to give it. self is the helper's own binary
// (for the SFTP server).
//
//   - git-service: git only.
//   - pty-shell: a listed shell, any arguments (a login shell, or `-c CMD`
//     for an SSH exec with a terminal).
//   - ssh-session: a listed shell as `SHELL`, `SHELL -l` or `SHELL -c CMD`,
//     or exactly [SFTPServerArgv].
//   - code-server: an absolute path. The real helper also requires it to be
//     the configured or installed code-server and not replaceable by a
//     workspace (see Daemon.checkCodeServer).
//   - runtime-command: any program on the workspace PATH.
func programFor(kind Kind, argv []string, self string) (string, []string, error) {
	switch kind {
	case KindGitService:
		if argv[0] != "git" {
			return "", nil, errors.New("git-service runs git only")
		}
	case KindPTYShell:
		if !shells[argv[0]] {
			return "", nil, fmt.Errorf("pty-shell: %s is not an allowed shell", argv[0])
		}
	case KindSSHSession:
		if len(argv) == 1 && argv[0] == SFTPServerArgv {
			if self == "" {
				return "", nil, errors.New("ssh-session: the helper does not know its own binary")
			}
			return self, []string{self, "sftp-server"}, nil
		}
		ok := shells[argv[0]] && (len(argv) == 1 || len(argv) == 2 && argv[1] == "-l" || len(argv) == 3 && argv[1] == "-c")
		if !ok {
			return "", nil, errors.New("ssh-session runs a listed shell (SHELL, SHELL -l, SHELL -c CMD) or the SFTP server only")
		}
	case KindCodeServer:
		if !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] {
			return "", nil, errors.New("code-server: the program must be a clean absolute path")
		}
		return argv[0], argv, nil
	}
	prog, err := resolveProgram(argv[0])
	return prog, argv, err
}

// shellFor is the SHELL of a workspace process: the shell itself for
// shells, else bash (or sh) for code-server and SSH sessions, whose
// terminals start it. Other kinds get none.
func shellFor(kind Kind, argv []string) string {
	switch kind {
	case KindPTYShell:
		return argv[0]
	case KindSSHSession, KindCodeServer:
		if shells[argv[0]] {
			return argv[0]
		}
		if _, err := os.Stat("/bin/bash"); err == nil {
			return "/bin/bash"
		}
		return "/bin/sh"
	}
	return ""
}
