// Command armageddon is the single Armageddon binary: server, hooks, device
// agent and CLI (contract §9.1).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/callmehalpha/Armageddon-/internal/agent"
	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/server"
)

var version = "0.1.0-mvp"

const usage = `armageddon — your development environment survives the machine.

Server:
  armageddon server init  [--data DIR] [--listen ADDR] [--public-url URL] [--tls-cert F --tls-key F]
  armageddon server run   [--data DIR] [--helper-socket PATH] [--dev]
  armageddon helper       [--data DIR] [--socket PATH] [--server-user NAME]   (as root)

Device:
  armageddon login <server-url> [--name NAME]   pair this machine (approve in the browser)
  armageddon workspaces                          list workspaces you can access
  armageddon clone <workspace> [dir]             create a follower replica
  armageddon agent run                           keep every replica current, and write when it holds the lease
  armageddon agent install                       run the agent at login (systemd --user / launchd)
  armageddon follow [dir]                        run one replica in the foreground (instead of the agent)
  armageddon status [dir]                        replica health and who holds the workspace
  armageddon work local [--force]                take the workspace: this replica becomes the writer
  armageddon work remote [--restart] [--force]   hand the workspace back to the server seat
  armageddon quarantine list|diff ID|apply ID|export ID DIR|drop ID
                                                 changes kept aside instead of being overwritten
  armageddon sync <workspace>                    checkpoint the server seat now
  armageddon logout                              forget this device's credentials

Other:
  armageddon version
`

