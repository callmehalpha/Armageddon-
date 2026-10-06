package proto

import (
	"regexp"
	"strings"
)

// workspaceRe matches a workspace id. The helper derives the OS user name
// (ws-<id>) and the workspace directory from it, so it must be tightly
// constrained: lowercase alphanumerics only, bounded length. No dots, slashes
// or dashes, so neither path traversal nor an unexpected user name is possible.
var workspaceRe = regexp.MustCompile(`^[a-z0-9]{8,24}$`)

// ValidWorkspace reports whether id is an acceptable workspace id.
func ValidWorkspace(id string) bool { return workspaceRe.MatchString(id) }

// UserName returns the OS user name for a workspace. Only call with a
// ValidWorkspace id.
func UserName(id string) string { return "ws-" + id }

// envKeyRe constrains the KEY in an allowlisted KEY=VALUE environment entry.
var envKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// envAllow is the closed set of environment keys a workspace process may be
// given. Anything that changes the dynamic loader or injects code is absent.
var envAllow = map[string]bool{
	"TERM": true, "LANG": true, "LC_ALL": true, "SHELL": true,
	"ARMAGEDDON_WORKSPACE": true, "GIT_PROTOCOL": true,
	"GIT_CONFIG_COUNT": true, "SSH_ORIGINAL_COMMAND": true,
}

// allowPrefixes: GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n are allowed (server
// injects trash-hook config), but no other GIT_* and nothing like LD_*.
var allowPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}

func validEnv(e string) bool {
	k, v, ok := strings.Cut(e, "=")
	if !ok || !envKeyRe.MatchString(k) {
		return false
	}
	if strings.ContainsAny(v, "\x00\n") {
		return false
	}
	if envAllow[k] {
		return true
	}
	for _, p := range allowPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
