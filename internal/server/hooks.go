package server

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/runtimes"
	"github.com/callmehalpha/Armageddon-/internal/treesync/gitshadow"
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
	case "runtime":
		// Runtime providers, run as the workspace user (plan M8).
		return runtimes.Main(args)
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
				if !tracked(u[2]) {
					continue
				}
				m.Updates = append(m.Updates, u)
				// Deletions are trashed once committed, and only if the ref
				// is really gone: Git also runs this hook for the
				// packed-refs side of a delete and for `pack-refs` (run by
				// `git gc`), where a "deleted" loose ref still resolves.
				if !isZero(u[1]) {
					continue
				}
				if _, err := gitOut("rev-parse", "--verify", "-q", u[2]); err != nil {
					if old := pendingOld(u[2], u[0]); old != "" {
						if t := trash(old, u[1], u[2]); t != "" {
							m.Trash = append(m.Trash, t)
						}
					}
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
			if isZero(u[1]) {
				// Deletions are trashed at "committed" (above); remember the
				// tip now, while the ref still resolves.
				if cur, err := gitOut("rev-parse", "--verify", "-q", u[2]); err == nil {
					rememberOld(u[2], cur)
				}
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
	case "seat-apply":
		return seatApplyMain(args)
	}
	fmt.Fprintln(os.Stderr, "armageddon: unknown hook", name)
	return 0
}

// seatApplyMain applies a checkpoint to the server worktree. The server
// runs it as the workspace user (never as root: apply writes files inside a
// directory the workspace user controls, §2.5).
//
//	armageddon hook seat-apply <shadow.git> <tree> <capture.index> <from|-> <to>
//
// Exit status 3 means the tree diverged from <from> (server drift).
func seatApplyMain(args []string) int {
	if len(args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: hook seat-apply GITDIR TREE INDEX FROM TO")
		return 2
	}
	sh := &gitshadow.Shadow{GitDir: args[0], WorkTree: args[1], IndexFile: args[2], Preserve: []string{".env*"}}
	var err error
	if args[3] == "-" {
		err = sh.Seed(args[4])
	} else {
		err = sh.Apply(args[3], args[4], gitshadow.ApplyOptions{})
	}
	if errors.Is(err, gitshadow.ErrDiverged) {
		return 3
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// rememberOld and pendingOld carry a deleted ref's tip from the
// "prepared" hook run to the "committed" one (separate processes).
func pendingFile() string {
	dir, err := gitOut("rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "armageddon-pending-deletes")
}

func rememberOld(ref, oid string) {
	if p := pendingFile(); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintf(f, "%s %s\n", oid, ref)
			f.Close()
		}
	}
}

// pendingOld returns the tip remembered for ref (or hookOld when it is
// set) and forgets it.
func pendingOld(ref, hookOld string) string {
	p := pendingFile()
	b, _ := os.ReadFile(p)
	old, keep := "", []string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if oid, r, ok := strings.Cut(l, " "); ok && r == ref {
			old = oid
		} else if l != "" {
			keep = append(keep, l)
		}
	}
	if len(b) > 0 {
		if len(keep) == 0 {
			os.Remove(p)
		} else {
			os.WriteFile(p, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
		}
	}
	if !isZero(hookOld) {
		return hookOld
	}
	return old
}

func isZero(oid string) bool { return strings.Trim(oid, "0") == "" }

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