func main() {
	agent.AgentVersion = version
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "version", "--version":
		fmt.Println("armageddon", version)
	case "server":
		err = serverCmd(ctx, args)
	case "helper":
		err = helperCmd(ctx, args)
	case "helper-exec": // internal: the server's shim into the helper
		os.Exit(helper.ShimMain(args))
	case "helper-nnp": // internal: run by the helper as ws-<id>
		os.Exit(helper.NNPMain(args))
	case "hook":
		if len(args) == 0 {
			os.Exit(2)
		}
		os.Exit(server.HookMain(args[0], args[1:]))
	case "git-credential":
		op := ""
		if len(args) > 0 {
			op = args[0]
		}
		err = agent.CredentialHelper(op, os.Stdin, os.Stdout)
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		name := fs.String("name", "", "device name (default: hostname)")
		fs.Parse(reorder(args))
		if fs.NArg() != 1 {
			err = fmt.Errorf("usage: armageddon login <server-url> [--name NAME]")
			break
		}
		err = agent.Login(fs.Arg(0), *name, os.Stdout)
	case "logout":
		dir, _ := os.UserConfigDir()
		err = os.RemoveAll(dir + "/armageddon")
		if err == nil {
			fmt.Println("Device credentials removed. Revoke the device in the web UI to cut server-side access too.")
		}
	case "workspaces", "ws":
		err = withClient(func(c *agent.Client) error {
			ws, err := c.Workspaces()
			for _, w := range ws {
				fmt.Printf("%-28s %-20s %-9s #%d\n", w.ID, w.Slug, w.State, w.CheckpointSeq)
			}
			return err
		})
	case "clone":
		if len(args) < 1 {
			err = fmt.Errorf("usage: armageddon clone <workspace> [dir]")
			break
		}
		dir := ""
		if len(args) > 1 {
			dir = args[1]
		}
		err = withClient(func(c *agent.Client) error { return c.Clone(args[0], dir, os.Stdout) })
	case "agent":
		switch {
		case len(args) == 1 && args[0] == "run":
			err = withClient(func(c *agent.Client) error { return c.RunAgent(ctx, os.Stdout) })
		case len(args) == 1 && args[0] == "install":
			err = agent.InstallService(os.Stdout)
		default:
			err = fmt.Errorf("usage: armageddon agent run|install")
		}
	case "work":
		fs := flag.NewFlagSet("work", flag.ExitOnError)
		force := fs.Bool("force", false, "take over without the holder's cooperation (its unsent changes are quarantined when it returns)")
		restart := fs.Bool("restart", false, "work remote: restart the workspace's dev processes on the server")
		wsRef := fs.String("workspace", "", "workspace (default: the replica containing the current directory)")
		if len(args) == 0 || (args[0] != "local" && args[0] != "remote") {
			err = fmt.Errorf("usage: armageddon work local [--force] | armageddon work remote [--restart] [--force] [--workspace WS]")
			break
		}
		fs.Parse(reorder(args[1:]))
		err = withClient(func(c *agent.Client) error {
			id, err := workspaceArg(c, *wsRef, fs.Args())
			if err != nil {
				return err
			}
			if args[0] == "local" {
				return c.WorkLocal(id, *force, os.Stdout)
			}
			return c.WorkRemote(id, *restart, *force, os.Stdout)
		})
	case "quarantine", "q":
		if len(args) == 0 {
			err = fmt.Errorf("usage: armageddon quarantine list|diff <id>|apply <id>|export <id> <dir>|drop <id>")
			break
		}
		fs := flag.NewFlagSet("quarantine", flag.ExitOnError)
		wsRef := fs.String("workspace", "", "workspace (default: the replica containing the current directory)")
		fs.Parse(reorder(args[1:]))
		rest := fs.Args()
		need := map[string]int{"list": 0, "diff": 1, "apply": 1, "drop": 1, "export": 2}
		n, ok := need[args[0]]
		if !ok || len(rest) != n {
			err = fmt.Errorf("usage: armageddon quarantine list|diff <id>|apply <id>|export <id> <dir>|drop <id>")
			break
		}
		err = withClient(func(c *agent.Client) error {
			id, err := workspaceArg(c, *wsRef, nil)
			if err != nil {
				return err
			}
			switch args[0] {
			case "list":
				return c.QuarantineList(id, os.Stdout)
			case "diff":
				return c.QuarantineDiff(id, rest[0], os.Stdout)
			case "apply":
				return c.QuarantineApply(id, rest[0], os.Stdout)
			case "export":
				return c.QuarantineExport(id, rest[0], rest[1], os.Stdout)
			}
			return c.QuarantineDrop(id, rest[0], os.Stdout)
		})
	case "follow", "status":
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		err = withClient(func(c *agent.Client) error {
			id, err := agent.FindReplica(dir)
			if err != nil {
				return err
			}
			if cmd == "status" {
				return c.Status(id, os.Stdout)
			}
			return c.Follow(ctx, id, os.Stdout)
		})
	case "sync":
		if len(args) != 1 {
			err = fmt.Errorf("usage: armageddon sync <workspace>")
			break
		}
		err = withClient(func(c *agent.Client) error {
			w, err := c.Resolve(args[0])
			if err != nil {
				return err
			}
			var res struct {
				NewSeq int64 `json:"new_seq"`
			}
			if err := c.Do("POST", "/api/workspaces/"+w.ID+"/sync", nil, &res); err != nil {
				return err
			}
			if res.NewSeq == 0 {
				fmt.Println("No changes since the last checkpoint.")
			} else {
				fmt.Printf("Checkpoint #%d created.\n", res.NewSeq)
			}
			return nil
		})
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "armageddon:", err)
		os.Exit(1)
	}
}

// workspaceArg resolves --workspace, a positional workspace, or the
// replica containing the current directory.
func workspaceArg(c *agent.Client, flagVal string, pos []string) (string, error) {
	ref := flagVal
	if ref == "" && len(pos) > 0 {
		ref = pos[0]
	}
	if ref == "" {
		return agent.FindReplica(".")
	}
	w, err := c.Resolve(ref)
	if err != nil {
		return "", err
	}
	return w.ID, nil
}

func withClient(f func(*agent.Client) error) error {
	c, err := agent.NewClient()
	if err != nil {
		return err
	}
	return f(c)
}

