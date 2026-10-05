package server

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const zeroOid = "0000000000000000000000000000000000000000"

// HookMain is the entry point for `armageddon hook <name>`, run by git as
// the workspace user. (This file is the one place in internal/server that
// starts git directly: it already runs as ws-<id>, spawned by git itself.)
// It reports ref updates to the authority over the workspace socket
// (P-14, authority.go). It preserves the old tip of every deleted or
// force-moved branch or tag as refs/armageddon/trash/<ns>/<cause>/<ref>
// (contract §5.3–§5.4). MVP: trash lives in repo.git itself, hidden from
// clients by server-injected uploadpack/receive hideRefs.
func HookMain(name string, args []string) int {
	switch name {
	case "post-receive":
		// Pushed updates: by now the objects have left quarantine and the
		// old tips are still in the object store (gc.auto=0).
		m := HookMessage{Hook: name}
		for _, u := range readUpdates() {
			m.Updates = append(m.Updates, u)
			if t := trash(u[0], u[1], u[2]); t != "" {
				m.Trash = append(m.Trash, t)
			}
		}
		notifyAuthority(m)
		return 0
	case "reference-transaction":
		if len(args) == 0 || os.Getenv("ARMAGEDDON_PUSH") == "1" {
			return 0
		}
		if args[0] == "committed" {
			m := HookMessage{Hook: name}
			for _, u := range readUpdates() {
				if tracked(u[2]) {
					m.Updates = append(m.Updates, u)
				}
			}
			notifyAuthority(m)
			return 0
		}
		if args[0] != "prepared" {
			return 0
		}
		for _, u := range readUpdates() {
			if !tracked(u[2]) {
				continue
			}
			// The hook's old value can be zero for unconditional updates:
			// read the real one (refs are locked but readable).
			if cur, err := gitOut("rev-parse", "--verify", "-q", u[2]); err == nil {
				u[0] = cur
			}
			trash(u[0], u[1], u[2])
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "armageddon: unknown hook", name)
	return 0
}

func tracked(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")
}

// trash preserves old and returns the trash ref ("" if nothing was kept).
func trash(old, new, ref string) string {
	if !tracked(ref) || old == "" || old == zeroOid {
		return ""
	}
	cause := ""
	if new == zeroOid {
		cause = "delete"
	} else if exec.Command("git", "merge-base", "--is-ancestor", old, new).Run() != nil {
		cause = "force"
	}
	if cause == "" {
		return ""
	}
	dst := fmt.Sprintf("refs/armageddon/trash/%d/%s/%s", time.Now().UnixNano(), cause, strings.TrimPrefix(ref, "refs/"))
	if out, err := exec.Command("git", "update-ref", dst, old).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "armageddon: could not preserve %s (%s): %v %s\n", ref, old, err, out)
		return ""
	}
	return dst
}

func readUpdates() [][3]string {
	var ups [][3]string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 {
			ups = append(ups, [3]string{f[0], f[1], f[2]})
		}
	}
	return ups
}

func gitOut(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}
