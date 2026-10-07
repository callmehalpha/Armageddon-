package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/runtimes"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

// Runtimes, Compose and ports (plan M8): the API behind `armageddon
// runtime|compose|ports` and the workspace page.

func (s *Server) runtimeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workspaces/{id}/runtime", s.requireUser(s.member(s.handleRuntime)))
	mux.HandleFunc("POST /api/workspaces/{id}/runtime/install", s.requireUser(s.member(s.handleRuntimeInstall)))
	mux.HandleFunc("POST /api/workspaces/{id}/runtime/start", s.requireUser(s.member(s.handleRuntimeStart)))
	mux.HandleFunc("POST /api/workspaces/{id}/runtime/stop", s.requireUser(s.member(s.handleRuntimeStop)))
	mux.HandleFunc("GET /api/workspaces/{id}/runtime/logs", s.requireUser(s.member(s.handleRuntimeLogs)))
	mux.HandleFunc("GET /api/workspaces/{id}/compose", s.requireUser(s.member(s.handleComposePs)))
	mux.HandleFunc("POST /api/workspaces/{id}/compose/up", s.requireUser(s.member(s.handleComposeUp)))
	mux.HandleFunc("POST /api/workspaces/{id}/compose/down", s.requireUser(s.member(s.handleComposeDown)))
	mux.HandleFunc("GET /api/workspaces/{id}/ports", s.requireUser(s.member(s.handlePorts)))
	// The port proxy: every method and subpath, after the same user and
	// membership checks as the IDE.
	mux.HandleFunc("/api/workspaces/{id}/ports/{port}/", s.requireUser(s.member(s.handlePortProxy)))
	mux.HandleFunc("/api/workspaces/{id}/ports/{port}", s.requireUser(s.member(s.handlePortProxy)))
}

// runtimeEnv passes the download locations to `hook runtime`.
func (s *Server) runtimeEnv() []string {
	var env []string
	if v := s.cfg.Runtimes.NodeMirror; v != "" {
		env = append(env, "ARMAGEDDON_NODE_MIRROR="+v)
	}
	if v := s.cfg.Runtimes.ComposerURL; v != "" {
		env = append(env, "ARMAGEDDON_COMPOSER_URL="+v)
	}
	return env
}

// runtimePlan runs `hook runtime plan` as the workspace user.
func (s *Server) runtimePlan(ctx context.Context, rt *runtime) (*runtimes.Plan, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := rt.acct.CommandContext(ctx, rt.p.Tree, s.hookBin, "hook", "runtime", "plan", rt.p.Tree)
	cmd.Env = append(cmd.Env, s.runtimeEnv()...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		msg = strings.TrimPrefix(msg, "armageddon: ")
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	var p runtimes.Plan
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("runtime plan: %w", err)
	}
	return &p, nil
}

func (s *Server) handleRuntime(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	out := map[string]any{"server_holds_lease": s.serverHoldsLease(w.ID)}
	if p, err := s.runtimePlan(r.Context(), rt); err != nil {
		out["plan_error"] = err.Error()
	} else {
		out["plan"] = p
	}
	procs := []devProcInfo{}
	for _, p := range s.procs.list(w.ID) {
		info := p.info()
		if info.Name == procDev && info.State == "running" && info.Port > 0 {
			info.Health = probeHealth(r.Context(), info.Port, healthPath(out["plan"]))
		}
		procs = append(procs, info)
	}
	out["processes"] = procs
	out["ports"] = s.workspacePorts(r.Context(), rt)
	writeJSON(rw, 200, out)
}

func healthPath(plan any) string {
	if p, ok := plan.(*runtimes.Plan); ok && p.HealthPath != "" {
		return p.HealthPath
	}
	return "/"
}

// probeHealth asks the dev server for its health path: any HTTP answer
// means it serves.
func probeHealth(ctx context.Context, port int, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "unresponsive"
		}
		return "starting"
	}
	resp.Body.Close()
	return "healthy"
}

