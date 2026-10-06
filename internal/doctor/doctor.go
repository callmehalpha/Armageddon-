// Package doctor implements `armageddon doctor` on the server (contract
// §9.3, §10; plan M5.6). Every check returns a status, what it found and,
// when something is wrong, a remedy the operator can act on.
package doctor

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/wsgit"

	_ "modernc.org/sqlite"
)

type Status string

const (
	OK       Status = "ok"
	Warn     Status = "warn"
	Fail     Status = "FAIL"
	Skip     Status = "skip"
	Repaired Status = "repaired"
)

// Result is the outcome of one check.
type Result struct {
	Check  string `json:"check"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// MinGit is the oldest supported Git (contract §9.1).
var MinGit = [3]int{2, 40, 0}

// Env is what the checks look at. Zero values mean the real system; tests
// substitute fixtures.
type Env struct {
	DataDir string
	Git     string // git binary; default "git"
	// HelperSupported: this binary has the privileged helper (Phase 2).
	HelperSupported bool
	HelperSocket    string // default /run/armageddon/helper.sock
	CgroupRoot      string // default /sys/fs/cgroup
	// Statfs returns free and total bytes of the filesystem holding path.
	Statfs func(path string) (free, total uint64, err error)
	// Openat2 probes openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) on dir.
	Openat2 func(dir string) error
	// FsckSample is how many workspaces get a `git fsck` (default 3).
	FsckSample int
	// Repair fixes what can be fixed safely (checkpoint refs from the DB).
	Repair bool
	// WorkspaceGit builds a Git command that runs as the workspace's OS
	// user (§2.5: Git on repo.git never runs as root or the server).
	WorkspaceGit wsgit.Func
}

func (e *Env) defaults() {
	if e.Git == "" {
		e.Git = "git"
	}
	if e.HelperSocket == "" {
		e.HelperSocket = "/run/armageddon/helper.sock"
	}
	if e.CgroupRoot == "" {
		e.CgroupRoot = "/sys/fs/cgroup"
	}
	if e.Statfs == nil {
		e.Statfs = statfs
	}
	if e.Openat2 == nil {
		e.Openat2 = probeOpenat2
	}
	if e.FsckSample == 0 {
		e.FsckSample = 3
	}
	if e.WorkspaceGit == nil {
		e.WorkspaceGit = wsgit.New(e.DataDir, e.HelperSocket)
	}
}

// Run executes every check in order.
func Run(e Env) []Result {
	e.defaults()
	out := []Result{
		CheckDisk(&e),
		CheckPermissions(&e),
		CheckHelper(&e),
		CheckGit(&e),
		CheckCgroup(&e),
		CheckOpenat2(&e),
	}
	wss, err := workspaces(e.DataDir)
	if err != nil {
		out = append(out, Result{Check: "database", Status: Fail, Detail: err.Error(),
			Remedy: "Check that the data directory is right (--data) and that the database is readable; restore from a backup if it is damaged (`armageddon server restore`)."})
		return out
	}
	out = append(out, CheckWorkspaceStates(wss))
	out = append(out, CheckFsck(&e, wss))
	out = append(out, CheckReconcile(&e, wss)...)
	return out
}

// Failed reports whether any result is a failure.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// Print writes results as a table.
func Print(w io.Writer, rs []Result) {
	for _, r := range rs {
		fmt.Fprintf(w, "%-9s %-22s %s\n", "["+string(r.Status)+"]", r.Check, r.Detail)
		if r.Remedy != "" && r.Status != OK {
			fmt.Fprintf(w, "%-9s %-22s remedy: %s\n", "", "", r.Remedy)
		}
	}
}

// ---- system checks ----

func statfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// CheckDisk: below 5% free, workspaces go DEGRADED (F8).
func CheckDisk(e *Env) Result {
	r := Result{Check: "disk"}
	free, total, err := e.Statfs(e.DataDir)
	if err != nil {
		r.Status, r.Detail = Fail, err.Error()
		r.Remedy = "Create the data directory or pass the right one with --data."
		return r
	}
	pct := 100 * float64(free) / float64(max(total, 1))
	r.Detail = fmt.Sprintf("%.1f GiB free of %.1f GiB (%.1f%%) on the filesystem holding %s", gib(free), gib(total), pct, e.DataDir)
	remedy := fmt.Sprintf("Free space on that filesystem (old backups in %s, unused Docker images, logs) or grow the disk, then run `armageddon doctor --repair`.", filepath.Join(e.DataDir, "backups"))
	switch {
	case pct < 5:
		r.Status, r.Remedy = Fail, "Below 5% free, commits are rejected with disk_full (F8). "+remedy
	case pct < 10:
		r.Status, r.Remedy = Warn, remedy
	default:
		r.Status = OK
	}
	return r
}

func gib(b uint64) float64 { return float64(b) / (1 << 30) }

// CheckPermissions: the database, configuration, keys and checkpoint
// repositories must be private to the server (§2.4, §2.5); the data
// directory must be traversable but not writable by others.
func CheckPermissions(e *Env) Result {
	r := Result{Check: "permissions"}
	var problems, fixes []string
	fi, err := os.Stat(e.DataDir)
	if err != nil {
		r.Status, r.Detail = Fail, err.Error()
		r.Remedy = "Run `armageddon server init` or pass the right --data directory."
		return r
	}
	m := fi.Mode().Perm()
	if m&0o002 != 0 {
		problems = append(problems, fmt.Sprintf("%s is world-writable (%v)", e.DataDir, m))
		fixes = append(fixes, "chmod o-w "+e.DataDir)
	}
	if m&0o001 == 0 {
		problems = append(problems, fmt.Sprintf("%s is not traversable by workspace users (%v)", e.DataDir, m))
		fixes = append(fixes, "chmod 755 "+e.DataDir)
	}
	private := []string{filepath.Join(e.DataDir, "armageddon.db"), config.Path(e.DataDir), filepath.Join(e.DataDir, "keys")}
	if ents, err := os.ReadDir(filepath.Join(e.DataDir, "workspaces")); err == nil {
		for _, ent := range ents {
			private = append(private, filepath.Join(e.DataDir, "workspaces", ent.Name(), "checkpoints.git"))
		}
	}
	keys := filepath.Join(e.DataDir, "keys")
	filepath.Walk(keys, func(p string, fi os.FileInfo, err error) error {
		if err == nil && p != keys && fi.Mode().IsRegular() && !strings.HasSuffix(p, ".crt") {
			private = append(private, p)
		}
		return nil
	})
	for _, p := range private {
		fi, err := os.Stat(p)
		if err != nil {
			continue // optional paths
		}
		if fi.Mode().Perm()&0o077 != 0 {
			problems = append(problems, fmt.Sprintf("%s is accessible to other users (%v)", p, fi.Mode().Perm()))
			fixes = append(fixes, "chmod go-rwx "+p)
		}
	}
	if len(problems) == 0 {
		r.Status, r.Detail = OK, "database, configuration, keys and checkpoint repositories are private"
		return r
	}
	r.Status, r.Detail = Fail, strings.Join(problems, "; ")
	r.Remedy = "Run: " + strings.Join(fixes, " && ")
	return r
}

// CheckHelper: the privileged helper's socket answers (Phase 2 layout).
func CheckHelper(e *Env) Result {
	r := Result{Check: "helper"}
	if !e.HelperSupported {
		r.Status, r.Detail = Skip, "this build has no privileged helper (single-process layout: the server runs as root)"
		return r
	}
	fi, err := os.Stat(e.HelperSocket)
	if err != nil {
		r.Status, r.Detail = Fail, fmt.Sprintf("no helper socket at %s", e.HelperSocket)
		r.Remedy = "Start the helper: `systemctl start armageddon-helper`, then check `journalctl -u armageddon-helper`."
		return r
	}
	if fi.Mode()&os.ModeSocket == 0 {
		r.Status, r.Detail = Fail, fmt.Sprintf("%s is not a socket", e.HelperSocket)
		r.Remedy = "Remove the stray file and restart the helper: `systemctl restart armageddon-helper`."
		return r
	}
	c, err := net.DialTimeout("unix", e.HelperSocket, 2*time.Second)
	if err != nil {
		r.Status, r.Detail = Fail, fmt.Sprintf("helper socket %s does not answer: %v", e.HelperSocket, err)
		r.Remedy = "Restart the helper: `systemctl restart armageddon-helper`. Run doctor as the armageddon user or root (the socket is 0600)."
		return r
	}
	c.Close()
	r.Status, r.Detail = OK, "helper reachable at "+e.HelperSocket
	return r
}

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)(?:\.(\d+))?`)