// reorder moves flags before positional arguments so `login URL --name x`
// works with the standard flag package.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, args[i])
		}
	}
	return append(flags, pos...)
}

func serverCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: armageddon server init|run [flags]")
	}
	fs := flag.NewFlagSet("server "+args[0], flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	listen := fs.String("listen", "", "listen address, e.g. :8080")
	public := fs.String("public-url", "", "URL clients use to reach this server")
	cert := fs.String("tls-cert", "", "TLS certificate file")
	key := fs.String("tls-key", "", "TLS key file")
	helperSock := fs.String("helper-socket", "", "privileged helper socket (default "+helper.DefaultSocket+")")
	dev := fs.Bool("dev", false, "development mode: no helper, everything runs as the current user (no isolation)")
	fs.Parse(args[1:])
	switch args[0] {
	case "init":
		cfg, err := config.Load(*data)
		if err != nil {
			cfg = config.Default(*data)
		}
		if *listen != "" {
			cfg.Listen = *listen
		}
		if *public != "" {
			cfg.PublicURL = strings.TrimRight(*public, "/")
		}
		if *cert != "" {
			cfg.TLSCert, cfg.TLSKey = *cert, *key
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Wrote %s\nStart the server with: armageddon server run --data %s\n", config.Path(*data), *data)
		return nil
	case "run":
		abs, err := filepath.Abs(*data)
		if err != nil {
			return err
		}
		client, runDir, err := pickHelper(abs, *helperSock, *dev)
		if err != nil {
			return err
		}
		// An MVP data directory was written by a root server: hand it to
		// this user before reading the (root-only) config and database.
		if err := server.UpgradeDataDir(ctx, abs, client); err != nil {
			return err
		}
		cfg, err := config.Load(abs)
		if err != nil {
			return fmt.Errorf("%v (run `armageddon server init --data %s` first)", err, *data)
		}
		if *listen != "" {
			cfg.Listen = *listen
		}
		if cfg.RunDir != "" {
			runDir = cfg.RunDir
		}
		s, err := server.New(cfg, server.WithHelper(client), server.WithRunDir(runDir))
		if err != nil {
			return err
		}
		return s.Run(ctx)
	}
	return fmt.Errorf("unknown server command %q", args[0])
}

// pickHelper chooses how the server reaches privileged operations: the
// helper socket when it exists (production), else the in-process dev
// helper when not root. A root server without a helper is refused: it
// would run workspace processes as root.
func pickHelper(dataDir, sock string, dev bool) (helper.Client, string, error) {
	if dev {
		return helper.NewDev(dataDir), "", nil
	}
	explicit := sock != ""
	if !explicit {
		sock = helper.DefaultSocket
	}
	if helper.Reachable(sock) {
		exe, err := os.Executable()
		if err != nil {
			return nil, "", err
		}
		return &helper.Socket{Path: sock, DataDir: dataDir, Shim: exe}, filepath.Dir(sock), nil
	}
	if explicit {
		return nil, "", fmt.Errorf("no helper socket at %s (start `armageddon helper` as root first)", sock)
	}
	if os.Geteuid() == 0 {
		return nil, "", fmt.Errorf("refusing to run the server as root: start `armageddon helper --data %s` as root and run the server as the armageddon user (or pass --dev for an unisolated development server)", dataDir)
	}
	return helper.NewDev(dataDir), "", nil
}

// helperCmd runs the privileged helper (contract §2.5). It must run as
// root; it serves only the server user.
func helperCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("helper", flag.ExitOnError)
	data := fs.String("data", "/var/lib/armageddon", "the server's data directory")
	sock := fs.String("socket", helper.DefaultSocket, "socket path (its directory also holds the per-workspace sockets)")
	user := fs.String("server-user", "armageddon", "the user the server runs as")
	fs.Parse(args)
	d, err := helper.NewDaemon(*data, *sock, *user)
	if err != nil {
		return err
	}
	return d.Serve(ctx)
}