// seatProcRuntime is the runtime of a request that starts server-seat
// processes: the workspace must be running and the server must hold the
// lease (§4.3: dev processes run only on the writing seat).
func (s *Server) seatProcRuntime(rw http.ResponseWriter, w *store.Workspace) *runtime {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return nil
	}
	if !s.serverHoldsLease(w.ID) {
		writeErr(rw, 409, leaseRefusal+" (dev processes run on the seat that writes)")
		return nil
	}
	return rt
}

func (s *Server) handleRuntimeInstall(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.seatProcRuntime(rw, w)
	if rt == nil {
		return
	}
	argv := []string{s.hookBin, "hook", "runtime", "install", rt.p.Tree}
	p, err := s.procs.start(rt, w, procInstall, argv, s.runtimeEnv(), 0)
	if err != nil {
		writeProcErr(rw, err)
		return
	}
	kind, id := actorOf(r)
	s.event(w.ID, kind, id, "runtime.install", nil)
	writeJSON(rw, 202, p.info())
}

func (s *Server) handleRuntimeStart(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.seatProcRuntime(rw, w)
	if rt == nil {
		return
	}
	var req struct {
		Port int `json:"port"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(rw, 400, "bad request")
		return
	}
	if req.Port < 0 || req.Port > 65535 || (req.Port > 0 && req.Port < 1024) {
		writeErr(rw, 400, "port must be between 1024 and 65535")
		return
	}
	plan, err := s.runtimePlan(r.Context(), rt)
	if err != nil {
		writeErr(rw, 400, err.Error())
		return
	}
	if len(plan.Start) == 0 {
		writeErr(rw, 400, "nothing to start: "+strings.Join(plan.Notes, "; "))
		return
	}
	if src := plan.Toolchain.Source; src == "install" || src == "missing" {
		writeErr(rw, 409, fmt.Sprintf("%s %s is not installed yet: run the install first (`armageddon runtime install`)", plan.Toolchain.Name, plan.Toolchain.Version))
		return
	}
	port := plan.Port
	if req.Port > 0 {
		port = req.Port
	}
	argv := []string{s.hookBin, "hook", "runtime", "exec", "--port", strconv.Itoa(port), rt.p.Tree}
	p, err := s.procs.start(rt, w, procDev, argv, s.runtimeEnv(), port)
	if err != nil {
		writeProcErr(rw, err)
		return
	}
	writeJSON(rw, 202, p.info())
}

func writeProcErr(rw http.ResponseWriter, err error) {
	if errors.Is(err, ErrProcRunning) {
		writeErr(rw, 409, err.Error())
		return
	}
	writeErr(rw, 500, err.Error())
}

func (s *Server) handleRuntimeStop(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(rw, 400, "bad request")
		return
	}
	if req.Name == "" {
		req.Name = procDev
	}
	if req.Name != procDev && req.Name != procInstall {
		writeErr(rw, 400, "name must be dev or install")
		return
	}
	if err := s.procs.stop(w.ID, req.Name, "user"); err != nil {
		writeErr(rw, 404, err.Error())
		return
	}
	writeJSON(rw, 200, s.procs.get(w.ID, req.Name).info())
}

// handleRuntimeLogs returns a process's output from ?offset= on, and the
// offset to ask for next. With ?wait=1 it waits up to 20 s for new output
// while the process runs (long poll).
func (s *Server) handleRuntimeLogs(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = procDev
	}
	p := s.procs.get(w.ID, name)
	if p == nil {
		writeErr(rw, 404, "no "+name+" process has run since the server started")
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	data, next := p.log.readFrom(offset)
	if len(data) == 0 && r.URL.Query().Get("wait") == "1" {
		deadline := time.After(20 * time.Second)
	wait:
		for p.running() {
			select {
			case <-r.Context().Done():
				return
			case <-deadline:
				break wait
			case <-p.done:
			case <-time.After(250 * time.Millisecond):
			}
			if data, next = p.log.readFrom(offset); len(data) > 0 {
				break
			}
		}
		data, next = p.log.readFrom(offset)
	}
	info := p.info()
	writeJSON(rw, 200, map[string]any{"data": string(data), "offset": next, "process": info})
}

// ---- Compose ----

func (s *Server) handleComposePs(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	if s.readyRuntime(rw, w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	svcs, err := s.helper.ComposePs(ctx, w.ID)
	if err != nil {
		writeErr(rw, 502, err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{"project": helper.ComposeProject(w.ID), "services": svcs})
}

func (s *Server) handleComposeUp(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	if s.readyRuntime(rw, w) == nil {
		return
	}
	var req struct {
		File string `json:"file"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(rw, 400, "bad request")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 25*time.Minute)
	defer cancel()
	kind, id := actorOf(r)
	warn, err := s.helper.ComposeUp(ctx, w.ID, helper.ComposeArgs{File: req.File})
	s.invalidateCompose(w.ID)
	if err != nil {
		s.event(w.ID, kind, id, "compose.refused", map[string]any{"file": req.File, "error": err.Error()})
		writeErr(rw, 400, strings.TrimPrefix(err.Error(), "helper ComposeUp: "))
		return
	}
	s.event(w.ID, kind, id, "compose.up", map[string]any{"file": req.File})
	svcs, _ := s.helper.ComposePs(ctx, w.ID)
	out := map[string]any{"project": helper.ComposeProject(w.ID), "services": svcs}
	if warn != "" {
		out["warnings"] = strings.Split(warn, "\n")
	}
	writeJSON(rw, 200, out)
}

