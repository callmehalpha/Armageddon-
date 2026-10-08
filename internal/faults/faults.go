// Package faults provides crash points for the disaster suite (plan M9.1,
// contract §10 F6–F7). A point named in ARMAGEDDON_FAULTS (comma
// separated) kills the process with SIGKILL when reached, the way a power
// cut or the OOM killer would: no deferred cleanup, no flushing. Unset,
// as in production, a point costs one map lookup.
package faults

import (
	"log"
	"os"
	"strings"
	"syscall"
)

var armed = parse(os.Getenv("ARMAGEDDON_FAULTS"))

func parse(v string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			m[f] = true
		}
	}
	return m
}

// Point crashes the process if name is armed.
func Point(name string) {
	if !armed[name] {
		return
	}
	log.Printf("FAULT INJECTION: crashing at %q (ARMAGEDDON_FAULTS)", name)
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // SIGKILL is not deliverable late, but never continue past a point
}
