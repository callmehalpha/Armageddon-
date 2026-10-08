package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// `armageddon git trash` (contract §5.3–§5.4, §10 F13): deleted and
// force-moved branches and tags are kept by the server's hooks as
// refs/armageddon/trash/<ns>/<cause>/<ref>, hidden from clones. Run it in
// the server seat (browser terminal, IDE or SSH), where repo.git's refs
// are visible; `git gc --prune=now` never removes what they point at.

const trashPrefix = "refs/armageddon/trash/"

type trashEntry struct {
	Ref, Oid, Cause, Orig, Subject string
	When                           time.Time
}

func gitTrashCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "trash" {
		return errors.New("usage: armageddon git trash [list | restore <n|ref> [--as <branch-or-ref>]]")
	}
	args = args[1:]
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	entries, err := trashEntries()
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		if len(entries) == 0 {
			fmt.Fprintln(out, "The trash is empty: no branch or tag has been deleted or force-moved here.")
			return nil
		}
		for i, e := range entries {
			fmt.Fprintf(out, "%3d  %s  %-6s %-30s %.12s  %s\n", i+1, e.When.Format("2006-01-02 15:04"), e.Cause, e.Orig, e.Oid, e.Subject)
		}
		fmt.Fprintln(out, "\nRestore one with `armageddon git trash restore <n>` (or `--as <name>` to restore it under another name).")
		return nil
	case "restore":
		fs := flag.NewFlagSet("git trash restore", flag.ContinueOnError)
		as := fs.String("as", "", "restore under this branch or ref instead of its original name")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: armageddon git trash restore <n|ref> [--as <branch-or-ref>]")
		}
		var e *trashEntry
		if n, err := strconv.Atoi(fs.Arg(0)); err == nil && n >= 1 && n <= len(entries) {
			e = &entries[n-1]
		}
		for i := range entries {
			if entries[i].Ref == fs.Arg(0) {
				e = &entries[i]
			}
		}
		if e == nil {
			return fmt.Errorf("%s: no such trash entry (see `armageddon git trash list`)", fs.Arg(0))
		}
		dst := e.Orig
		if *as != "" {
			dst = *as
			if !strings.HasPrefix(dst, "refs/") {
				dst = "refs/heads/" + dst
			}
		}
		if err := exec.Command("git", "check-ref-format", dst).Run(); err != nil || strings.HasPrefix(dst, "refs/armageddon/") {
			return fmt.Errorf("%s is not a valid branch or tag name", dst)
		}
		// Create only: never overwrite a ref that exists now.
		if b, err := exec.Command("git", "update-ref", "-m", "armageddon git trash restore", dst, e.Oid, strings.Repeat("0", len(e.Oid))).CombinedOutput(); err != nil {
			return fmt.Errorf("%s already exists or could not be created (%s); restore under another name with --as", dst, strings.TrimSpace(string(b)))
		}
		fmt.Fprintf(out, "Restored %s at %.12s (%s). The trash entry is kept.\n", dst, e.Oid, e.Subject)
		return nil
	}
	return errors.New("usage: armageddon git trash [list | restore <n|ref> [--as <branch-or-ref>]]")
}

// trashEntries lists the trash, newest first.
func trashEntries() ([]trashEntry, error) {
	b, err := exec.Command("git", "for-each-ref", "--format=%(refname)%00%(objectname)%00%(contents:subject)", trashPrefix).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("not in a Git repository: run this in the workspace on the server (terminal, IDE or SSH): %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var out []trashEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 {
			continue
		}
		// <ns>/<cause>/<ref without refs/>
		parts := strings.SplitN(strings.TrimPrefix(f[0], trashPrefix), "/", 3)
		if len(parts) != 3 {
			continue
		}
		ns, _ := strconv.ParseInt(parts[0], 10, 64)
		out = append(out, trashEntry{Ref: f[0], Oid: f[1], Subject: f[2], Cause: parts[1], Orig: "refs/" + parts[2], When: time.Unix(0, ns)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].When.After(out[j].When) })
	return out, nil
}
