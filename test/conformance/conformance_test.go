// Package conformance runs the P4 failure model's scenario generator
// against the real implementation (plan M7.8): a real server, two agents
// as separate processes, and a fault-injecting proxy between them, with
// kill -9 of agents and of the server at commit and handoff steps.
//
// The checker shares no code with the protocol. After the faults stop and
// the system converges it verifies, from the server's database and Git
// repositories and the devices' disks:
//
//	I1  every committed checkpoint was authored by the seat granted its epoch,
//	    and no epoch was granted to two seats
//	I3  current contains every token of every committed checkpoint
//	I4  every token made on any seat is in current, in a quarantine on the
//	    server, or still on that seat's disk
//	liveness: the system converges (no stuck handoff, queues and outboxes
//	    drained, the writer's work committed)
//
// Work is modelled as token files (tok/<n>.txt) that are only ever added.
//
// It is slow (real processes, real timers), so it runs only when asked:
//
//	CONFORMANCE_RUNS=2 CONFORMANCE_STEPS=30 go test ./test/conformance -run Conformance -v -timeout 30m
package conformance

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func buildBinary(t *testing.T) string {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "armageddon-conformance-bin-")
		if err != nil {
			buildErr = err
			return
		}
		os.Chmod(dir, 0o755)
		binPath = filepath.Join(dir, "armageddon")
		out, err := exec.Command("go", "build", "-o", binPath, "github.com/callmehalpha/Armageddon-/cmd/armageddon").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// ---- fault-injecting proxy ----

var errDrop = errors.New("dropped by the fault proxy")

// proxy sits between the devices and the server. It drops responses after
// the server processed the request (the client sees a reset connection),
// delays responses, and can cut everyone off (partition).
type proxy struct {
	rng                   *rand.Rand
	mu                    sync.Mutex
	dropRate, delayRate   float64
	down                  atomic.Bool
	drops, delays, aborts atomic.Int64
	srv                   *http.Server
}

func (p *proxy) roll(rate float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rng.Float64() < rate
}

func (p *proxy) setFaults(drop, delay float64) {
	p.mu.Lock()
	p.dropRate, p.delayRate = drop, delay
	p.mu.Unlock()
}

func startProxy(t *testing.T, port, target int, seed int64) *proxy {
	p := &proxy{rng: rand.New(rand.NewSource(seed))}
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", target))
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	rp.ModifyResponse = func(r *http.Response) error {
		p.mu.Lock()
		drop, delay := p.dropRate, p.delayRate
		p.mu.Unlock()
		if p.roll(delay) {
			p.delays.Add(1)
			time.Sleep(time.Duration(200+rand.Intn(1300)) * time.Millisecond)
		}
		if p.roll(drop) {
			p.drops.Add(1)
			return errDrop
		}
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.aborts.Add(1)
		panic(http.ErrAbortHandler) // the client sees the connection die
	}
	p.srv = &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.down.Load() {
			panic(http.ErrAbortHandler)
		}
		rp.ServeHTTP(w, r)
	}), ErrorLog: nil}
	l, err := net.Listen("tcp", p.srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go p.srv.Serve(l)
	t.Cleanup(func() { p.srv.Close() })
	return p
}

// ---- the system under test ----

type device struct {
	name, cfg, data, replica, id string
	agent                        *exec.Cmd
	agentLog                     *os.File
}

type sut struct {
	t                  *testing.T
	rng                *rand.Rand
	bin, root, dataDir string
	port, pport        int
	srv                *exec.Cmd
	px                 *proxy
	http               *http.Client
	csrf, password, ws string
	devs               []*device
	next               int
	madeOn             map[string]string // token → seat that made it
	log                []string
	stats              map[string]int
}

func (s *sut) logf(f string, a ...any) {
	s.log = append(s.log, fmt.Sprintf("%s "+f, append([]any{time.Now().Format("15:04:05.000")}, a...)...))
}

