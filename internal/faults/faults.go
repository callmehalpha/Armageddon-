// Package faults provides crash points for the disaster suite (plan M9.1,
// contract §10 F6–F7). A point named in ARMAGEDDON_FAULTS (comma
// separated) kills the process with SIGKILL when reached, the way a power
// cut or the OOM killer would: no deferred cleanup, no flushing. Unset,
// as in production, a point costs one map lookup.
//
// An entry "clock=<duration>" (e.g. clock=+3h) shifts Now, the clock the
// agent reads for skew and for naming its queued checkpoints (F15).
// faketime cannot do this: Go reads the clock without libc.
package faults

import (
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

var armed, offset = parse(os.Getenv("ARMAGEDDON_FAULTS"))

func parse(v string) (map[string]bool, time.Duration) {
	m := map[string]bool{}
	var off time.Duration
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if d, ok := strings.CutPrefix(f, "clock="); ok {
			off, _ = time.ParseDuration(strings.TrimPrefix(d, "+"))
		} else if f != "" {
			m[f] = true
		}
	}
	return m, off
}

// Now is time.Now, shifted by an injected clock offset.
func Now() time.Time { return time.Now().Add(offset) }

// Point crashes the process if name is armed.
func Point(name string) {
	if !armed[name] {
		return
	}
	log.Printf("FAULT INJECTION: crashing at %q (ARMAGEDDON_FAULTS)", name)
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {} // SIGKILL is not deliverable late, but never continue past a point
}
