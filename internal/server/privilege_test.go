package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoPrivilegeOutsideHelper enforces contract §2.5 on this package's
// source: the server never switches credentials itself, never starts
// processes except through the helper client (Account.Command / Prepare /
// helper.Client.Spawn), and builds no git-shadow for workspace repositories
// that would run git as the server user.
//
// Allowed exceptions:
//   - hooks.go: `armageddon hook` runs inside git, already as ws-<id>.
//   - gitshadow.Shadow values over checkpoints.git, which the server owns
//     and workspace processes never open.
func TestNoPrivilegeOutsideHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`syscall\.Credential|Credential\s*:`), "credential switch outside the helper"},
		{regexp.MustCompile(`\bSetuid\b|\bSetgid\b|\bSetresuid\b|\bSetgroups\b`), "credential switch outside the helper"},
		{regexp.MustCompile(`\bos\.(Chown|Lchown)\b|\.Chown\(`), "chown: the server cannot give files away; create them as the workspace user"},
		{regexp.MustCompile(`exec\.Command(Context)?\(`), "process started outside the helper client (use rt.acct.Command or s.helper.Spawn)"},
		{regexp.MustCompile(`\bpty\.(Open|Start)`), "PTYs are opened by the helper"},
		{regexp.MustCompile(`\bsysuser\.`), "use the helper client, not the MVP sysuser package"},
	}
	shadowLit := regexp.MustCompile(`gitshadow\.Shadow\{`)
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		src := stripComments(string(b))
		for _, fb := range forbidden {
			if f == "hooks.go" && strings.HasPrefix(fb.re.String(), `exec\.Command`) {
				continue
			}
			for _, loc := range fb.re.FindAllStringIndex(src, -1) {
				t.Errorf("%s:%d: %s: %q", f, line(src, loc[0]), fb.why, src[loc[0]:loc[1]])
			}
		}
		// hooks.go is the entry point of processes that already run as the
		// workspace user (Git hooks, `hook seat-apply`), so its git-shadow
		// needs no Prepare.
		for _, loc := range shadowLit.FindAllStringIndex(src, -1) {
			if f == "hooks.go" {
				continue
			}
			lit := balanced(src[loc[0]:])
			if !strings.Contains(lit, "Prepare:") && !strings.Contains(lit, "p.Checkpoints") {
				t.Errorf("%s:%d: git-shadow without Prepare runs git as the server user; only checkpoints.git may: %s", f, line(src, loc[0]), lit)
			}
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned only %d files", scanned)
	}
}

var commentRe = regexp.MustCompile(`(?m)//.*$`)

func stripComments(s string) string {
	return commentRe.ReplaceAllStringFunc(s, func(c string) string { return strings.Repeat(" ", len(c)) })
}

func line(s string, off int) int { return strings.Count(s[:off], "\n") + 1 }

// balanced returns s up to the brace closing its first opening brace.
func balanced(s string) string {
	depth := 0
	for i, c := range s {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}
