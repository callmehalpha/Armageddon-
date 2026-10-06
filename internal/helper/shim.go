package helper

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// ShimMain is `armageddon helper-exec`, run by the server (as the server
// user) in place of a workspace command. It asks the helper to start the
// real command as the workspace user with this process's stdin, stdout and
// stderr, forwards termination signals, and exits with the command's
// status (128+n if it was killed by signal n). If the shim dies, the helper
// sees the connection drop and kills the workspace process, so
// exec.CommandContext cancellation keeps working.
//
// The environment of the shim is the requested environment (the helper
// filters it). Nothing is ever written to stdout.
func ShimMain(args []string) int {
	fs := flag.NewFlagSet("helper-exec", flag.ContinueOnError)
	sock := fs.String("socket", "", "helper socket")
	ws := fs.String("workspace", "", "workspace ID")
	kind := fs.String("kind", string(KindRuntimeCommand), "process kind")
	cwd := fs.String("cwd", "", "working directory inside the workspace")
	if err := fs.Parse(args); err != nil {
		return 127
	}
	argv := fs.Args()
	if *sock == "" || *ws == "" || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: armageddon helper-exec --socket S --workspace ID --kind K --cwd DIR -- argv...")
		return 127
	}
	// Catch signals before spawning so none is lost in between.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	c := &Socket{Path: *sock}
	p, err := c.Spawn(context.Background(), *ws, SpawnSpec{Kind: Kind(*kind), Argv: argv, Env: os.Environ(), Dir: *cwd,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "armageddon helper-exec:", err)
		return 127
	}
	go func() {
		for s := range sigc {
			p.Signal(s.(syscall.Signal))
		}
	}()
	st, err := p.Wait()
	if err != nil {
		fmt.Fprintln(os.Stderr, "armageddon helper-exec:", err)
		return 127
	}
	return st.ShellCode()
}