// CheckGit: Git ≥ 2.40 (contract §9.1).
func CheckGit(e *Env) Result {
	r := Result{Check: "git"}
	remedy := fmt.Sprintf("Install Git %d.%d or newer (Debian 12: bookworm-backports; Ubuntu: ppa:git-core/ppa; Fedora: dnf install git).", MinGit[0], MinGit[1])
	out, err := exec.Command(e.Git, "--version").Output()
	if err != nil {
		r.Status, r.Detail, r.Remedy = Fail, "git not found: "+err.Error(), remedy
		return r
	}
	m := gitVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		r.Status, r.Detail, r.Remedy = Fail, "cannot parse "+strings.TrimSpace(string(out)), remedy
		return r
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	r.Detail = strings.TrimSpace(string(out))
	for i := 0; i < 3; i++ {
		if v[i] != MinGit[i] {
			if v[i] < MinGit[i] {
				r.Status, r.Remedy = Fail, remedy
				return r
			}
			break
		}
	}
	r.Status = OK
	return r
}

// CheckCgroup: cgroup v2 is needed for per-workspace limits (§2.5).
func CheckCgroup(e *Env) Result {
	r := Result{Check: "cgroup v2"}
	b, err := os.ReadFile(filepath.Join(e.CgroupRoot, "cgroup.controllers"))
	if err != nil {
		r.Status, r.Detail = Warn, fmt.Sprintf("no unified cgroup v2 hierarchy at %s: per-workspace CPU, memory and pids limits are unavailable", e.CgroupRoot)
		r.Remedy = "Boot with the unified hierarchy (kernel parameter systemd.unified_cgroup_hierarchy=1; the default on Debian 11+, Ubuntu 21.10+, Fedora 31+)."
		return r
	}
	ctrls := strings.Fields(string(b))
	var missing []string
	for _, want := range []string{"cpu", "memory", "pids"} {
		found := false
		for _, c := range ctrls {
			found = found || c == want
		}
		if !found {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		r.Status, r.Detail = Warn, "cgroup v2 present but controllers missing: "+strings.Join(missing, ", ")
		r.Remedy = "Enable the controllers (systemd delegates them to services with Delegate=yes)."
		return r
	}
	r.Status, r.Detail = OK, "cgroup v2 with cpu, memory and pids controllers"
	return r
}

// CheckOpenat2: the helper resolves workspace paths with openat2 (Linux
// 5.6+) so a planted symlink cannot redirect it (§2.5, P6 E6).
func CheckOpenat2(e *Env) Result {
	r := Result{Check: "openat2"}
	err := e.Openat2(e.DataDir)
	switch {
	case err == nil:
		r.Status, r.Detail = OK, "openat2 with RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS works"
	case errors.Is(err, unix.ENOSYS):
		r.Status, r.Detail = Warn, "openat2 is not available (kernel older than 5.6, or blocked by a seccomp filter)"
		r.Remedy = "Upgrade the kernel to 5.6 or newer (Debian 11+, Ubuntu 20.10+, Fedora 32+). Without it the helper cannot rule out symlink races when preparing workspace directories."
	default:
		r.Status, r.Detail = Warn, "openat2 probe failed: "+err.Error()
		r.Remedy = "Check that the data directory exists and is not itself a symlink."
	}
	return r
}

// ---- workspace checks ----

type wsRow struct {
	ID, Slug, State, OSUser, Current string
	Seq                              int64
}

func workspaces(dataDir string) ([]wsRow, error) {
	p := filepath.Join(dataDir, "armageddon.db")
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("no database at %s", p)
	}
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, slug, state, os_user, COALESCE(current_checkpoint_id, ''), checkpoint_seq FROM workspaces WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("reading workspaces: %w", err)
	}
	defer rows.Close()
	var out []wsRow
	for rows.Next() {
		var w wsRow
		if err := rows.Scan(&w.ID, &w.Slug, &w.State, &w.OSUser, &w.Current, &w.Seq); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CheckWorkspaceStates summarises lifecycle states.
func CheckWorkspaceStates(wss []wsRow) Result {
	r := Result{Check: "workspaces"}
	n := map[string]int{}
	var bad []string
	for _, w := range wss {
		n[w.State]++
		if w.State == "degraded" || w.State == "failed" {
			bad = append(bad, w.Slug+" ("+w.State+")")
		}
	}
	r.Detail = fmt.Sprintf("%d workspaces: %d ready", len(wss), n["ready"])
	if len(bad) > 0 {
		r.Status = Warn
		r.Detail += "; not ready: " + strings.Join(bad, ", ")
		r.Remedy = "See the workspace's state reason in the web UI or `armageddon workspaces`; fix the cause (disk, corruption) and restart the server."
		return r
	}
	r.Status = OK
	return r
}

func ready(wss []wsRow) []wsRow {
	var out []wsRow
	for _, w := range wss {
		if w.State == "ready" || w.State == "degraded" {
			out = append(out, w)
		}
	}
	return out
}

func (e *Env) cpsGit(id string, args ...string) ([]byte, error) {
	dir := filepath.Join(e.DataDir, "workspaces", id, "checkpoints.git")
	cmd := exec.Command(e.Git, append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	return cmd.CombinedOutput()
}

// CheckFsck runs `git fsck --connectivity-only` on a sample of workspaces:
// checkpoints.git as the server, repo.git as the workspace user (§2.5).
func CheckFsck(e *Env, wss []wsRow) Result {
	r := Result{Check: "git fsck (sample)"}
	cands := ready(wss)
	rand.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	if len(cands) > e.FsckSample {
		cands = cands[:e.FsckSample]
	}
	if len(cands) == 0 {
		r.Status, r.Detail = Skip, "no ready workspaces"
		return r
	}
	var bad, names, repaired []string
	emptyTreeOnly := true
	for _, w := range cands {
		names = append(names, w.Slug)
		root := filepath.Join(e.DataDir, "workspaces", w.ID)
		asServer := func(args ...string) *exec.Cmd {
			cmd := exec.Command(e.Git, append([]string{"--git-dir=" + filepath.Join(root, "checkpoints.git")}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
			return cmd
		}
		name := w.OSUser
		if name == "" {
			name = helper.UserName(w.ID)
		}
		if _, err := e.WorkspaceGit(w.ID, name, root, "--version"); err != nil {
			bad = append(bad, fmt.Sprintf("%s: workspace user: %v", w.Slug, err))
			emptyTreeOnly = false
			continue
		}
		asWS := func(args ...string) *exec.Cmd {
			cmd, _ := e.WorkspaceGit(w.ID, name, root, append([]string{"--git-dir=" + filepath.Join(root, "repo.git")}, args...)...)
			cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1")
			return cmd
		}
		for _, repo := range []struct {
			name string
			git  func(...string) *exec.Cmd
		}{{"checkpoints.git", asServer}, {"repo.git", asWS}} {
			out, err := repo.git("fsck", "--no-progress", "--connectivity-only").CombinedOutput()
			if err == nil {
				continue
			}
			// Workspaces created before this fix referenced Git's implicit
			// empty tree without writing it: harmless, and repairable.
			if isMissingEmptyTreeOnly(out) {
				if e.Repair {
					if werr := repo.git("hash-object", "-w", "-t", "tree", "/dev/null").Run(); werr == nil {
						if repo.git("fsck", "--no-progress", "--connectivity-only").Run() == nil {
							repaired = append(repaired, w.Slug+" "+repo.name)
							continue
						}
					}
				}
			} else {
				emptyTreeOnly = false
			}
			bad = append(bad, fmt.Sprintf("%s %s: %s", w.Slug, repo.name, firstLine(out, err)))
		}
	}
	if len(bad) > 0 {
		r.Status, r.Detail = Fail, strings.Join(bad, "; ")
		r.Remedy = "Repository corruption (F10). Restore the objects from a device replica (`git fetch` from it into the server) or from the latest backup (`armageddon server restore` onto a fresh data directory, then copy the workspace)."
		if emptyTreeOnly {
			r.Remedy = "Only Git's empty tree object is missing (workspaces created by v0.1.0-mvp); no data is affected. Run `armageddon doctor --repair` to write it."
		}
		return r
	}
	if len(repaired) > 0 {
		r.Status, r.Detail = Repaired, "wrote the missing empty tree in "+strings.Join(repaired, ", ")
		return r
	}
	r.Status, r.Detail = OK, "clean: "+strings.Join(names, ", ")
	return r
}

func isMissingEmptyTreeOnly(out []byte) bool {
	found := false
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "" || strings.HasPrefix(l, "notice:") || strings.HasPrefix(l, "dangling "):
		case l == "missing tree "+emptyTree:
			found = true
		default:
			return false
		}
	}
	return found
}

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func firstLine(out []byte, err error) string {
	s := ""
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "notice:") {
			s = l
			break
		}
	}
	if s == "" {
		s = err.Error()
	}
	return s
}

