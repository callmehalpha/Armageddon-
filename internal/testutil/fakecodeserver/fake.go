// Package fakecodeserver is a stand-in for code-server in tests: it accepts
// the flags the server passes, listens on the unix socket, and serves a few
// endpoints that let tests observe what the proxy forwards. Like
// code-server, it refuses WebSocket upgrades whose Origin does not match the
// Host header.
package fakecodeserver

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/coder/websocket"
)

// Main runs the fake and returns an exit code.
func Main(args []string) int {
	fs := flag.NewFlagSet("code-server", flag.ContinueOnError)
	sock := fs.String("socket", "", "")
	mode := fs.String("socket-mode", "", "")
	auth := fs.String("auth", "password", "")
	userData := fs.String("user-data-dir", "", "")
	extDir := fs.String("extensions-dir", "", "")
	fs.Bool("disable-telemetry", false, "")
	fs.Bool("disable-update-check", false, "")
	fs.Bool("disable-proxy", false, "")
	fs.Bool("disable-workspace-trust", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sock == "" || *auth != "none" {
		fmt.Fprintln(os.Stderr, "fake code-server: want --socket and --auth none")
		return 2
	}
	folder := fs.Arg(0)
	os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake code-server:", err)
		return 1
	}
	if m, err := strconv.ParseUint(*mode, 8, 32); err == nil {
		os.Chmod(*sock, os.FileMode(m))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(rw, "<!doctype html><title>fake code-server</title><p>fake code-server folder=%s</p><script src=\"./static/app.js\"></script>", folder)
	})
	mux.HandleFunc("/static/app.js", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/javascript")
		io.WriteString(rw, "console.log('fake')\n")
	})
	mux.HandleFunc("/echo", func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.NewEncoder(rw).Encode(map[string]any{"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
			"host": r.Host, "headers": r.Header, "body": string(body)})
	})
	mux.HandleFunc("/env", func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(map[string]any{"uid": os.Getuid(), "home": os.Getenv("HOME"), "user": os.Getenv("USER"),
			"user_data_dir": *userData, "extensions_dir": *extDir, "folder": folder, "pid": os.Getpid(),
			"env": os.Environ()})
	})
	mux.HandleFunc("/setcookie", func(rw http.ResponseWriter, r *http.Request) {
		http.SetCookie(rw, &http.Cookie{Name: "arm_session", Value: "planted", Path: "/"})
		http.SetCookie(rw, &http.Cookie{Name: "cs_pref", Value: "dark", Path: "/"})
		io.WriteString(rw, "ok")
	})
	mux.HandleFunc("/crash", func(rw http.ResponseWriter, r *http.Request) {
		os.Exit(3)
	})
	mux.HandleFunc("/ws", func(rw http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(rw, "origin mismatch", 403)
				return
			}
		}
		c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.Write(r.Context(), websocket.MessageText, []byte("hello"))
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if c.Write(r.Context(), typ, b) != nil {
				return
			}
		}
	})
	srv := &http.Server{Handler: mux}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		srv.Shutdown(context.Background())
		os.Exit(0)
	}()
	srv.Serve(ln)
	return 0
}
