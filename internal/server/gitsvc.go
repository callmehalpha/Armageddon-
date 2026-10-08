package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/callmehalpha/Armageddon-/internal/store"
)

// serveGit implements Git smart HTTP (protocol v0/v1) natively (P-6): it
// spawns `git upload-pack|receive-pack --stateless-rpc` as the workspace
// user. CGI is not used: Go's net/http/cgi rejects chunked request bodies,
// which git sends for every push over http.postBuffer.
//
// URL layout: /git/<workspace-id>.git/{info/refs,git-upload-pack,git-receive-pack}
func (s *Server) serveGit(rw http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 || !strings.HasSuffix(rest[:slash], ".git") {
		http.NotFound(rw, r)
		return
	}
	wsID, sub := strings.TrimSuffix(rest[:slash], ".git"), rest[slash+1:]

	dev := s.deviceFromRequest(r)
	if dev == nil {
		rw.Header().Set("WWW-Authenticate", `Basic realm="armageddon"`)
		http.Error(rw, "authentication required (run `armageddon login`)", http.StatusUnauthorized)
		return
	}
	w, _, err := s.store.WorkspaceForMember(wsID, dev.UserID)
	if err != nil || !running(w) {
		http.NotFound(rw, r)
		return
	}
	rt := s.runtimeFor(w.ID)
	if rt == nil {
		http.Error(rw, "workspace not running", http.StatusServiceUnavailable)
		return
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		service = sub
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(rw, "unsupported service", http.StatusForbidden)
		return
	}
	if service == "git-receive-pack" {
		switch reason := s.writesRefused(w); reason {
		case "":
		case ReasonDiskFull:
			http.Error(rw, "disk_full: the server's disk is almost full; pushes are refused until its administrator frees space and runs `armageddon doctor --repair` (F8). Your commits stay on this machine.", http.StatusInsufficientStorage)
			return
		default:
			http.Error(rw, reason+": the workspace is DEGRADED and refuses pushes until it is repaired. Your commits stay on this machine.", http.StatusServiceUnavailable)
			return
		}
		epoch, _ := strconv.ParseInt(r.Header.Get("X-Armageddon-Epoch"), 10, 64)
		if r.Method == http.MethodPost {
			// Fencing (§4.2): hold the fence for the whole receive-pack, and
			// check the lease under it.
			rt.fence.RLock()
			defer rt.fence.RUnlock()
		}
		lease, err := s.store.LeaseOf(nil, w.ID)
		if err != nil {
			http.Error(rw, "lease unavailable", http.StatusInternalServerError)
			return
		}
		if lease.HolderKind != "device" || lease.HolderDevice != dev.ID || lease.Epoch != epoch {
			holder := "the server seat"
			if lease.HolderKind == "device" {
				holder = "device " + lease.HolderDevice
			}
			http.Error(rw, fmt.Sprintf("lease_lost: this workspace is written by %s (epoch %d). Replicas are read-only; commit on the server seat or push to your own remote.", holder, lease.Epoch), http.StatusConflict)
			return
		}
	}
	// Server-owned settings: GIT_CONFIG_* outranks the workspace-writable
	// repository config (§2.5, P5 S10).
	cfg := [][2]string{
		{"core.hooksPath", rt.p.Hooks},
		{"receive.denyCurrentBranch", "ignore"},
		// The writer mirrors its refs (§5.3): it may delete the branch the
		// server worktree has checked out; the next checkpoint moves HEAD.
		{"receive.denyDeleteCurrent", "ignore"},
		{"receive.fsckObjects", "true"},
		{"uploadpack.hideRefs", "refs/armageddon"},
		{"receive.hideRefs", "refs/armageddon"},
		{"uploadpack.allowAnySHA1InWant", "false"},
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)), "ARMAGEDDON_PUSH=1"}
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	verb := strings.TrimPrefix(service, "git-")
	switch {
	case r.Method == http.MethodGet && sub == "info/refs":
		cmd := rt.acct.Command(rt.p.Repo, "git", verb, "--stateless-rpc", "--advertise-refs", rt.p.Repo)
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.Output()
		if err != nil {
			http.Error(rw, "advertise failed", http.StatusInternalServerError)
			return
		}
		rw.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		rw.Header().Set("Cache-Control", "no-cache")
		hdr := "# service=" + service + "\n"
		fmt.Fprintf(rw, "%04x%s0000", len(hdr)+4, hdr)
		rw.Write(out)
	case r.Method == http.MethodPost && sub == service:
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(rw, "bad gzip body", http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = gz
		}
		cmd := rt.acct.Command(rt.p.Repo, "git", verb, "--stateless-rpc", rt.p.Repo)
		cmd.Env = append(cmd.Env, env...)
		cmd.Stdin = body
		rw.Header().Set("Content-Type", "application/x-"+service+"-result")
		rw.Header().Set("Cache-Control", "no-cache")
		cmd.Stdout = rw
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			log.Printf("git %s %s: %v %s", verb, w.ID, err, stderr.String())
		}
		s.store.TouchDevice(dev.ID, store.Now())
	default:
		http.NotFound(rw, r)
	}
}