func (s *Server) handleComposeDown(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	if s.readyRuntime(rw, w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Minute)
	defer cancel()
	err := s.helper.ComposeDown(ctx, w.ID)
	s.invalidateCompose(w.ID)
	if err != nil {
		writeErr(rw, 502, err.Error())
		return
	}
	kind, id := actorOf(r)
	s.event(w.ID, kind, id, "compose.down", nil)
	writeJSON(rw, 200, map[string]any{"project": helper.ComposeProject(w.ID)})
}

// ---- ports and the port proxy (M8.6) ----

func (s *Server) handlePorts(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	ports := s.workspacePorts(r.Context(), rt)
	for i := range ports {
		ports[i].Path = s.cfg.PublicURL + ports[i].Path
	}
	writeJSON(rw, 200, ports)
}

var portPathRe = regexp.MustCompile(`^/api/workspaces/[^/]+/ports/[0-9]+(/|$)`)

func isPortPath(p string) bool { return portPathRe.MatchString(p) }

// handlePortProxy serves /api/workspaces/{id}/ports/{port}/… : an HTTP (and
// WebSocket) reverse proxy to a port the workspace listens on, for its
// members only. Armageddon's own credentials never reach the app.
func (s *Server) handlePortProxy(rw http.ResponseWriter, r *http.Request, w *store.Workspace, role string) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port <= 0 || port > 65535 {
		writeErr(rw, 404, "no such port")
		return
	}
	prefix := "/api/workspaces/" + w.ID + "/ports/" + strconv.Itoa(port)
	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if rest == r.URL.EscapedPath() || !strings.HasPrefix(rest, "/") {
		http.Redirect(rw, r, prefix+"/", http.StatusFound)
		return
	}
	if isWebSocketUpgrade(r) && sessionOf(r) != nil && !sameOrigin(r) {
		writeErr(rw, 403, "cross-origin WebSocket refused")
		return
	}
	rt := s.readyRuntime(rw, w)
	if rt == nil {
		return
	}
	var target *wsPort
	for _, p := range s.workspacePorts(r.Context(), rt) {
		if p.Port == port {
			target = &p
			break
		}
	}
	if target == nil {
		writeErr(rw, 404, fmt.Sprintf("nothing in this workspace listens on port %d", port))
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			setBackendURL(pr.Out.URL, rest, pr.In.URL.RawQuery)
			pr.Out.URL.Host = target.dial
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			scrubIDERequest(pr.Out.Header)
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			scrubIDEResponse(resp.Header, prefix)
			// Apps are never framed: the response keeps Armageddon's DENY.
			resp.Header.Del("X-Frame-Options")
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			writeErr(rw, 502, fmt.Sprintf("port %d is not answering: %v", port, err))
		},
	}
	proxy.ServeHTTP(rw, r)
}