// CheckReconcile compares each workspace's current checkpoint in the
// database with refs/checkpoints/current. The database is truth (§8.3);
// with Repair the ref is rewritten from it, which is what startup
// reconciliation does too.
func CheckReconcile(e *Env, wss []wsRow) []Result {
	var out []Result
	n := 0
	for _, w := range ready(wss) {
		if w.Current == "" {
			continue
		}
		n++
		r := Result{Check: "reconcile " + w.Slug}
		if _, err := e.cpsGit(w.ID, "cat-file", "-e", w.Current+"^{commit}"); err != nil {
			r.Status = Fail
			r.Detail = fmt.Sprintf("the database's current checkpoint #%d (%s) is missing from checkpoints.git", w.Seq, short(w.Current))
			r.Remedy = "Acknowledged data is missing: restore this workspace from the latest backup, or reseed it from a replica (`armageddon workspace seed --from-replica`)."
			out = append(out, r)
			continue
		}
		ref, err := e.cpsGit(w.ID, "rev-parse", "-q", "--verify", "refs/checkpoints/current")
		got := strings.TrimSpace(string(ref))
		if err == nil && got == w.Current {
			continue
		}
		if got == "" {
			got = "(missing)"
		}
		r.Detail = fmt.Sprintf("database says #%d %s, refs/checkpoints/current is %s", w.Seq, short(w.Current), short(got))
		if e.Repair {
			if outb, err := e.cpsGit(w.ID, "update-ref", "refs/checkpoints/current", w.Current); err != nil {
				r.Status, r.Remedy = Fail, "repair failed: "+firstLine(outb, err)
			} else {
				r.Status, r.Detail = Repaired, r.Detail+"; ref rewritten from the database"
			}
		} else {
			r.Status = Fail
			r.Remedy = "Run `armageddon doctor --repair` (rewrites the ref from the database, which is the source of truth), or restart the server: startup reconciliation does the same."
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		out = append(out, Result{Check: "reconcile", Status: OK, Detail: fmt.Sprintf("database and refs/checkpoints/current agree for %d workspaces", n)})
	}
	return out
}

func short(oid string) string {
	if len(oid) > 12 && !strings.HasPrefix(oid, "(") {
		return oid[:12]
	}
	return oid
}
