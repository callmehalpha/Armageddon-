// Command armageddon is the single Armageddon binary: server, hooks, device
// agent and CLI (contract §9.1).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/callmehalpha/Armageddon-/internal/agent"
	"github.com/callmehalpha/Armageddon-/internal/components"
	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/helper"
	"github.com/callmehalpha/Armageddon-/internal/server"
)

// version and commit are stamped by the release build with -ldflags -X.
var (
	version = "0.1.0-mvp"
	commit  = "unknown"
)

const usage = `armageddon — your development environment survives the machine.

Server:
  armageddon server init  [--data DIR] [--listen ADDR] [--public-url URL]
                          [--domain D --acme-email E [--acme-staging]] | [--ip-only [--ip A,B]]
                          | [--tls-cert F --tls-key F]
                          [--ssh-listen ADDR] [--code-server PATH]
  armageddon server run   [--data DIR] [--helper-socket PATH] [--dev]
  armageddon helper       [--data DIR] [--socket PATH] [--server-user NAME]   (as root)
  armageddon server fingerprint                  print the TLS certificate fingerprint
  armageddon server backup [--to DIR] [--passphrase-file F]   (keys included when a passphrase is given)
  armageddon server restore <backup-dir> [--data DIR] [--passphrase-file F]
  armageddon server update [--to VERSION | --bundle FILE.tar]
  armageddon server rollback [--keep-db]
  armageddon server uninstall [--purge]
  armageddon server migrate [--data DIR]         apply database migrations and exit
  armageddon server components install code-server [--version V] [--data DIR]
  armageddon doctor [--data DIR] [--repair] [--json]

Device:
  armageddon login <server-url> [--name NAME] [--fingerprint SHA256]
                                                 pair this machine (approve in the browser)
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
  armageddon runtime [status|install|start [--port N]|stop|logs [--follow]]
                                                 the workspace's runtime on the server seat (Node, PHP)
  armageddon compose up [--file F]|down|ps       the workspace's Docker Compose services, on the server
  armageddon ports                               ports the workspace listens on, with their authenticated URLs
  armageddon ssh-config [workspace...] [--file F | --print]
                                                 write ~/.ssh/config Host blocks for the SSH endpoint
  armageddon workspace seed --from-replica [dir] [--name NAME]
                                                 rebuild a lost workspace on this (new) server from a replica (F9)
  armageddon workspace repair --from-device [dir]
                                                 send a replica's history to restore objects the server lost (F10)
  armageddon logout                              forget this device's credentials

In the workspace on the server (terminal, IDE, SSH):
  armageddon git trash [list | restore <n> [--as NAME]]
                                                 deleted and force-moved branches and tags, kept by the server

Other:
  armageddon version
  armageddon release verify <manifest.json> <manifest.json.minisig> [--pubkey KEY] [--dir DIR]
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
		fmt.Printf("armageddon %s (commit %s)\n", version, commit)
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
	case "seat-credential":
		// Git credential helper inside server-seat sessions (workspace user).
		op := ""
		if len(args) > 0 {
			op = args[0]
		}
		err = server.SeatCredentialHelper(op, os.Stdin, os.Stdout)
	case "sftp-server":
		// Started by the SSH endpoint as the workspace user.
		err = server.SFTPServerMain()
	case "ssh-config":
		fs := flag.NewFlagSet("ssh-config", flag.ExitOnError)
		file := fs.String("file", "", "ssh config file to update (default ~/.ssh/config; - prints)")
		print := fs.Bool("print", false, "print the Host blocks instead of writing them")
		fs.Parse(reorder(args))
		if *print {
			*file = "-"
		}
		err = withClient(func(c *agent.Client) error { return c.SSHConfig(fs.Args(), *file, os.Stdout) })
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		name := fs.String("name", "", "device name (default: hostname)")
		fp := fs.String("fingerprint", "", "expected SHA-256 fingerprint of the server certificate (IP-only servers)")
		fs.Parse(reorder(args))
		if fs.NArg() != 1 {
			err = fmt.Errorf("usage: armageddon login <server-url> [--name NAME]")
			break
		}
		err = agent.Login(fs.Arg(0), *name, *fp, os.Stdout)
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
	case "git":
		err = gitTrashCmd(args, os.Stdout)
	case "workspace":
		err = workspaceCmd(args)
	case "runtime":
		err = runtimeCmd(ctx, args)
	case "compose":
		err = composeCmd(args)
	case "ports":
		err = portsCmd(args)
	case "doctor":
		err = doctorCmd(args)
	case "release":
		err = releaseCmd(args)
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
		return fmt.Errorf("usage: armageddon server init|run|backup|restore|update|rollback|uninstall|fingerprint|migrate|components [flags]")
	}
	if op, ok := opsCommands[args[0]]; ok {
		return op(ctx, args[1:])
	}
	fs := flag.NewFlagSet("server "+args[0], flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	listen := fs.String("listen", "", "listen address, e.g. :8080")
	helperSock := fs.String("helper-socket", "", "privileged helper socket (default "+helper.DefaultSocket+")")
	dev := fs.Bool("dev", false, "development mode: no helper, everything runs as the current user (no isolation)")
	fs.Parse(args[1:])
	switch args[0] {
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

// componentsCmd is `armageddon server components install code-server
// [--version v] [--sha256 hex] [--data DIR]`.
func componentsCmd(args []string) error {
	if len(args) < 2 || args[0] != "install" || args[1] != "code-server" {
		return fmt.Errorf("usage: armageddon server components install code-server [--version V] [--sha256 HEX] [--data DIR]")
	}
	fs := flag.NewFlagSet("components install", flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	ver := fs.String("version", components.CodeServerVersion, "code-server version")
	sum := fs.String("sha256", "", "expected SHA-256 of the release tarball (required for versions not pinned in this build)")
	fs.Parse(args[2:])
	exe, err := components.InstallCodeServer(*data, *ver, *sum, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("code-server is ready at %s; the server finds it there automatically.\n", exe)
	return nil
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

// workspaceCmd: recovery from a replica (contract §10 F9 b, F10).
func workspaceCmd(args []string) error {
	usage := errors.New("usage: armageddon workspace seed --from-replica [dir] [--name NAME] | repair --from-device [dir]")
	if len(args) == 0 {
		return usage
	}
	fs := flag.NewFlagSet("workspace "+args[0], flag.ExitOnError)
	fromReplica := fs.Bool("from-replica", false, "seed from the replica in dir (default: the current directory)")
	fromDevice := fs.Bool("from-device", false, "repair from the replica in dir (default: the current directory)")
	name := fs.String("name", "", "name of the new workspace (default: the replica directory's name)")
	fs.Parse(reorder(args[1:]))
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	switch {
	case args[0] == "seed" && *fromReplica:
		return withClient(func(c *agent.Client) error { return c.SeedFromReplica(dir, *name, os.Stdout) })
	case args[0] == "repair" && *fromDevice:
		return withClient(func(c *agent.Client) error { return c.RepairFromDevice(dir, os.Stdout) })
	}
	return usage
}
