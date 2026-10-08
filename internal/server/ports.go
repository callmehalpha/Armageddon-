package server

import (
	"bufio"
	"context"
	"encoding/hex"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/helper"
)

// Port listing (plan M8.6). A workspace's ports are the TCP sockets its user
// listens on, read from /proc/net/tcp{,6} (the uid column), plus the ports
// its Compose project publishes (bound to 127.0.0.1 by the helper). Only
// those are reachable through the port proxy, so a workspace cannot use it
// to reach the server's or another workspace's loopback services.

// listenSocket is one listening TCP socket.
type listenSocket struct {
	IP   net.IP
	Port int
	UID  uint32
}

// parseProcNetTCP reads /proc/net/tcp or /proc/net/tcp6 and returns the
// sockets in LISTEN state.
func parseProcNetTCP(r io.Reader) []listenSocket {
	var out []listenSocket
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// sl local_address rem_address st tx:rx tr:tm retrnsmt uid ...
		if len(f) < 8 || f[3] != "0A" {
			continue
		}
		host, port, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		p, err := strconv.ParseUint(port, 16, 16)
		if err != nil {
			continue
		}
		ip := procIP(host)
		uid, err := strconv.ParseUint(f[7], 10, 32)
		if ip == nil || err != nil {
			continue
		}
		out = append(out, listenSocket{IP: ip, Port: int(p), UID: uint32(uid)})
	}
	return out
}

// procIP decodes /proc's address column: the address bytes in host byte
// order, in 32-bit words.
func procIP(h string) net.IP {
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return nil
	}
	ip := make(net.IP, len(b))
	for i := 0; i < len(b); i += 4 {
		// Each 32-bit word is little-endian on the hosts Armageddon runs on.
		ip[i], ip[i+1], ip[i+2], ip[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return ip
}

func listeningSockets() []listenSocket {
	var out []listenSocket
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		out = append(out, parseProcNetTCP(f)...)
		f.Close()
	}
	return out
}

// wsPort is one port of a workspace, as the API shows it.
type wsPort struct {
	Port    int    `json:"port"`
	Address string `json:"address"` // where it listens
	Source  string `json:"source"`  // process, compose
	Service string `json:"service,omitempty"`
	Path    string `json:"path"` // the authenticated proxy path
	// dial is where the proxy connects.
	dial string
}

// reachableIP is the address the proxy dials for a socket bound to ip, or
// "" if the socket cannot be reached over loopback.
func reachableIP(ip net.IP) string {
	switch {
	case ip.IsUnspecified() && ip.To4() == nil:
		return "::1" // [::] may be IPV6_V6ONLY, which 127.0.0.1 would miss
	case ip.IsUnspecified():
		return "127.0.0.1"
	case ip.IsLoopback():
		return ip.String()
	}
	if v4 := ip.To4(); v4 != nil && len(ip) == 16 && v4.IsLoopback() {
		return v4.String()
	}
	return ""
}

// workspacePorts lists the ports of a workspace.
func (s *Server) workspacePorts(ctx context.Context, rt *runtime) []wsPort {
	seen := map[int]bool{}
	var out []wsPort
	own := s.ownPorts()
	prefix := "/api/workspaces/" + rt.id + "/ports/"
	for _, so := range listeningSockets() {
		if so.UID != rt.acct.UID || seen[so.Port] || own[so.Port] {
			continue
		}
		addr := reachableIP(so.IP)
		if addr == "" {
			continue
		}
		seen[so.Port] = true
		out = append(out, wsPort{Port: so.Port, Address: net.JoinHostPort(so.IP.String(), strconv.Itoa(so.Port)), Source: "process",
			Path: prefix + strconv.Itoa(so.Port) + "/", dial: net.JoinHostPort(addr, strconv.Itoa(so.Port))})
	}
	for _, svc := range s.composeServices(ctx, rt.id) {
		for _, p := range svc.Ports {
			if seen[p.Published] || (p.Protocol != "" && p.Protocol != "tcp") {
				continue
			}
			seen[p.Published] = true
			out = append(out, wsPort{Port: p.Published, Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Published)),
				Source: "compose", Service: svc.Service, Path: prefix + strconv.Itoa(p.Published) + "/",
				dial: net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Published))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	if out == nil {
		out = []wsPort{}
	}
	return out
}

// ownPorts are the server's own listeners. Without isolation (dev mode)
// they have the workspace "user's" uid too.
func (s *Server) ownPorts() map[int]bool {
	own := map[int]bool{}
	for _, a := range []string{s.cfg.Listen, s.cfg.SSH.Listen} {
		if _, p, err := net.SplitHostPort(a); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				own[n] = true
			}
		}
	}
	return own
}

// composeServices is ComposePs, cached briefly: the port proxy asks on
// every request.
func (s *Server) composeServices(ctx context.Context, wsID string) []helper.ComposeService {
	s.composeMu.Lock()
	c := s.composeCache[wsID]
	s.composeMu.Unlock()
	if c != nil && time.Since(c.at) < 10*time.Second {
		return c.svcs
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	svcs, err := s.helper.ComposePs(ctx, wsID)
	if err != nil {
		svcs = nil // no Docker, or nothing running: no Compose ports
	}
	s.composeMu.Lock()
	s.composeCache[wsID] = &composeCacheEntry{at: time.Now(), svcs: svcs}
	s.composeMu.Unlock()
	return svcs
}

type composeCacheEntry struct {
	at   time.Time
	svcs []helper.ComposeService
}

func (s *Server) invalidateCompose(wsID string) {
	s.composeMu.Lock()
	delete(s.composeCache, wsID)
	s.composeMu.Unlock()
}
