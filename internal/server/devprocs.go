package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Server dev processes (plan M8.5, decision Q1). The runtime install and
// the dev server run as the workspace user through the helper
// (SpawnInWorkspace, kind runtime-command), started as `armageddon hook
// runtime install|exec <tree>`. The server keeps their state and the tail of
// their output, nothing else.
//
// When the lease leaves the server seat, Quiesce stops every workspace
// process (SignalWorkspace(all)), dev processes included; Compose services
// are the Docker daemon's, not the workspace user's, so they keep running.
// The dev processes stopped that way are remembered, and `armageddon work
// remote --restart` starts them again once the server holds the lease.

const (
	devLogSize   = 1 << 20 // bytes of output kept per process
	devStopGrace = 5 * time.Second
	// Process names.
	procInstall = "install"
	procDev     = "dev"
)

type devProcs struct {
	s    *Server
	mu   sync.Mutex
	byWS map[string]map[string]*devProc
}

func newDevProcs(s *Server) *devProcs { return &devProcs{s: s, byWS: map[string]map[string]*devProc{}} }

// devProc is one managed process.
type devProc struct {
	name string
	argv []string
	env  []string
	port int

	mu        sync.Mutex
	state     string // running, exited, stopped
	stoppedBy string // "", "user", "handoff"
	startedAt int64
	endedAt   int64
	exit      *helper.ExitStatus
	err       string
	proc      helper.Process
	done      chan struct{}
	log       logRing
}

// devProcInfo is a process as the API shows it.
type devProcInfo struct {
	Name      string   `json:"name"`
	Argv      []string `json:"argv"`
	State     string   `json:"state"`
	StoppedBy string   `json:"stopped_by,omitempty"`
	Port      int      `json:"port,omitempty"`
	StartedAt int64    `json:"started_at"`
	EndedAt   int64    `json:"ended_at,omitempty"`
	ExitCode  *int     `json:"exit_code,omitempty"`
	Error     string   `json:"error,omitempty"`
	LogSize   int64    `json:"log_size"`
	Health    string   `json:"health,omitempty"` // dev only: healthy, starting, down
}

func (p *devProc) info() devProcInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := devProcInfo{Name: p.name, Argv: p.argv, State: p.state, StoppedBy: p.stoppedBy, Port: p.port,
		StartedAt: p.startedAt, EndedAt: p.endedAt, Error: p.err, LogSize: p.log.size()}
	if p.exit != nil {
		c := p.exit.ShellCode()
		out.ExitCode = &c
	}
	return out
}

func (p *devProc) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == "running"
}

// ErrProcRunning is returned when a process of that name already runs.
var ErrProcRunning = errors.New("already running")

// start runs argv in the workspace's tree as the workspace user, under
// name. env is added to the workspace's base environment.
func (m *devProcs) start(rt *runtime, w *store.Workspace, name string, argv []string, env []string, port int) (*devProc, error) {
	m.mu.Lock()
	procs := m.byWS[rt.id]
	if procs == nil {
		procs = map[string]*devProc{}
		m.byWS[rt.id] = procs
	}
	if old := procs[name]; old != nil && old.running() {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", name, ErrProcRunning)
	}
	p := &devProc{name: name, argv: argv, env: env, port: port, state: "running", startedAt: store.Now(), done: make(chan struct{})}
	procs[name] = p
	m.mu.Unlock()

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, m.failStart(p, err)
	}
	fullEnv := append(append(rt.acct.BaseEnv(), seatEnv(rt, w, nil)...), env...)
	proc, err := m.s.helper.Spawn(context.Background(), rt.id, helper.SpawnSpec{Kind: helper.KindRuntimeCommand,
		Argv: argv, Env: fullEnv, Dir: rt.p.Tree, Stdout: pw, Stderr: pw})
	pw.Close()
	if err != nil {
		pr.Close()
		return nil, m.failStart(p, err)
	}
	p.mu.Lock()
	p.proc = proc
	p.mu.Unlock()
	m.s.event(rt.id, "server", "", "runtime.started", map[string]any{"name": name, "argv": argv})
	go func() {
		io.Copy(&p.log, pr)
		pr.Close()
	}()
	go func() {
		st, err := proc.Wait()
		p.mu.Lock()
		p.endedAt = store.Now()
		p.exit = &st
		if err != nil {
			p.err = err.Error()
		}
		if p.stoppedBy != "" {
			p.state = "stopped"
		} else {
			p.state = "exited"
		}
		p.proc = nil
		by := p.stoppedBy
		p.mu.Unlock()
		close(p.done)
		m.s.event(rt.id, "server", "", "runtime.exited", map[string]any{"name": name, "exit_code": st.ShellCode(), "stopped_by": by})
	}()
	return p, nil
}

