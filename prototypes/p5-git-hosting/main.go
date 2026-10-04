// P5: can the Armageddon server host the canonical repository itself?
//
// A Go HTTP front end wraps `git http-backend` (CGI) and adds what §4–§5 of
// the design need:
//   - device authentication and per-request authorization;
//   - the lease/epoch check on every receive-pack request;
//   - the fencing lock: receive-pack holds the workspace fence as a reader,
//     lease transitions take it as a writer;
//   - server-owned Git settings injected through GIT_CONFIG_* (the repository
//     config is workspace-writable and must not be able to override them);
//   - pre-receive and reference-transaction hooks that call back to the
//     authority over a unix socket and record trash refs for deleted or
//     force-moved branches in a separate checkpoints.git, through an
//     internal Git endpoint (never by sharing object directories).
//
// `p5` runs the scenario suite. `p5 hook <name>` is the hook entry point.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	mrand "math/rand"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const zeroOid = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------------------
// Server

type Workspace struct {
	ID, Root, RepoGit, CheckpointsGit, Tree, HooksDir, CpHooksDir, Sock string
	HookToken                                                           string
	lastPushEnd                                                         time.Time

	fence  sync.RWMutex // receive-pack: RLock; lease transition: Lock
	mu     sync.Mutex
	holder string
	epoch  int
	events []string
}

func (w *Workspace) Lease() (string, int) { w.mu.Lock(); defer w.mu.Unlock(); return w.holder, w.epoch }

// Transfer moves the lease; it waits for in-flight pushes (fencing).
func (w *Workspace) Transfer(to string) (waited time.Duration) {
	_, waited = w.TransferAt(to)
	return
}

// TransferAt also reports when the fence was acquired.
func (w *Workspace) TransferAt(to string) (acquired time.Time, waited time.Duration) {
	t := time.Now()
	w.fence.Lock()
	acquired = time.Now()
	waited = acquired.Sub(t)
	w.mu.Lock()
	w.holder, w.epoch = to, w.epoch+1
	w.mu.Unlock()
	w.fence.Unlock()
	w.event("lease → %s e%d (waited %s for in-flight pushes)", to, w.epoch, waited.Round(time.Millisecond))
	return
}

func (w *Workspace) LastPushEnd() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.lastPushEnd }

func (w *Workspace) event(f string, a ...any) {
	w.mu.Lock()
	w.events = append(w.events, fmt.Sprintf(f, a...))
	w.mu.Unlock()
}

