// P6 prototype binary.
//
//	p6 helper -socket S -server-uid N -base B   root helper (blocks)
//	p6 stub [--pty] -- argv...                  trampoline exec'd by the helper
//	p6 demo -socket S -base B -srv D -ws ID     server half (unprivileged): functional run + timings
//	p6 bench-direct -base B -ws ID -n N         baseline: direct setuid spawn (root)
//
// See run-e2e.sh for the end-to-end sequence.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"armageddon/prototypes/p6-privilege-boundary/client"
	"armageddon/prototypes/p6-privilege-boundary/helper"
	"armageddon/prototypes/p6-privilege-boundary/proto"
)

const gitBin = "/usr/bin/git"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: p6 helper|stub|demo|bench-direct ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "stub":
		os.Exit(helper.StubMain(os.Args[2:]))
	case "helper":
		os.Exit(runHelper(os.Args[2:]))
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "bench-direct":
		os.Exit(runBenchDirect(os.Args[2:]))
	}
	fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
	os.Exit(2)
}

func runHelper(args []string) int {
	fs := flag.NewFlagSet("helper", flag.ExitOnError)
	sock := fs.String("socket", "/run/p6/helper.sock", "socket path")
	uid := fs.Int("server-uid", -1, "the only uid allowed to connect")
	base := fs.String("base", "", "workspaces base directory")
	fs.Parse(args)
	if os.Geteuid() != 0 || *uid < 0 || *base == "" {
		fmt.Fprintln(os.Stderr, "helper: must run as root with -server-uid and -base")
		return 2
	}
	self, _ := os.Executable()
	_, l, err := helper.Serve(helper.Config{Socket: *sock, ServerUID: *uid, BaseDir: *base, Self: self})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	// Socket owned by the server user, mode 0600 (set by Serve).
	os.Chown(*sock, *uid, -1)
	fmt.Fprintf(os.Stderr, "helper: listening on %s for uid %d\n", *sock, *uid)
	defer l.Close()
	select {}
}

// spawnCollect spawns via the helper with stdin/stdout/stderr pipes and
// returns stdout once the child closes it. The prototype API has no
// exit-status operation (see write-up ⟨P-15⟩); EOF on stdout marks completion.
func spawnCollect(c *client.Client, ws string, kind proto.SpawnKind, stdin []byte, argv ...string) ([]byte, string, error) {
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	_, err := c.Spawn(proto.Request{Workspace: ws, Kind: kind, Argv: argv},
		[]int{int(inR.Fd()), int(outW.Fd()), int(errW.Fd())})
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, "", err
	}
	go func() { inW.Write(stdin); inW.Close() }()
	var stderr bytes.Buffer
	done := make(chan struct{})
	go func() { io.Copy(&stderr, errR); close(done) }()
	out, err := io.ReadAll(outR)
	outR.Close()
	<-done
	errR.Close()
	return out, stderr.String(), err
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	sock := fs.String("socket", "/run/p6/helper.sock", "socket path")
	base := fs.String("base", "", "workspaces base directory")
	srv := fs.String("srv", "", "server-owned directory (holds checkpoints.git)")
	ws := fs.String("ws", "", "workspace id")
	n := fs.Int("n", 100, "benchmark iterations")
	fs.Parse(args)
	ok := true
	report := func(name string, pass bool, detail string) {
		mark := "PASS"
		if !pass {
			mark, ok = "FAIL", false
		}
		fmt.Printf("%s %s %s\n", mark, name, detail)
	}
	fmt.Printf("demo: server half running as uid %d\n", os.Getuid())
	c, err := client.Dial(*sock)
	if err != nil {
		fmt.Println("FAIL dial:", err)
		return 1
	}
	defer c.Close()

	_, err = c.Do(proto.Request{Op: proto.OpCreateWorkspaceUser, Workspace: *ws})
	report("CreateWorkspaceUser", err == nil, errStr(err))
	_, err = c.Do(proto.Request{Op: proto.OpPrepareWorkspaceDirs, Workspace: *ws})
	report("PrepareWorkspaceDirs", err == nil, errStr(err))
	u, _ := user.Lookup(proto.UserName(*ws))
	wsUID := ""
	if u != nil {
		wsUID = u.Uid
	}

	out, _, err := spawnCollect(c, *ws, proto.KindRuntimeCommand, nil, "/usr/bin/id", "-u")
	got := strings.TrimSpace(string(out))
	report("runtime-command runs as ws user", err == nil && got == wsUID && got != "0", "uid="+got+" want="+wsUID)

	out, _, _ = spawnCollect(c, *ws, proto.KindRuntimeCommand, nil, "/bin/grep", "-E", "^(NoNewPrivs|CapEff|Groups)", "/proc/self/status")
	report("spawned process has NoNewPrivs=1, no caps", bytes.Contains(out, []byte("NoNewPrivs:\t1")) && bytes.Contains(out, []byte("CapEff:\t0000000000000000")),
		strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", "; "))

	// PTY shell: server allocates the pty, passes the slave via SCM_RIGHTS.
	ptmx, tty, err := pty.Open()
	if err == nil {
		_, err = c.Spawn(proto.Request{Workspace: *ws, Kind: proto.KindPTYShell, WantPTY: true,
			Argv: []string{"/bin/sh", "-c", "tty >/dev/null && echo PTYOK uid=$(id -u)"}},
			[]int{int(tty.Fd()), int(tty.Fd()), int(tty.Fd())})
		tty.Close()
		buf := make([]byte, 4096)
		var acc []byte
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !bytes.Contains(acc, []byte("\n")) {
			ptmx.SetReadDeadline(deadline)
			k, rerr := ptmx.Read(buf)
			acc = append(acc, buf[:k]...)
			if rerr != nil {
				break
			}
		}
		ptmx.Close()
		s := strings.TrimSpace(string(acc))
		report("pty-shell has a controlling tty, runs as ws user", strings.Contains(s, "PTYOK uid="+wsUID), s)
	} else {
		report("pty-shell", false, err.Error())
	}

	// git-service: create repo.git and a commit in tree/, as the ws user.
	tree := filepath.Join(*base, *ws, "tree")
	repo := filepath.Join(*base, *ws, "repo.git")
	for _, a := range [][]string{
		{gitBin, "init", "-q", "--bare", repo},
		{gitBin, "-C", tree, "init", "-q"},
		{gitBin, "-C", tree, "-c", "user.email=p6@x", "-c", "user.name=p6", "commit", "-q", "--allow-empty", "-m", "base"},
		{gitBin, "-C", tree, "push", "-q", repo, "HEAD:refs/heads/main"},
	} {
		_, se, err := spawnCollect(c, *ws, proto.KindGitService, nil, a...)
		if err != nil || strings.Contains(se, "fatal") {
			report("git-service "+a[1], false, errStr(err)+" "+se)
		}
	}
	adv, _, err := spawnCollect(c, *ws, proto.KindGitService, nil, gitBin, "upload-pack", "--stateless-rpc", "--advertise-refs", repo)
	report("git-service upload-pack advertises refs", err == nil && bytes.Contains(adv, []byte("refs/heads/main")), fmt.Sprintf("%d bytes", len(adv)))

	// Capture: pack-objects as ws user, piped to index-pack run as the server user.
	cps := filepath.Join(*srv, "checkpoints.git")
	exec.Command(gitBin, "init", "-q", "--bare", cps).Run()
	capture := func() error {
		pack, se, err := spawnCollect(c, *ws, proto.KindGitService, []byte("HEAD\n"), gitBin, "-C", tree, "pack-objects", "--revs", "--stdout", "-q")
		if err != nil || len(pack) == 0 {
			return fmt.Errorf("pack-objects: %v %s", err, se)
		}
		ip := exec.Command(gitBin, "--git-dir="+cps, "index-pack", "--stdin", "--fix-thin")
		ip.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + *srv}
		ip.Stdin = bytes.NewReader(pack)
		if o, err := ip.CombinedOutput(); err != nil {
			return fmt.Errorf("index-pack: %v %s", err, o)
		}
		return nil
	}
	err = capture()
	report("capture: pack-objects (ws) -> index-pack (server)", err == nil, errStr(err))

	// Timings through the helper.
	gitReq := func() error {
		_, _, err := spawnCollect(c, *ws, proto.KindGitService, nil, gitBin, "upload-pack", "--stateless-rpc", "--advertise-refs", repo)
		return err
	}
	printTimes("helper", "git request (upload-pack --advertise-refs)", *n, gitReq)
	printTimes("helper", "capture (pack-objects | index-pack)", *n, capture)
	printTimes("helper", "spawn /bin/true", *n, func() error {
		_, _, err := spawnCollect(c, *ws, proto.KindRuntimeCommand, nil, "/bin/true")
		return err
	})
	if !ok {
		return 1
	}
	return 0
}

