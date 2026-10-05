// P8: embedded SSH endpoint spike (throwaway).
//
//	p8 serve -listen 127.0.0.1:2222 -keys authorized -hostkey FILE
//	p8 sftp-server                      SFTP over stdio (exec'd as the workspace user)
//
// The authorized file holds lines "ws-<slug> ssh-ed25519 AAAA...". A client
// logs in as ws-<slug>; only Ed25519 device keys listed for that user are
// accepted. Every session process (shell, exec, SFTP) runs as the OS user
// ws-<slug>. In the product this spawn goes through the helper
// (SpawnInWorkspace kind=ssh-session); here the spike runs as root and drops
// privileges directly, which P6 showed is equivalent in effect.
//
// Local port forwarding (direct-tcpip, which also carries `ssh -D`) is allowed
// only to loopback destinations.
package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

var userRe = regexp.MustCompile(`^ws-[a-z0-9]{8,24}$`)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sftp-server" {
		srv, err := sftp.NewServer(struct {
			io.Reader
			io.WriteCloser
		}{os.Stdin, os.Stdout})
		if err != nil {
			os.Exit(1)
		}
		if err := srv.Serve(); err != nil && err != io.EOF {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: p8 serve -listen ADDR -keys FILE -hostkey FILE | p8 sftp-server")
		os.Exit(2)
	}
	listen, keysFile, hostKey := "127.0.0.1:2222", "authorized", "hostkey"
	for i := 2; i+1 < len(os.Args); i += 2 {
		switch os.Args[i] {
		case "-listen":
			listen = os.Args[i+1]
		case "-keys":
			keysFile = os.Args[i+1]
		case "-hostkey":
			hostKey = os.Args[i+1]
		}
	}
	auth, err := loadAuthorized(keysFile)
	if err != nil {
		log.Fatal(err)
	}
	self, _ := os.Executable()
	signer, err := loadOrCreateHostKey(hostKey)
	if err != nil {
		log.Fatal(err)
	}

	srv := &ssh.Server{
		Addr: listen,
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			if key.Type() != gossh.KeyAlgoED25519 || !userRe.MatchString(ctx.User()) {
				return false
			}
			for _, k := range auth[ctx.User()] {
				if ssh.KeysEqual(k, key) {
					return true
				}
			}
			return false
		},
		Handler: func(s ssh.Session) { handleSession(s) },
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(s ssh.Session) { runAsUser(s, nil, false, self, "sftp-server") },
		},
		LocalPortForwardingCallback: func(ctx ssh.Context, host string, port uint32) bool {
			ok := isLoopback(host)
			log.Printf("forward %s -> %s:%d allowed=%v", ctx.User(), host, port, ok)
			return ok
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
	}
	srv.AddHostKey(signer)
	log.Printf("p8: listening on %s", listen)
	log.Fatal(srv.ListenAndServe())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func handleSession(s ssh.Session) {
	ptyReq, winCh, isPty := s.Pty()
	var argv []string
	if raw := s.RawCommand(); raw != "" {
		argv = []string{"/bin/bash", "-c", raw}
	} else {
		argv = []string{"/bin/bash", "-l"}
	}
	if isPty {
		runAsUser(s, &ptyState{req: ptyReq, win: winCh}, true, argv[0], argv[1:]...)
		return
	}
	runAsUser(s, nil, false, argv[0], argv[1:]...)
}

type ptyState struct {
	req ssh.Pty
	win <-chan ssh.Window
}

// runAsUser runs a program as the OS user named by the SSH login, wired to the
// session (via a PTY or plain pipes), and reports its exit status.
func runAsUser(s ssh.Session, p *ptyState, _ bool, name string, args ...string) {
	u, err := user.Lookup(s.User())
	if err != nil || !userRe.MatchString(s.User()) {
		fmt.Fprintln(s.Stderr(), "no such workspace")
		s.Exit(1)
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cmd := exec.Command(name, args...)
	cmd.Dir = u.HomeDir
	if _, err := os.Stat(cmd.Dir); err != nil {
		cmd.Dir = "/"
	}
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + u.HomeDir, "USER=" + u.Username,
		"LOGNAME=" + u.Username, "SHELL=/bin/bash", "LANG=C.UTF-8",
	}
	for _, e := range s.Environ() { // client-sent env: allowlist only
		if strings.HasPrefix(e, "LC_") || strings.HasPrefix(e, "TERM=") || strings.HasPrefix(e, "VSCODE_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}},
	}
	if p != nil {
		cmd.Env = append(cmd.Env, "TERM="+p.req.Term)
		f, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: uint16(p.req.Window.Width), Rows: uint16(p.req.Window.Height)},
			&syscall.SysProcAttr{Credential: cmd.SysProcAttr.Credential, Setsid: true, Setctty: true})
		if err != nil {
			fmt.Fprintln(s.Stderr(), "pty:", err)
			s.Exit(1)
			return
		}
		go func() {
			for w := range p.win {
				pty.Setsize(f, &pty.Winsize{Cols: uint16(w.Width), Rows: uint16(w.Height)})
			}
		}()
		go io.Copy(f, s)
		io.Copy(s, f)
		s.Exit(exitCode(cmd.Wait()))
		return
	}
	cmd.Stdin = s
	cmd.Stdout = s
	cmd.Stderr = s.Stderr()
	s.Exit(exitCode(cmd.Run()))
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

func loadAuthorized(path string) (map[string][]ssh.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string][]ssh.PublicKey{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		userName, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", userName, err)
		}
		out[userName] = append(out[userName], k)
	}
	return out, nil
}

func loadOrCreateHostKey(path string) (gossh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		return gossh.ParsePrivateKey(b)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(priv)
}
