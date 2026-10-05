package server

import "sync"

// Session kinds registered with the session registry (contract §4.3).
const (
	SessionTerminal   = "terminal"
	SessionCodeServer = "code-server"
	SessionSSH        = "ssh"
)

// sessionRegistry tracks the interactive server-seat sessions of every
// workspace, so a lease transfer can close all of them (contract §4.3,
// plan M4.6). Every interactive session kind registers here: the browser
// terminal now, code-server and SSH later.
type sessionRegistry struct {
	mu   sync.Mutex
	next uint64
	byWS map[string]map[uint64]*seatSession
}

type seatSession struct {
	Kind   string
	UserID string
	close  func(reason string)
}

// Register records a session. closeFn must end the session promptly and
// show reason to the user where the kind allows it. The returned function
// unregisters the session and is safe to call more than once.
func (r *sessionRegistry) Register(wsID, kind, userID string, closeFn func(reason string)) (unregister func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byWS == nil {
		r.byWS = map[string]map[uint64]*seatSession{}
	}
	if r.byWS[wsID] == nil {
		r.byWS[wsID] = map[uint64]*seatSession{}
	}
	r.next++
	id := r.next
	r.byWS[wsID][id] = &seatSession{Kind: kind, UserID: userID, close: closeFn}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.byWS[wsID], id)
	}
}

// CloseAll closes every session of a workspace and returns how many it
// closed. Sessions unregister themselves as they end.
func (r *sessionRegistry) CloseAll(wsID, reason string) int {
	r.mu.Lock()
	var fns []func(string)
	for _, s := range r.byWS[wsID] {
		fns = append(fns, s.close)
	}
	r.mu.Unlock()
	for _, f := range fns {
		f(reason)
	}
	return len(fns)
}

// Count returns the number of open sessions of a workspace, by kind.
func (r *sessionRegistry) Count(wsID string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, s := range r.byWS[wsID] {
		out[s.Kind]++
	}
	return out
}