func (s *sut) startServer() {
	logf, _ := os.OpenFile(filepath.Join(s.root, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd := exec.Command(s.bin, "server", "run", "--data", s.dataDir)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.srv = cmd
	for i := 0; i < 100; i++ {
		if r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", s.port)); err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatal("server did not start")
}

func (s *sut) killServer() {
	if s.srv != nil {
		s.srv.Process.Signal(syscall.SIGKILL)
		s.srv.Wait()
		s.srv = nil
	}
}

func (s *sut) api(method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", s.port, path), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrf)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		json.Unmarshal(b, out)
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode, nil
}

func (s *sut) run(d *device, timeout time.Duration, args ...string) (string, error) {
	cmd := exec.Command(s.bin, args...)
	cmd.Dir = d.replica
	if _, err := os.Stat(d.replica); err != nil {
		cmd.Dir = filepath.Dir(d.replica)
	}
	cmd.Env = append(os.Environ(), "ARMAGEDDON_CONFIG_DIR="+d.cfg, "ARMAGEDDON_DATA_DIR="+d.data)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		return out.String(), errors.New("timeout")
	}
}

func (s *sut) startAgent(d *device) {
	if d.agent != nil {
		return
	}
	cmd := exec.Command(s.bin, "agent", "run")
	cmd.Env = append(os.Environ(), "ARMAGEDDON_CONFIG_DIR="+d.cfg, "ARMAGEDDON_DATA_DIR="+d.data)
	cmd.Stdout, cmd.Stderr = d.agentLog, d.agentLog
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	d.agent = cmd
}

func (s *sut) killAgent(d *device) {
	if d.agent != nil {
		d.agent.Process.Signal(syscall.SIGKILL)
		d.agent.Wait()
		d.agent = nil
	}
}

func (s *sut) treeOf(seat string) string {
	if seat == "server" {
		return filepath.Join(s.dataDir, "workspaces", s.ws, "tree")
	}
	for _, d := range s.devs {
		if d.name == seat {
			return d.replica
		}
	}
	panic(seat)
}

// edit adds one token file on a seat.
func (s *sut) edit(seat string) {
	s.next++
	tok := fmt.Sprintf("t%04d", s.next)
	dir := filepath.Join(s.treeOf(seat), "tok")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, tok+".txt")
	if err := os.WriteFile(p, []byte(tok+" by "+seat+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	if seat == "server" && os.Geteuid() == 0 {
		// Isolated mode: files on the server seat belong to the workspace user.
		if fi, err := os.Stat(s.treeOf("server")); err == nil {
			st := fi.Sys().(*syscall.Stat_t)
			os.Lchown(dir, int(st.Uid), int(st.Gid))
			os.Lchown(p, int(st.Uid), int(st.Gid))
		}
	}
	s.madeOn[tok] = seat
	s.logf("edit %s on %s", tok, seat)
}

func newSUT(t *testing.T, seed int64) *sut {
	s := &sut{t: t, rng: rand.New(rand.NewSource(seed)), bin: buildBinary(t), madeOn: map[string]string{}, stats: map[string]int{}}
	root, err := os.MkdirTemp("", fmt.Sprintf("armageddon-conformance-%d-", seed))
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(root, 0o755)
	s.root = root
	t.Cleanup(func() {
		s.killServer()
		for _, d := range s.devs {
			s.killAgent(d)
		}
		if !t.Failed() {
			os.RemoveAll(root)
		} else {
			t.Logf("kept %s for inspection", root)
		}
	})
	s.dataDir = filepath.Join(root, "server")
	s.port, s.pport = freePort(t), freePort(t)
	s.px = startProxy(t, s.pport, s.port, seed)
	if out, err := exec.Command(s.bin, "server", "init", "--data", s.dataDir, "--listen", fmt.Sprintf("127.0.0.1:%d", s.port),
		"--public-url", fmt.Sprintf("http://127.0.0.1:%d", s.pport)).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	cfgPath := filepath.Join(s.dataDir, "server.json")
	var cfg map[string]any
	b, _ := os.ReadFile(cfgPath)
	json.Unmarshal(b, &cfg)
	cfg["lease_stale_ms"], cfg["handoff_timeout_ms"], cfg["capture_interval_ms"] = 8000, 8000, 1000
	b, _ = json.Marshal(cfg)
	os.WriteFile(cfgPath, b, 0o600)
	s.startServer()
	time.Sleep(300 * time.Millisecond)
	logb, _ := os.ReadFile(filepath.Join(root, "server.log"))
	m := regexp.MustCompile(`setup\?token=([a-z0-9]+)`).FindSubmatch(logb)
	if m == nil {
		t.Fatal("no setup token")
	}
	jar, _ := cookiejar.New(nil)
	s.http = &http.Client{Jar: jar, Timeout: 60 * time.Second}
	s.password = fmt.Sprintf("pw-%d-%d", seed, time.Now().UnixNano())
	var setup struct {
		CSRF string `json:"csrf"`
	}
	if _, err := s.api("POST", "/api/setup", map[string]string{"token": string(m[1]), "username": "admin", "password": s.password}, &setup); err != nil {
		t.Fatal(err)
	}
	s.csrf = setup.CSRF
	var w struct {
		ID string `json:"id"`
	}
	if _, err := s.api("POST", "/api/workspaces", map[string]string{"name": "conf"}, &w); err != nil {
		t.Fatal(err)
	}
	s.ws = w.ID
	for i := 0; i < 100; i++ {
		var ws struct {
			State string `json:"state"`
		}
		s.api("GET", "/api/workspaces/"+s.ws, nil, &ws)
		if ws.State == "ready" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for i, name := range []string{"d1", "d2"} {
		d := &device{name: name, cfg: filepath.Join(root, name, "cfg"), data: filepath.Join(root, name, "data"), replica: filepath.Join(root, name, "replica")}
		os.MkdirAll(filepath.Join(root, name), 0o755)
		d.agentLog, _ = os.Create(filepath.Join(root, name, "agent.log"))
		s.devs = append(s.devs, d)
		cmd := exec.Command(s.bin, "login", fmt.Sprintf("http://127.0.0.1:%d", s.pport), "--name", name)
		cmd.Env = append(os.Environ(), "ARMAGEDDON_CONFIG_DIR="+d.cfg, "ARMAGEDDON_DATA_DIR="+d.data)
		var out syncBuffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var code string
		for j := 0; j < 100 && code == ""; j++ {
			if m := regexp.MustCompile(`code=([A-Z]+-[A-Z]+)`).FindStringSubmatch(out.String()); m != nil {
				code = m[1]
			}
			time.Sleep(100 * time.Millisecond)
		}
		if _, err := s.api("POST", "/api/pair/approve", map[string]string{"code": code}, nil); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("login %s: %v %s", name, err, out.String())
		}
		var devs []struct{ ID, Name string }
		s.api("GET", "/api/devices", nil, &devs)
		for _, x := range devs {
			if x.Name == name {
				d.id = x.ID
			}
		}
		if out, err := s.run(d, time.Minute, "clone", "conf", d.replica); err != nil {
			t.Fatalf("clone %s: %v %s", name, err, out)
		}
		_ = i
		s.startAgent(d)
	}
	return s
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// ---- the scenario generator (after prototypes/p4-failure-model) ----

func (s *sut) step() {
	r := s.rng.Float64()
	d := s.devs[s.rng.Intn(len(s.devs))]
	switch {
	case r < 0.40: // a user edits some seat (server edits while a device writes are drift)
		if s.rng.Float64() < 0.3 {
			s.edit("server")
		} else {
			s.edit(d.name)
		}
		s.stats["edit"]++
	case r < 0.52: // armageddon work local
		s.stats["work local"]++
		args := []string{"work", "local"}
		if s.rng.Float64() < 0.15 {
			args = append(args, "--force")
			s.stats["force (device)"]++
		}
		s.goCLI(d, args...)
	case r < 0.60: // armageddon work remote (from any device)
		s.stats["work remote"]++
		args := []string{"work", "remote"}
		if s.rng.Float64() < 0.2 {
			args = append(args, "--force")
			s.stats["force (cli)"]++
		}
		s.goCLI(d, args...)
	case r < 0.64: // forced takeover from the browser, with password re-auth
		s.stats["force (browser)"]++
		s.api("POST", "/api/workspaces/"+s.ws+"/lease/force", map[string]string{"to": "server", "password": s.password}, nil)
		s.logf("browser forced takeover")
	case r < 0.74: // kill -9 an agent, shortly after an edit or a handoff request
		if d.agent != nil {
			time.Sleep(time.Duration(s.rng.Intn(2500)) * time.Millisecond)
			s.killAgent(d)
			s.stats["agent kill -9"]++
			s.logf("kill -9 agent %s", d.name)
		} else {
			s.startAgent(d)
			s.logf("restart agent %s", d.name)
		}
	case r < 0.77: // kill -9 the server mid-commit / mid-handoff
		time.Sleep(time.Duration(s.rng.Intn(1500)) * time.Millisecond)
		s.killServer()
		s.stats["server kill -9"]++
		s.logf("kill -9 server")
		time.Sleep(time.Duration(s.rng.Intn(1500)) * time.Millisecond)
		s.startServer()
	case r < 0.85: // network: drops, delays, partition
		switch s.rng.Intn(3) {
		case 0:
			s.px.setFaults(0.15, 0.15)
		case 1:
			s.px.setFaults(0, 0)
		case 2:
			s.px.down.Store(!s.px.down.Load())
			s.stats["partition toggle"]++
		}
	default:
		time.Sleep(time.Duration(s.rng.Intn(2000)) * time.Millisecond)
	}
	time.Sleep(time.Duration(100+s.rng.Intn(600)) * time.Millisecond)
}

var cliWG sync.WaitGroup

// goCLI runs a CLI command concurrently: users do not wait for each other.
func (s *sut) goCLI(d *device, args ...string) {
	s.logf("%s: armageddon %s", d.name, strings.Join(args, " "))
	cliWG.Add(1)
	go func() {
		defer cliWG.Done()
		s.run(d, 60*time.Second, args...)
	}()
}

// ---- convergence and the checker ----

type replicaState struct {
	AppliedOid string `json:"applied_oid"`
	Epoch      int64  `json:"epoch"`
	Pending    []any  `json:"pending"`
	Outbox     []any  `json:"outbox"`
}

type leaseView struct {
	Holder string `json:"holder"`
	Epoch  int64  `json:"epoch"`
	State  string `json:"state"`
}

type cur struct {
	Seq   int64     `json:"seq"`
	ID    string    `json:"id"`
	Lease leaseView `json:"lease"`
}

func (s *sut) gitOut(args ...string) string {
	out, _ := exec.Command("git", append([]string{"--git-dir=" + filepath.Join(s.dataDir, "workspaces", s.ws, "checkpoints.git")}, args...)...).Output()
	return string(out)
}

func tokensIn(names string) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(names, "\n") {
		if b, ok := strings.CutPrefix(l, "tok/"); ok {
			m[strings.TrimSuffix(b, ".txt")] = true
		}
	}
	return m
}

func (s *sut) cpTokens(cp string) map[string]bool {
	if cp == "" {
		return map[string]bool{}
	}
	return tokensIn(s.gitOut("ls-tree", "-r", "--name-only", cp+":worktree"))
}

func diskTokens(dir string) map[string]bool {
	m := map[string]bool{}
	ents, _ := os.ReadDir(filepath.Join(dir, "tok"))
	for _, e := range ents {
		m[strings.TrimSuffix(e.Name(), ".txt")] = true
	}
	return m
}

func subset(a, b map[string]bool) []string {
	var miss []string
	for k := range a {
		if !b[k] {
			miss = append(miss, k)
		}
	}
	sort.Strings(miss)
	return miss
}

// converge stops the faults and waits until the system is quiescent.
func (s *sut) converge(timeout time.Duration) error {
	cliWG.Wait()
	s.px.setFaults(0, 0)
	s.px.down.Store(false)
	if s.srv == nil {
		s.startServer()
	}
	for _, d := range s.devs {
		s.startAgent(d)
	}
	deadline := time.Now().Add(timeout)
	why := ""
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		var c cur
		if _, err := s.api("GET", "/api/workspaces/"+s.ws+"/current", nil, &c); err != nil {
			why = err.Error()
			continue
		}
		if c.Lease.State == "handoff" {
			why = "handoff in progress"
			continue
		}
		curToks := s.cpTokens(c.ID)
		ok := true
		for _, d := range s.devs {
			var st replicaState
			b, err := os.ReadFile(filepath.Join(d.data, "replicas", s.ws, "state.json"))
			if err != nil || json.Unmarshal(b, &st) != nil {
				ok, why = false, d.name+": no state"
				break
			}
			if len(st.Pending) > 0 || len(st.Outbox) > 0 {
				ok, why = false, fmt.Sprintf("%s: %d pending, %d in outbox", d.name, len(st.Pending), len(st.Outbox))
				break
			}
			holder := c.Lease.Holder == "device:"+d.id
			if holder && (st.Epoch != c.Lease.Epoch || len(subset(diskTokens(d.replica), curToks)) > 0) {
				ok, why = false, d.name+": writer not caught up"
				break
			}
			if !holder && st.Epoch != 0 {
				ok, why = false, d.name+": still believes it writes"
				break
			}
		}
		if ok && c.Lease.Holder == "server" && len(subset(diskTokens(s.treeOf("server")), curToks)) > 0 {
			ok, why = false, "server seat not captured yet"
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("liveness: no convergence after faults stopped: %s", why)
}

func (s *sut) check() error {
	var c cur
	if _, err := s.api("GET", "/api/workspaces/"+s.ws+"/current", nil, &c); err != nil {
		return err
	}
	current := s.cpTokens(c.ID)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(s.dataDir, "armageddon.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	// I1: epoch → the seat granted it (epoch 1: the server seat).
	granted := map[int64]string{1: "server"}
	rows, err := db.Query(`SELECT type, payload FROM events WHERE workspace_id = ? AND type IN ('lease.granted', 'lease.forced') ORDER BY ts`, s.ws)
	if err != nil {
		return err
	}
	for rows.Next() {
		var typ, payload string
		rows.Scan(&typ, &payload)
		var p struct {
			To    string `json:"to"`
			Epoch int64  `json:"epoch"`
		}
		json.Unmarshal([]byte(payload), &p)
		if g, ok := granted[p.Epoch]; ok && g != p.To {
			rows.Close()
			return fmt.Errorf("I1: epoch %d granted to %s and %s", p.Epoch, g, p.To)
		}
		granted[p.Epoch] = p.To
	}
	rows.Close()
	rows, err = db.Query(`SELECT id, seq, epoch, author_kind, COALESCE(author_device_id, '') FROM checkpoints WHERE workspace_id = ? ORDER BY seq`, s.ws)
	if err != nil {
		return err
	}
	type cpRow struct {
		id                   string
		seq, epoch           int64
		kind, dev, authorStr string
	}
	var cps []cpRow
	for rows.Next() {
		var r cpRow
		rows.Scan(&r.id, &r.seq, &r.epoch, &r.kind, &r.dev)
		r.authorStr = r.kind
		if r.kind == "device" {
			r.authorStr = "device:" + r.dev
		}
		cps = append(cps, r)
	}
	rows.Close()
	for _, r := range cps {
		if granted[r.epoch] != r.authorStr {
			return fmt.Errorf("I1: checkpoint #%d by %s under epoch %d, granted to %q", r.seq, r.authorStr, r.epoch, granted[r.epoch])
		}
		// I3: every acknowledged checkpoint's work is in current.
		if miss := subset(s.cpTokens(r.id), current); len(miss) > 0 {
			return fmt.Errorf("I3: tokens %v of acknowledged checkpoint #%d missing from current #%d", miss, r.seq, c.Seq)
		}
	}
	s.stats["checkpoints"] += len(cps)
	// I4: every token is in current, a quarantine, or its seat's disk.
	safe := map[string]bool{}
	for k := range current {
		safe[k] = true
	}
	qrefs := strings.Fields(s.gitOut("for-each-ref", "--format=%(objectname)", "refs/quarantine/"))
	s.stats["quarantines"] += len(qrefs)
	for _, q := range qrefs {
		for k := range s.cpTokens(q) {
			safe[k] = true
		}
	}
	var lost []string
	for tok, seat := range s.madeOn {
		if safe[tok] || diskTokens(s.treeOf(seat))[tok] {
			continue
		}
		lost = append(lost, tok+"@"+seat)
	}
	sort.Strings(lost)
	if len(lost) > 0 {
		return fmt.Errorf("I4: tokens lost (not in current, any quarantine, or their seat's disk): %v", lost)
	}
	local := 0
	for tok := range s.madeOn {
		if !safe[tok] {
			local++
		}
	}
	s.stats["tokens left local on dirty followers"] += local
	return nil
}

func TestConformance(t *testing.T) {
	runs := envInt("CONFORMANCE_RUNS", 0)
	if runs == 0 {
		t.Skip("set CONFORMANCE_RUNS (and CONFORMANCE_STEPS) to run the protocol conformance suite")
	}
	steps := envInt("CONFORMANCE_STEPS", 30)
	base := int64(envInt("CONFORMANCE_SEED", int(time.Now().Unix()%100000)))
	total := map[string]int{}
	for i := 0; i < runs; i++ {
		seed := base + int64(i)
		ok := t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			s := newSUT(t, seed)
			for j := 0; j < steps; j++ {
				s.step()
			}
			err := s.converge(3 * time.Minute)
			if err == nil {
				err = s.check()
			}
			if err != nil {
				t.Logf("history:\n%s", strings.Join(s.log, "\n"))
				t.Fatalf("seed %d: %v", seed, err)
			}
			s.stats["drops"] += int(s.px.drops.Load())
			s.stats["delays"] += int(s.px.delays.Load())
			for k, v := range s.stats {
				total[k] += v
			}
		})
		if !ok {
			break
		}
	}
	var keys []string
	for k := range total {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%d ", k, total[k])
	}
	t.Logf("%d runs × %d steps: %s", runs, steps, b.String())
}
