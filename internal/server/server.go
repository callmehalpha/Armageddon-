// Package server is the Armageddon server: identity, workspaces, the
// authority, Git hosting, checkpoints, the browser terminal and the web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/ids"
	"github.com/callmehalpha/Armageddon-/internal/store"
)

type Server struct {
	cfg     *config.Server
	store   *store.Store
	hookBin string // copy of this binary that workspace users can execute

	// helper performs every privileged action (contract §2.5): the socket
	// client in production, the in-process dev helper otherwise.
	helper helper.Client
	// runDir holds the per-workspace authority sockets (P-14).
	runDir string

	mu  sync.Mutex
	rts map[string]*runtime // per-workspace runtime, by workspace ID

	sessions sessionRegistry // interactive server-seat sessions (§4.3)

	ctx context.Context
}

// Option configures a Server.
type Option func(*Server)

// WithHelper sets the privileged helper client (default: the in-process
// dev helper, no isolation).
func WithHelper(c helper.Client) Option { return func(s *Server) { s.helper = c } }

// WithRunDir sets the directory of the per-workspace authority sockets.
func WithRunDir(dir string) Option { return func(s *Server) { s.runDir = dir } }

// DefaultRunDir is where authority sockets go without a helper: a private
// per-user directory under the system temp dir.
func DefaultRunDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("armageddon-%d", os.Getuid()))
}

// New opens the data directory and prepares the server. It does not listen.
func New(cfg *config.Server, opts ...Option) (*Server, error) {
	// Everything the server creates is private by default (the database, its
	// WAL and backups, keys). Paths workspace users need are chmod-ed
	// explicitly. Found by the acceptance test: without this the SQLite file
	// was 0644 and readable by workspace users.
	syscall.Umask(0o077)
	st, err := store.Open(filepath.Join(cfg.DataDir, "armageddon.db"))
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, rts: map[string]*runtime{}}
	for _, o := range opts {
		o(s)
	}
	if s.helper == nil {
		s.helper = helper.NewDev(cfg.DataDir)
	}
	if s.runDir == "" {
		s.runDir = cfg.RunDir
	}
	if s.runDir == "" {
		s.runDir = DefaultRunDir()
	}
	if err := os.MkdirAll(s.runDir, 0o755); err != nil {
		return nil, fmt.Errorf("run directory: %w", err)
	}
	if err := s.installHookBinary(); err != nil {
		return nil, fmt.Errorf("install hook binary: %w", err)
	}
	return s, nil
}

func (s *Server) Store() *store.Store { return s.store }

// installHookBinary copies the running binary to <data>/bin so that hooks,
// which run as workspace users, can execute it regardless of where the
// server binary lives.
func (s *Server) installHookBinary() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Join(s.cfg.DataDir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Workspace users must traverse to their own directories and run hooks.
	for _, d := range []string{s.cfg.DataDir, dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	dst := filepath.Join(dir, "armageddon")
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	s.hookBin = dst
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Run serves HTTP until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.ctx = ctx
	if !s.helper.Isolated() {
		log.Printf("WARNING: no privileged helper: workspace processes run as the server's own user (no isolation, development mode)")
	} else if err := checkTraversable(s.cfg.DataDir); err != nil {
		return err
	}
	if err := s.ensureSetupToken(); err != nil {
		return err
	}
	if err := s.startWorkspaces(ctx); err != nil {
		return err
	}
	go s.janitor(ctx)

	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.routes(), ReadHeaderTimeout: 15 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Printf("armageddon server listening on %s (public URL %s)", s.cfg.Listen, s.cfg.PublicURL)
		if s.cfg.TLSCert != "" {
			errc <- srv.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		s.mu.Lock()
		for _, rt := range s.rts {
			rt.stop()
		}
		s.mu.Unlock()
		return s.store.Close()
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.store.Prune(store.Now())
		}
	}
}

// ensureSetupToken prints a one-time setup URL while no user exists (§7.1).
func (s *Server) ensureSetupToken() error {
	n, err := s.store.CountUsers()
	if err != nil || n > 0 {
		return err
	}
	tok := ids.Secret(32)
	if err := s.store.CreateOneTimeToken(ids.Hash(tok), "setup", "admin", "", store.Now()+int64(time.Hour/time.Millisecond)); err != nil {
		return err
	}
	fmt.Printf("\n  No users yet. Create the first admin within 1 hour at:\n\n    %s/setup?token=%s\n\n", s.cfg.PublicURL, tok)
	return nil
}

// checkTraversable makes sure workspace users can reach their directories:
// every ancestor of the data directory must be searchable by others.
func checkTraversable(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for d := filepath.Dir(abs); ; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("data directory %s is unusable: workspace users cannot traverse %s (mode %v). Use a path such as /var/lib/armageddon, or chmod o+x %s", abs, d, fi.Mode().Perm(), d)
		}
		if d == "/" || d == "." {
			return nil
		}
	}
}

// event records an audit event; failures are logged, not fatal.
func (s *Server) event(wsID, actorKind, actorID, typ string, payload any) {
	b, _ := json.Marshal(payload)
	if err := s.store.AddEvent(&store.Event{ID: ids.New(), TS: store.Now(), WorkspaceID: wsID, ActorKind: actorKind, ActorID: actorID, Type: typ, Payload: b}); err != nil {
		log.Printf("event %s: %v", typ, err)
	}
}
