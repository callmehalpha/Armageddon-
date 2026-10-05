// Command armageddon is the single Armageddon binary: server, hooks, device
// agent and CLI (contract §9.1).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/callmehalpha/Armageddon-/internal/agent"
	"github.com/callmehalpha/Armageddon-/internal/components"
	"github.com/callmehalpha/Armageddon-/internal/config"
	"github.com/callmehalpha/Armageddon-/internal/server"
)

var version = "0.1.0-mvp"

const usage = `armageddon — your development environment survives the machine.

Server:
  armageddon server init  [--data DIR] [--listen ADDR] [--public-url URL] [--tls-cert F --tls-key F]
  armageddon server init  ... [--ssh-listen ADDR] [--code-server PATH]
  armageddon server run   [--data DIR]
  armageddon server components install code-server [--version V] [--data DIR]

Device:
  armageddon login <server-url> [--name NAME]   pair this machine (approve in the browser)
  armageddon workspaces                          list workspaces you can access
  armageddon clone <workspace> [dir]             create a follower replica
  armageddon follow [dir]                        keep a replica current (foreground)
  armageddon status [dir]                        replica health
  armageddon sync <workspace>                    checkpoint the server seat now
  armageddon ssh-config [workspace...] [--file F | --print]
                                                 write ~/.ssh/config Host blocks for the SSH endpoint
  armageddon logout                              forget this device's credentials

Other:
  armageddon version
`

func main() {
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
		return fmt.Errorf("usage: armageddon server init|run|components [flags]")
	}
	if args[0] == "components" {
		return componentsCmd(args[1:])
	}
	fs := flag.NewFlagSet("server "+args[0], flag.ExitOnError)
	data := fs.String("data", config.DefaultDataDir(), "data directory")
	listen := fs.String("listen", "", "listen address, e.g. :8080")
	public := fs.String("public-url", "", "URL clients use to reach this server")
	cert := fs.String("tls-cert", "", "TLS certificate file")
	key := fs.String("tls-key", "", "TLS key file")
	sshListen := fs.String("ssh-listen", "", "enable the SSH endpoint on this address, e.g. :2222 (experimental, gated on P8)")
	codeServer := fs.String("code-server", "", "path to the code-server executable")
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
		if *sshListen != "" {
			cfg.SSH.Enabled, cfg.SSH.Listen = true, *sshListen
		}
		if *codeServer != "" {
			cfg.CodeServer.Path = *codeServer
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Wrote %s\nStart the server with: armageddon server run --data %s\n", config.Path(*data), *data)
		return nil
	case "run":
		cfg, err := config.Load(*data)
		if err != nil {
			return fmt.Errorf("%v (run `armageddon server init --data %s` first)", err, *data)
		}
		if *listen != "" {
			cfg.Listen = *listen
		}
		s, err := server.New(cfg)
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