func (m *devProcs) failStart(p *devProc, err error) error {
	p.mu.Lock()
	p.state, p.err, p.endedAt = "exited", err.Error(), store.Now()
	p.mu.Unlock()
	close(p.done)
	return err
}

func (m *devProcs) get(wsID, name string) *devProc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byWS[wsID][name]
}

// list returns the workspace's processes, install first.
func (m *devProcs) list(wsID string) []*devProc {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*devProc
	for _, n := range []string{procInstall, procDev} {
		if p := m.byWS[wsID][n]; p != nil {
			out = append(out, p)
		}
	}
	return out
}

// stop ends a process: SIGTERM to its process group, then SIGKILL after
// the grace period. by is recorded ("user", "handoff").
func (m *devProcs) stop(wsID, name, by string) error {
	p := m.get(wsID, name)
	if p == nil {
		return fmt.Errorf("no %s process", name)
	}
	p.mu.Lock()
	proc := p.proc
	if p.state != "running" || proc == nil {
		p.mu.Unlock()
		return nil
	}
	p.stoppedBy = by
	p.mu.Unlock()
	proc.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(devStopGrace):
		proc.Signal(syscall.SIGKILL)
		<-p.done
	}
	return nil
}

// markHandoff records that the running processes are about to be stopped
// by a handoff (Quiesce stops them all through the helper).
func (m *devProcs) markHandoff(wsID string) []string {
	var names []string
	for _, p := range m.list(wsID) {
		p.mu.Lock()
		if p.state == "running" {
			p.stoppedBy = "handoff"
			names = append(names, p.name)
		}
		p.mu.Unlock()
	}
	return names
}

// restartAfterHandoff starts again the dev server that a handoff stopped
// (`work remote --restart`). An interrupted install is not repeated: it
// is reported, and `armageddon runtime install` runs it again.
func (m *devProcs) restartAfterHandoff(rt *runtime, w *store.Workspace) ([]string, error) {
	p := m.get(rt.id, procDev)
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	stopped := p.stoppedBy == "handoff" && p.state != "running"
	argv, env, port := p.argv, p.env, p.port
	p.mu.Unlock()
	if !stopped {
		return nil, nil
	}
	if _, err := m.start(rt, w, procDev, argv, env, port); err != nil {
		return nil, err
	}
	return []string{procDev}, nil
}

// ---- output ----

// logRing keeps the last devLogSize bytes of a process's output, with
// absolute offsets so that readers can poll for what is new.
type logRing struct {
	mu    sync.Mutex
	buf   []byte
	total int64 // bytes ever written
}

func (l *logRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > devLogSize {
		l.buf = append([]byte(nil), l.buf[len(l.buf)-devLogSize:]...)
	}
	l.total += int64(len(p))
	return len(p), nil
}

func (l *logRing) size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// readFrom returns the output from offset on (from the oldest kept byte if
// offset is older), and the offset to read from next.
func (l *logRing) readFrom(offset int64) ([]byte, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := l.total - int64(len(l.buf))
	if offset < first || offset < 0 {
		offset = first
	}
	if offset > l.total {
		offset = l.total
	}
	return append([]byte(nil), l.buf[offset-first:]...), l.total
}