type Server struct {
	self   string            // path to this binary (hooks exec it)
	tokens map[string]string // device → token
	ws     map[string]*Workspace
	addr   string
	plain  bool // baseline: no auth, no hooks, no lease (throughput comparison)
	pushes sync.WaitGroup
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4)
	if len(parts) < 4 {
		http.NotFound(rw, r)
		return
	}
	kind, wsID, repo, rest := parts[0], parts[1], parts[2], parts[3]
	w := s.ws[wsID]
	if w == nil {
		http.NotFound(rw, r)
		return
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		service = rest
	}
	isReceive := service == "git-receive-pack"

	env := []string{"GIT_HTTP_EXPORT_ALL=1", "GIT_PROJECT_ROOT=" + w.Root}
	switch {
	case s.plain:
	case kind == "git" && repo == "repo.git":
		user, pass, ok := r.BasicAuth()
		if !ok || s.tokens[user] == "" || s.tokens[user] != pass {
			rw.Header().Set("WWW-Authenticate", `Basic realm="armageddon"`)
			http.Error(rw, "authentication required", http.StatusUnauthorized)
			return
		}
		epoch, _ := strconv.Atoi(r.Header.Get("X-Armageddon-Epoch"))
		if isReceive {
			holder, cur := w.Lease()
			if holder != user || epoch != cur {
				http.Error(rw, fmt.Sprintf("lease_lost: holder=%s epoch=%d (you sent %d)", holder, cur, epoch), http.StatusConflict)
				return
			}
			if r.Method == http.MethodPost {
				// Fencing: hold the fence for the whole receive-pack, then
				// re-check the lease under it.
				w.fence.RLock()
				defer w.fence.RUnlock()
				if h, e := w.Lease(); h != user || e != epoch {
					http.Error(rw, "lease_lost", http.StatusConflict)
					return
				}
				s.pushes.Add(1)
				defer s.pushes.Done()
				defer func() { // runs before the fence is released
					w.mu.Lock()
					w.lastPushEnd = time.Now()
					w.mu.Unlock()
				}()
			}
		}
		env = append(env, "REMOTE_USER="+user, "ARMAGEDDON_DEVICE="+user, "ARMAGEDDON_EPOCH="+strconv.Itoa(epoch))
	case kind == "internal" && repo == "checkpoints.git":
		if r.Header.Get("X-Armageddon-Hook-Token") != w.HookToken {
			http.Error(rw, "forbidden", http.StatusForbidden)
			return
		}
		env = append(env, "REMOTE_USER=hook")
	default:
		http.NotFound(rw, r)
		return
	}
	if !s.plain {
		// Server-owned settings. GIT_CONFIG_* outranks repository config, so
		// a workspace editing repo.git/config cannot change these.
		hooks := w.HooksDir
		if kind == "internal" {
			hooks = w.CpHooksDir
		}
		cfg := [][2]string{
			{"core.hooksPath", hooks},
			{"receive.denyCurrentBranch", "ignore"},
			{"receive.fsckObjects", "true"},
			{"receive.advertisePushOptions", "false"},
			{"http.receivepack", "true"},
			{"uploadpack.allowAnySHA1InWant", "false"},
		}
		env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
		for i, kv := range cfg {
			env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
		}
		env = append(env, "ARMAGEDDON_SOCK="+w.Sock, "ARMAGEDDON_WS="+w.ID, "ARMAGEDDON_PUSH=1",
			"ARMAGEDDON_INTERNAL_URL=http://"+s.addr+"/internal/"+w.ID+"/checkpoints.git",
			"ARMAGEDDON_HOOK_TOKEN="+w.HookToken, "ARMAGEDDON_BIN="+s.self)
	} else {
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true")
	}
	h := &cgi.Handler{Path: "/usr/bin/env", Args: []string{"git", "http-backend"}, Env: env,
		InheritEnv: []string{"PATH", "HOME"}}
	r.URL.Path = "/" + repo + "/" + rest
	h.ServeHTTP(rw, r)
}

// Authority callbacks from hooks (unix socket, one JSON request per conn).
type hookReq struct {
	Op      string      `json:"op"`
	Device  string      `json:"device"`
	Epoch   int         `json:"epoch"`
	Updates [][3]string `json:"updates"` // old, new, ref
	State   string      `json:"state"`
}
type hookResp struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
}

func (w *Workspace) serveSocket() error {
	os.Remove(w.Sock)
	l, err := net.Listen("unix", w.Sock)
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req hookReq
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					return
				}
				json.NewEncoder(c).Encode(w.hook(req))
			}(c)
		}
	}()
	return nil
}

func (w *Workspace) hook(req hookReq) hookResp {
	switch req.Op {
	case "pre-receive":
		holder, epoch := w.Lease()
		if req.Device != holder || req.Epoch != epoch {
			return hookResp{false, "lease_lost (re-checked in pre-receive)"}
		}
		for _, u := range req.Updates {
			if u[2] == "refs/heads/forbidden" {
				return hookResp{false, "policy: refs/heads/forbidden is not allowed"}
			}
		}
		w.event("pre-receive ok: %d updates by %s e%d", len(req.Updates), req.Device, req.Epoch)
	case "ref-tx":
		for _, u := range req.Updates {
			w.event("server-seat ref %s %s: %.7s → %.7s", req.State, u[2], u[0], u[1])
		}
	}
	return hookResp{true, ""}
}

// ---------------------------------------------------------------------------
// Hooks (run by git; this binary is the hook)