// runBenchDirect is the baseline: what the MVP does today (root process,
// exec with SysProcAttr.Credential), without the helper round trip or stub.
func runBenchDirect(args []string) int {
	fs := flag.NewFlagSet("bench-direct", flag.ExitOnError)
	base := fs.String("base", "", "workspaces base directory")
	srv := fs.String("srv", "", "server-owned directory")
	ws := fs.String("ws", "", "workspace id")
	n := fs.Int("n", 100, "iterations")
	fs.Parse(args)
	u, err := user.Lookup(proto.UserName(*ws))
	if err != nil {
		fmt.Println("bench-direct:", err)
		return 1
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	repo := filepath.Join(*base, *ws, "repo.git")
	tree := filepath.Join(*base, *ws, "tree")
	cps := filepath.Join(*srv, "checkpoints.git")
	asWS := func(stdin []byte, argv ...string) ([]byte, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = tree
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(*base, *ws, "home")}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		return cmd.Output()
	}
	printTimes("direct", "git request (upload-pack --advertise-refs)", *n, func() error {
		_, err := asWS(nil, gitBin, "upload-pack", "--stateless-rpc", "--advertise-refs", repo)
		return err
	})
	printTimes("direct", "capture (pack-objects | index-pack)", *n, func() error {
		pack, err := asWS([]byte("HEAD\n"), gitBin, "-C", tree, "pack-objects", "--revs", "--stdout", "-q")
		if err != nil {
			return err
		}
		ip := exec.Command(gitBin, "--git-dir="+cps, "index-pack", "--stdin", "--fix-thin")
		ip.Stdin = bytes.NewReader(pack)
		return ip.Run()
	})
	printTimes("direct", "spawn /bin/true", *n, func() error {
		_, err := asWS(nil, "/bin/true")
		return err
	})
	return 0
}

func printTimes(mode, name string, n int, f func() error) {
	var ds []time.Duration
	for i := 0; i < n; i++ {
		t := time.Now()
		if err := f(); err != nil {
			fmt.Printf("TIME %-6s %-45s error: %v\n", mode, name, err)
			return
		}
		ds = append(ds, time.Since(t))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p := func(q float64) time.Duration { return ds[int(q*float64(len(ds)-1))] }
	fmt.Printf("TIME %-6s %-45s n=%d p50=%v p95=%v\n", mode, name, n,
		p(0.5).Round(10*time.Microsecond), p(0.95).Round(10*time.Microsecond))
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
