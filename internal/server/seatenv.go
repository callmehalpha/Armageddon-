package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// seatEnv is the environment of every interactive server-seat process
// (terminal, code-server, SSH): the workspace name and Git configuration
// passed through the environment, so it applies regardless of what the
// workspace user put in repo.git/config (P-13). gitcfg entries are appended
// after the server's own; an empty value resets a multi-valued key.
func seatEnv(rt *runtime, w *store.Workspace, gitcfg [][2]string, extra ...string) []string {
	cfg := append([][2]string{{"core.hooksPath", rt.p.Hooks}}, gitcfg...)
	env := []string{"ARMAGEDDON_WORKSPACE=" + w.Name, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg))}
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return append(env, extra...)
}

// serverHoldsLease reports whether interactive server-seat sessions are
// allowed: the server must hold the workspace lease (contract §4.2).
func (s *Server) serverHoldsLease(wsID string) bool {
	l, err := s.store.LeaseOf(nil, wsID)
	return err == nil && l.HolderKind == "server"
}

const leaseRefusal = "the workspace is owned by a device; the server seat is read-only until it is handed back"

// sameOrigin reports whether the request carries an Origin header naming
// this host. Browsers send Origin on WebSocket handshakes and on every
// non-GET fetch, so requiring it blocks cross-site use of the session cookie.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}