func callAuthority(req hookReq) hookResp {
	c, err := net.Dial("unix", os.Getenv("ARMAGEDDON_SOCK"))
	if err != nil {
		return hookResp{false, "authority unreachable: " + err.Error()}
	}
	defer c.Close()
	json.NewEncoder(c).Encode(req)
	var resp hookResp
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return hookResp{false, "authority: " + err.Error()}
	}
	return resp
}

func gitOut(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// recordTrash preserves an old tip in checkpoints.git before the ref moves,
// by pushing it through the internal endpoint (not by touching that repo's
// files: it belongs to a different OS user in the real design).
func recordTrash(old, ref, cause string) error {
	dst := fmt.Sprintf("refs/trash/%d/%s/%s", time.Now().UnixNano(), cause, strings.TrimPrefix(ref, "refs/"))
	cmd := exec.Command("git", "-c", "http.extraHeader=X-Armageddon-Hook-Token: "+os.Getenv("ARMAGEDDON_HOOK_TOKEN"),
		"push", "-q", "--no-verify", os.Getenv("ARMAGEDDON_INTERNAL_URL"), old+":"+dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("trash push failed: %v: %s", err, out)
	}
	return nil
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

func needsTrash(old, new string) (bool, string) {
	if old == zeroOid || old == "" {
		return false, ""
	}
	if new == zeroOid {
		return true, "delete"
	}
	if exec.Command("git", "merge-base", "--is-ancestor", old, new).Run() != nil {
		return true, "force"
	}
	return false, ""
}

func hookMain(name string, args []string) int {
	switch name {
	case "pre-receive":
		ups := readUpdates()
		epoch, _ := strconv.Atoi(os.Getenv("ARMAGEDDON_EPOCH"))
		resp := callAuthority(hookReq{Op: "pre-receive", Device: os.Getenv("ARMAGEDDON_DEVICE"), Epoch: epoch, Updates: ups})
		if !resp.OK {
			fmt.Fprintln(os.Stderr, "armageddon:", resp.Msg)
			return 1
		}
		for _, u := range ups {
			if ok, cause := needsTrash(u[0], u[1]); ok && strings.HasPrefix(u[2], "refs/heads/") || ok && strings.HasPrefix(u[2], "refs/tags/") {
				if err := recordTrash(u[0], u[2], cause); err != nil {
					fmt.Fprintln(os.Stderr, "armageddon:", err)
					return 1 // never move a ref whose old tip we could not preserve
				}
			}
		}
		return 0
	case "reference-transaction":
		state := args[0]
		ups := readUpdates()
		if os.Getenv("ARMAGEDDON_PUSH") == "1" {
			return 0 // receive-pack path is handled by pre-receive
		}
		var rel [][3]string
		for _, u := range ups {
			if strings.HasPrefix(u[2], "refs/heads/") || strings.HasPrefix(u[2], "refs/tags/") {
				// The hook's old value can be zero for unconditional updates;
				// read the real current value (refs are locked, readable).
				if cur, err := gitOut("rev-parse", "--verify", "-q", u[2]); err == nil {
					u[0] = cur
				}
				rel = append(rel, u)
			}
		}
		if len(rel) == 0 {
			return 0
		}
		cfg := readHookEnvFile()
		for k, v := range cfg {
			os.Setenv(k, v)
		}
		if state == "prepared" {
			for _, u := range rel {
				if ok, cause := needsTrash(u[0], u[1]); ok {
					if err := recordTrash(u[0], u[2], cause); err != nil {
						fmt.Fprintln(os.Stderr, "armageddon:", err)
						return 1 // aborts the local ref transaction
					}
				}
			}
		}
		if state == "committed" {
			callAuthority(hookReq{Op: "ref-tx", State: state, Updates: rel})
		}
		return 0
	case "checkpoints-pre-receive":
		for _, u := range readUpdates() {
			if !strings.HasPrefix(u[2], "refs/trash/") || u[0] != zeroOid {
				fmt.Fprintf(os.Stderr, "internal endpoint: only creating refs/trash/* is allowed (got %s)\n", u[2])
				return 1
			}
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown hook", name)
	return 1
}

// Local server-seat git commands have no request env; the hook reads its
// endpoint settings from a file in the server-owned hooks directory.
func readHookEnvFile() map[string]string {
	b, err := os.ReadFile(filepath.Join(os.Getenv("ARMAGEDDON_HOOKS_DIR"), "hook.env"))
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Setup

func run(dir string, env []string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("git %v in %s: %v\n%s", args, dir, err, out))
	}
	return strings.TrimSpace(string(out))
}

func try(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func newWorkspace(base, id, self, addr string) *Workspace {
	root := filepath.Join(base, id)
	os.RemoveAll(root)
	w := &Workspace{ID: id, Root: root, RepoGit: filepath.Join(root, "repo.git"),
		CheckpointsGit: filepath.Join(root, "checkpoints.git"), Tree: filepath.Join(root, "tree"),
		HooksDir: filepath.Join(root, "hooks"), Sock: filepath.Join(root, "authority.sock"),
		holder: "server", epoch: 1}
	tok := make([]byte, 16)
	rand.Read(tok)
	w.HookToken = hex.EncodeToString(tok)
	os.MkdirAll(w.HooksDir, 0o755)
	run(base, nil, "init", "-q", "--bare", "-b", "main", w.RepoGit)
	run(base, nil, "init", "-q", "--bare", w.CheckpointsGit)
	run(w.RepoGit, nil, "config", "core.logAllRefUpdates", "always")
	run(w.RepoGit, nil, "config", "gc.auto", "0")
	// Server-seat local git uses the same server-owned hooks directory.
	run(w.RepoGit, nil, "config", "core.hooksPath", w.HooksDir)
	cpHooks := filepath.Join(root, "cp-hooks")
	w.CpHooksDir = cpHooks
	os.MkdirAll(cpHooks, 0o755)
	run(w.CheckpointsGit, nil, "config", "core.hooksPath", cpHooks)
	for _, h := range []struct{ dir, name, as string }{
		{w.HooksDir, "pre-receive", "pre-receive"},
		{w.HooksDir, "reference-transaction", "reference-transaction"},
		{cpHooks, "pre-receive", "checkpoints-pre-receive"},
	} {
		script := fmt.Sprintf("#!/bin/sh\nARMAGEDDON_HOOKS_DIR=$(dirname \"$0\") exec %q hook %s \"$@\"\n", self, h.as)
		os.WriteFile(filepath.Join(h.dir, h.name), []byte(script), 0o755)
	}
	os.WriteFile(filepath.Join(w.HooksDir, "hook.env"), []byte(strings.Join([]string{
		"ARMAGEDDON_SOCK=" + w.Sock,
		"ARMAGEDDON_INTERNAL_URL=http://" + addr + "/internal/" + id + "/checkpoints.git",
		"ARMAGEDDON_HOOK_TOKEN=" + w.HookToken,
	}, "\n")), 0o600)
	// Seed history and the server worktree.
	seed := filepath.Join(base, id+"-seed")
	os.RemoveAll(seed)
	run(base, nil, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644)
	run(seed, gitIdent, "add", ".")
	run(seed, gitIdent, "commit", "-q", "-m", "seed")
	run(seed, nil, "push", "-q", w.RepoGit, "main", "main:feature", "main:feature2")
	run(w.RepoGit, nil, "worktree", "add", "-q", w.Tree, "main")
	if err := w.serveSocket(); err != nil {
		panic(err)
	}
	return w
}

var gitIdent = []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x"}

// ---------------------------------------------------------------------------
// Scenarios

type result struct{ name, status, detail string }

var results []result

func check(name string, ok bool, detail string, a ...any) {
	st := "PASS"
	if !ok {
		st = "FAIL"
	}
	results = append(results, result{name, st, fmt.Sprintf(detail, a...)})
	fmt.Printf("%s  %-62s %s\n", st, name, fmt.Sprintf(detail, a...))
}

func refOf(repo, ref string) string {
	out, err := try(repo, nil, "rev-parse", "--verify", "-q", ref)
	if err != nil {
		return ""
	}
	return out
}

func trashRefs(w *Workspace) []string {
	out, _ := try(w.CheckpointsGit, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/trash/")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "hook" {
		os.Exit(hookMain(os.Args[2], os.Args[3:]))
	}
	bigMB := flag.Int("big-mb", 300, "size of the throughput repository (MiB of incompressible blobs)")
	reps := flag.Int("reps", 3, "throughput repetitions")
	workdir := flag.String("workdir", "", "scratch directory")
	flag.Parse()
	if *workdir == "" {
		*workdir, _ = os.MkdirTemp("", "p5-")
	}
	base, _ := filepath.Abs(*workdir)
	os.MkdirAll(base, 0o755)
	self, _ := os.Executable()
	gv, _ := gitOut("version")
	fmt.Printf("P5 Git hosting — %s\n\n", gv)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	addr := ln.Addr().String()
	srv := &Server{self: self, addr: addr, tokens: map[string]string{"d1": "tok1", "d2": "tok2"}, ws: map[string]*Workspace{}}
	w := newWorkspace(base, "ws1", self, addr)
	srv.ws["ws1"] = w
	go http.Serve(ln, srv)

	url := func(dev string) string {
		return fmt.Sprintf("http://%s:%s@%s/git/ws1/repo.git", dev, srv.tokens[dev], addr)
	}
	ep := func(e int) []string { return []string{"-c", "http.extraHeader=X-Armageddon-Epoch: " + strconv.Itoa(e)} }
	push := func(dir string, e int, args ...string) (string, error) {
		return try(dir, gitIdent, append(ep(e), append([]string{"push"}, args...)...)...)
	}
	d1 := filepath.Join(base, "d1")
	d2 := filepath.Join(base, "d2")
	os.RemoveAll(d1)
	os.RemoveAll(d2)

	// S1
	_, e1 := try(base, nil, "clone", "-q", url("d1"), d1)
	_, e2 := try(base, nil, "clone", "-q", url("d2"), d2)
	_, e3 := try(base, nil, "clone", "-q", fmt.Sprintf("http://d1:wrong@%s/git/ws1/repo.git", addr), filepath.Join(base, "bad"))
	check("S1 clone by paired devices; bad token refused", e1 == nil && e2 == nil && e3 != nil, "d1=%v d2=%v bad-token-err=%v", e1 == nil, e2 == nil, e3 != nil)

	w.Transfer("d1") // epoch 2
	commitIn := func(dir, msg string) string {
		os.WriteFile(filepath.Join(dir, "f.txt"), []byte(msg+"\n"), 0o644)
		run(dir, gitIdent, "add", "f.txt")
		run(dir, gitIdent, "commit", "-q", "-m", msg)
		return refOf(dir, "HEAD")
	}

	// S2
	c2 := commitIn(d1, "two")
	_, err = push(d1, 2, "-q", "origin", "main")
	wtHead := refOf(w.Tree, "HEAD")
	check("S2 holder push to branch checked out in server worktree", err == nil && refOf(w.RepoGit, "main") == c2 && wtHead == c2,
		"err=%v repo main=%.7s worktree HEAD=%.7s (worktree files are updated by checkpoint apply, not by push)", err, refOf(w.RepoGit, "main"), wtHead)

	// S3
	c3 := commitIn(d1, "three")
	out, err := push(d1, 1, "-q", "origin", "main")
	check("S3 holder push with stale epoch rejected", err != nil && refOf(w.RepoGit, "main") == c2, "rejected=%v (%s)", err != nil, firstLine(out))

	// S4
	commitIn(d2, "d2-change")
	out, err = push(d2, 2, "-q", "origin", "main")
	check("S4 non-holder device push rejected", err != nil && refOf(w.RepoGit, "main") == c2, "rejected=%v (%s)", err != nil, firstLine(out))

	// S5
	run(d1, nil, "branch", "-f", "forbidden", "HEAD")
	out, err = push(d1, 2, "--atomic", "origin", "main", "forbidden")
	check("S5 --atomic push with one ref vetoed by authority: nothing lands", err != nil && refOf(w.RepoGit, "main") == c2 && refOf(w.RepoGit, "forbidden") == "",
		"main still %.7s, forbidden absent=%v", refOf(w.RepoGit, "main"), refOf(w.RepoGit, "forbidden") == "")
	_, err = push(d1, 2, "-q", "origin", "main")
	if err != nil || refOf(w.RepoGit, "main") != c3 {
		check("S5b normal push after veto", false, "%v", err)
	}

	// S6
	featureTip := refOf(w.RepoGit, "feature")
	_, err = push(d1, 2, "-q", "origin", ":feature")
	tr := trashRefs(w)
	ok6 := err == nil && refOf(w.RepoGit, "feature") == "" && containsOid(tr, featureTip, "delete/heads/feature")
	_, objErr := try(w.CheckpointsGit, nil, "cat-file", "-e", featureTip+"^{commit}")
	check("S6 pushed branch deletion preserved as trash ref", ok6 && objErr == nil, "trash=%v objects present=%v", tr, objErr == nil)

	// S7
	run(d1, gitIdent, "reset", "-q", "--hard", "HEAD~1")
	c7 := commitIn(d1, "rewritten")
	_, err = push(d1, 2, "-q", "--force", "origin", "main")
	check("S7 force push preserves the overwritten tip as trash ref", err == nil && refOf(w.RepoGit, "main") == c7 && containsOid(trashRefs(w), c3, "force/heads/main"),
		"old tip %.7s in trash=%v", c3, containsOid(trashRefs(w), c3, "force/heads/main"))

	// S8: server seat (local git in tree/) — lease back to server first.
	w.Transfer("server")
	run(w.Tree, gitIdent, "reset", "-q", "--hard", "main")
	f2 := refOf(w.RepoGit, "feature2")
	_, errD := try(w.Tree, gitIdent, "branch", "-q", "-D", "feature2")
	before := refOf(w.Tree, "HEAD")
	c8 := commitIn(w.Tree, "server-seat commit")
	_, errR := try(w.Tree, gitIdent, "reset", "-q", "--hard", "HEAD~1")
	trs := trashRefs(w)
	check("S8 server-seat local delete and reset preserved via reference-transaction hook",
		errD == nil && errR == nil && containsOid(trs, f2, "delete/heads/feature2") && containsOid(trs, c8, "force/heads/main") && refOf(w.Tree, "HEAD") == before,
		"feature2 trashed=%v, reset-away commit %.7s trashed=%v", containsOid(trs, f2, "delete/heads/feature2"), c8, containsOid(trs, c8, "force/heads/main"))

	// S9: internal endpoint only accepts refs/trash creations, only with token.
	_, errNoTok := try(base, nil, "ls-remote", "http://"+addr+"/internal/ws1/checkpoints.git")
	_, errBadRef := try(w.RepoGit, nil, "-c", "http.extraHeader=X-Armageddon-Hook-Token: "+w.HookToken, "push", "-q",
		"http://"+addr+"/internal/ws1/checkpoints.git", "main:refs/heads/sneaky")
	check("S9 internal checkpoints endpoint: token required, only refs/trash/*", errNoTok != nil && errBadRef != nil, "no-token refused=%v non-trash ref refused=%v", errNoTok != nil, errBadRef != nil)

	// S10: workspace-writable repo config cannot redirect hooks.
	evil := filepath.Join(base, "evil-hooks")
	os.MkdirAll(evil, 0o755)
	marker := filepath.Join(base, "EVIL_RAN")
	os.Remove(marker)
	os.WriteFile(filepath.Join(evil, "pre-receive"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(evil, "post-receive"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	run(w.RepoGit, nil, "config", "core.hooksPath", evil) // what a malicious workspace process could do
	w.Transfer("d1")                                      // epoch 4
	_, epoch := w.Lease()
	run(d1, nil, "fetch", "-q", "origin")
	run(d1, gitIdent, "reset", "-q", "--hard", "origin/main")
	commitIn(d1, "after-evil-config")
	_, err = push(d1, epoch, "-q", "origin", "main")
	_, statErr := os.Stat(marker)
	check("S10 repo.git/config core.hooksPath overridden by server-owned GIT_CONFIG_*", err == nil && os.IsNotExist(statErr),
		"push ok=%v evil hook ran=%v", err == nil, statErr == nil)
	run(w.RepoGit, nil, "config", "core.hooksPath", w.HooksDir)

	// S11: fencing — a takeover waits for an in-flight push; the pusher's
	// next push with the old epoch is rejected.
	fence(base, w, d1, push, *bigMB/3)

	// S12: throughput vs plain http-backend.
	throughput(base, addr, srv, w, *bigMB, *reps)

	fmt.Println()
	fails := 0
	for _, r := range results {
		if r.status == "FAIL" {
			fails++
		}
	}
	fmt.Printf("FAILURES=%d of %d scenarios\n", fails, len(results))
	fmt.Println("authority events:")
	for _, e := range w.events {
		fmt.Println("   ", e)
	}
	if fails > 0 {
		os.Exit(1)
	}
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "lease_lost") || strings.Contains(l, "error") || strings.Contains(l, "rejected") {
			return strings.TrimSpace(l)
		}
	}
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func containsOid(refs []string, oid, suffix string) bool {
	for _, r := range refs {
		if strings.HasSuffix(strings.Fields(r)[0], suffix) && strings.HasSuffix(r, oid) {
			return true
		}
	}
	return false
}

func randomBlobs(dir string, mb int, files int) {
	buf := make([]byte, mb*(1<<20)/files)
	for i := 0; i < files; i++ {
		mrand.Read(buf)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("blob-%d.bin", i)), buf, 0o644)
	}
}

func fence(base string, w *Workspace, d1 string, push func(string, int, ...string) (string, error), mb int) {
	_, epoch := w.Lease()
	randomBlobs(d1, mb, 8)
	run(d1, gitIdent, "add", "-A")
	run(d1, gitIdent, "commit", "-q", "-m", "big")
	big := refOf(d1, "HEAD")
	type res struct {
		end time.Time
		err error
	}
	done := make(chan res)
	go func() {
		_, err := push(d1, epoch, "-q", "origin", "main")
		done <- res{time.Now(), err}
	}()
	// Wait until the receive-pack POST is in flight, then take over.
	time.Sleep(150 * time.Millisecond)
	for i := 0; i < 1500; i++ {
		if w.fence.TryLock() {
			w.fence.Unlock()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		break
	}
	acquired, waited := w.TransferAt("server")
	r := <-done
	pushEnd := w.LastPushEnd()
	landed := refOf(w.RepoGit, "main") == big
	commitInStale := func() error {
		os.WriteFile(filepath.Join(d1, "late.txt"), []byte("late\n"), 0o644)
		run(d1, gitIdent, "add", "late.txt")
		run(d1, gitIdent, "commit", "-q", "-m", "late")
		_, err := push(d1, epoch, "-q", "origin", "main")
		return err
	}
	lateErr := commitInStale()
	check("S11 takeover waits for in-flight push (fencing); old epoch then rejected",
		r.err == nil && landed && !acquired.Before(pushEnd) && waited > 0 && lateErr != nil,
		"push of %d MiB ok=%v landed=%v; takeover blocked %s and acquired the fence %s after receive-pack finished; late push with old epoch rejected=%v",
		mb, r.err == nil, landed, waited.Round(time.Millisecond), acquired.Sub(pushEnd).Round(time.Microsecond), lateErr != nil)
}

func throughput(base, addr string, srv *Server, w *Workspace, mb, reps int) {
	// Build a repository with `mb` MiB of incompressible content in 3 commits.
	src := filepath.Join(base, "big-src")
	os.RemoveAll(src)
	run(base, nil, "init", "-q", "-b", "main", src)
	for c := 0; c < 3; c++ {
		d := filepath.Join(src, fmt.Sprintf("c%d", c))
		os.MkdirAll(d, 0o755)
		randomBlobs(d, mb/3, 30)
		run(src, gitIdent, "add", "-A")
		run(src, gitIdent, "commit", "-q", "-m", fmt.Sprintf("c%d", c))
	}
	plainWS := newWorkspace(base, "plain", srv.self, addr)
	fullWS := newWorkspace(base, "full", srv.self, addr)
	run(plainWS.RepoGit, nil, "config", "--unset", "core.hooksPath") // baseline: stock http-backend, no hooks
	run(src, nil, "push", "-q", "--force", plainWS.RepoGit, "main")
	run(src, nil, "push", "-q", "--force", fullWS.RepoGit, "main")
	run(plainWS.RepoGit, nil, "repack", "-adq")
	run(fullWS.RepoGit, nil, "repack", "-adq")

	plain := &Server{self: srv.self, plain: true, ws: map[string]*Workspace{"plain": plainWS}}
	pl, _ := net.Listen("tcp", "127.0.0.1:0")
	plain.addr = pl.Addr().String()
	go http.Serve(pl, plain)
	srv.ws["full"] = fullWS

	timeClone := func(u string) time.Duration {
		dst := filepath.Join(base, "clone-tmp")
		os.RemoveAll(dst)
		t := time.Now()
		run(base, nil, "clone", "-q", "--bare", u, dst)
		d := time.Since(t)
		os.RemoveAll(dst)
		return d
	}
	var pc, fc []time.Duration
	for i := 0; i < reps; i++ {
		pc = append(pc, timeClone("http://"+plain.addr+"/git/plain/repo.git"))
		fc = append(fc, timeClone(fmt.Sprintf("http://d1:tok1@%s/git/full/repo.git", addr)))
	}
	// Push throughput: a fresh 1/3-size commit pushed by the lease holder.
	timePush := func(wsName string, ws *Workspace, u string, withEpoch bool) time.Duration {
		dst := filepath.Join(base, "push-tmp")
		os.RemoveAll(dst)
		run(base, nil, "clone", "-q", u, dst)
		d := filepath.Join(dst, "new")
		os.MkdirAll(d, 0o755)
		randomBlobs(d, mb/3, 10)
		run(dst, gitIdent, "add", "-A")
		run(dst, gitIdent, "commit", "-q", "-m", "push-test")
		args := []string{"push", "-q", "origin", "HEAD:refs/heads/push-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
		if withEpoch {
			_, e := ws.Lease()
			args = append([]string{"-c", "http.extraHeader=X-Armageddon-Epoch: " + strconv.Itoa(e)}, args...)
		}
		t := time.Now()
		run(dst, gitIdent, args...)
		el := time.Since(t)
		os.RemoveAll(dst)
		return el
	}
	fullWS.Transfer("d1")
	var pp, fp []time.Duration
	for i := 0; i < reps; i++ {
		pp = append(pp, timePush("plain", plainWS, "http://"+plain.addr+"/git/plain/repo.git", false))
		fp = append(fp, timePush("full", fullWS, fmt.Sprintf("http://d1:tok1@%s/git/full/repo.git", addr), true))
	}
	med := func(d []time.Duration) time.Duration {
		c := append([]time.Duration{}, d...)
		sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
		return c[len(c)/2]
	}
	sz, _ := gitOut("-C", fullWS.RepoGit, "count-objects", "-vH")
	packSize := ""
	for _, l := range strings.Split(sz, "\n") {
		if strings.HasPrefix(l, "size-pack") {
			packSize = strings.TrimSpace(strings.TrimPrefix(l, "size-pack:"))
		}
	}
	cloneOverhead := float64(med(fc))/float64(med(pc)) - 1
	pushOverhead := float64(med(fp))/float64(med(pp)) - 1
	check("S12 clone throughput within 20% of plain git http-backend", cloneOverhead < 0.20,
		"repo %s: plain %s vs armageddon %s (%+.1f%%), median of %d", packSize, med(pc).Round(time.Millisecond), med(fc).Round(time.Millisecond), 100*cloneOverhead, reps)
	check("S12b push throughput (lease holder) within 20% of plain", pushOverhead < 0.20,
		"%d MiB push: plain %s vs armageddon %s (%+.1f%%; includes hook + authority callback)", mb/3, med(pp).Round(time.Millisecond), med(fp).Round(time.Millisecond), 100*pushOverhead)
}
