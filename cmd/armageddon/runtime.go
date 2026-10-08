package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/callmehalpha/Armageddon-/internal/agent"
)

// `armageddon runtime|compose|ports` (plan M8): runtimes, Compose services
// and ports of a workspace's server seat, through the server API.

const runtimeUsage = `usage:
  armageddon runtime [status]                    what the workspace runs, its processes and ports
  armageddon runtime install [--no-follow]       install the toolchain and dependencies on the server
  armageddon runtime start [--port N] [--follow] start the dev server on the server seat
  armageddon runtime stop [install]              stop the dev server (or the install)
  armageddon runtime logs [dev|install] [--follow]
  (each takes --workspace WS; default: the replica containing the current directory)`

const composeUsage = `usage: armageddon compose up [--file F] | down | ps   [--workspace WS]`

type procInfo struct {
	Name      string   `json:"name"`
	Argv      []string `json:"argv"`
	State     string   `json:"state"`
	StoppedBy string   `json:"stopped_by"`
	Port      int      `json:"port"`
	ExitCode  *int     `json:"exit_code"`
	Error     string   `json:"error"`
	Health    string   `json:"health"`
}

type portInfo struct {
	Port    int    `json:"port"`
	Address string `json:"address"`
	Source  string `json:"source"`
	Service string `json:"service"`
	Path    string `json:"path"`
}

func runtimeCmd(ctx context.Context, args []string) error {
	verb := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("runtime "+verb, flag.ExitOnError)
	wsRef := fs.String("workspace", "", "workspace (default: the replica containing the current directory)")
	port := fs.Int("port", 0, "start: the port the dev server listens on (default: the framework's)")
	follow := fs.Bool("follow", false, "keep printing the output")
	noFollow := fs.Bool("no-follow", false, "install: return at once instead of following the output")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, runtimeUsage) }
	fs.Parse(reorder(args))
	return withClient(func(c *agent.Client) error {
		id, err := workspaceArg(c, *wsRef, nil)
		if err != nil {
			return err
		}
		base := "/api/workspaces/" + id
		switch verb {
		case "status":
			return runtimeStatus(c, base, os.Stdout)
		case "install":
			if err := postJSON(c, base+"/runtime/install", nil, nil); err != nil {
				return err
			}
			if *noFollow {
				fmt.Println("Installing on the server; follow with `armageddon runtime logs install --follow`.")
				return nil
			}
			p, err := followLogs(ctx, c, base, "install", true, os.Stdout)
			if err != nil {
				return err
			}
			if p.ExitCode == nil || *p.ExitCode != 0 {
				return fmt.Errorf("the install failed (see the output above)")
			}
			return nil
		case "start":
			var body any
			if *port > 0 {
				body = map[string]int{"port": *port}
			}
			var p procInfo
			if err := postJSON(c, base+"/runtime/start", body, &p); err != nil {
				return err
			}
			fmt.Printf("Started the dev server on the server seat (port %d).\n", p.Port)
			if *follow {
				_, err := followLogs(ctx, c, base, "dev", true, os.Stdout)
				return err
			}
			return waitServing(ctx, c, base, p.Port, os.Stdout)
		case "stop":
			name := "dev"
			if fs.NArg() > 0 {
				name = fs.Arg(0)
			}
			if err := postJSON(c, base+"/runtime/stop", map[string]string{"name": name}, nil); err != nil {
				return err
			}
			fmt.Printf("Stopped %s.\n", name)
			return nil
		case "logs":
			name := "dev"
			if fs.NArg() > 0 {
				name = fs.Arg(0)
			}
			_, err := followLogs(ctx, c, base, name, *follow, os.Stdout)
			return err
		}
		return fmt.Errorf("%s", runtimeUsage)
	})
}

func postJSON(c *agent.Client, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	return c.Do("POST", path, body, out)
}

func runtimeStatus(c *agent.Client, base string, out io.Writer) error {
	var st struct {
		Plan *struct {
			Provider       string   `json:"provider"`
			Framework      string   `json:"framework"`
			PackageManager string   `json:"package_manager"`
			Version        string   `json:"version"`
			VersionFrom    string   `json:"version_from"`
			Start          []string `json:"start"`
			Port           int      `json:"port"`
			Notes          []string `json:"notes"`
			Toolchain      struct {
				Name, Version, Source string
			} `json:"toolchain"`
		} `json:"plan"`
		PlanError        string     `json:"plan_error"`
		ServerHoldsLease bool       `json:"server_holds_lease"`
		Processes        []procInfo `json:"processes"`
		Ports            []portInfo `json:"ports"`
	}
	if err := c.Do("GET", base+"/runtime", nil, &st); err != nil {
		return err
	}
	if p := st.Plan; p != nil {
		fw := p.Provider
		if p.Framework != "" {
			fw += " (" + p.Framework + ")"
		}
		fmt.Fprintf(out, "Runtime:     %s\n", fw)
		tc := fmt.Sprintf("%s %s, %s", p.Toolchain.Name, p.Toolchain.Version, map[string]string{
			"system": "the server's", "workspace": "installed in the workspace", "install": "to install", "missing": "missing"}[p.Toolchain.Source])
		if p.VersionFrom != "" {
			tc += fmt.Sprintf(" (%s asks for %s)", p.VersionFrom, p.Version)
		}
		fmt.Fprintf(out, "Toolchain:   %s\n", tc)
		if p.PackageManager != "" {
			fmt.Fprintf(out, "Packages:    %s\n", p.PackageManager)
		}
		if len(p.Start) > 0 {
			fmt.Fprintf(out, "Dev server:  %s (port %d)\n", strings.Join(p.Start, " "), p.Port)
		}
		for _, n := range p.Notes {
			fmt.Fprintf(out, "Note:        %s\n", n)
		}
	} else {
		fmt.Fprintf(out, "Runtime:     %s\n", st.PlanError)
	}
	if !st.ServerHoldsLease {
		fmt.Fprintln(out, "A device writes this workspace: dev processes on the server are stopped until it is handed back (`armageddon work remote --restart`).")
	}
	if len(st.Processes) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PROCESS\tSTATE\tDETAIL")
		for _, p := range st.Processes {
			detail := ""
			switch {
			case p.State == "running" && p.Health != "":
				detail = fmt.Sprintf("port %d, %s", p.Port, p.Health)
			case p.StoppedBy != "":
				detail = "stopped by " + p.StoppedBy
			case p.ExitCode != nil:
				detail = fmt.Sprintf("exit %d", *p.ExitCode)
			}
			if p.Error != "" {
				detail += " " + p.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.State, detail)
		}
		tw.Flush()
	}
	printPorts(st.Ports, c, out)
	return nil
}

func printPorts(ports []portInfo, c *agent.Client, out io.Writer) {
	if len(ports) == 0 {
		return
	}
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tFROM\tURL (signed-in members only)")
	for _, p := range ports {
		from := p.Source
		if p.Service != "" {
			from += " " + p.Service
		}
		url := p.Path
		if strings.HasPrefix(url, "/") {
			url = strings.TrimRight(c.Cfg.Server, "/") + url
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\n", p.Port, from, url)
	}
	tw.Flush()
}

// followLogs prints a process's output; with follow, until the process
// ends. It returns the process as last seen.
func followLogs(ctx context.Context, c *agent.Client, base, name string, follow bool, out io.Writer) (*procInfo, error) {
	var offset int64
	for {
		var r struct {
			Data    string   `json:"data"`
			Offset  int64    `json:"offset"`
			Process procInfo `json:"process"`
		}
		q := fmt.Sprintf("%s/runtime/logs?name=%s&offset=%d", base, name, offset)
		if follow {
			q += "&wait=1"
		}
		if err := c.Do("GET", q, nil, &r); err != nil {
			return nil, err
		}
		io.WriteString(out, r.Data)
		offset = r.Offset
		if !follow || (r.Process.State != "running" && r.Data == "") {
			if follow && r.Process.State != "running" {
				switch {
				case r.Process.StoppedBy != "":
					fmt.Fprintf(out, "[%s stopped by %s]\n", name, r.Process.StoppedBy)
				case r.Process.ExitCode != nil:
					fmt.Fprintf(out, "[%s exited with %d]\n", name, *r.Process.ExitCode)
				}
			}
			return &r.Process, nil
		}
		select {
		case <-ctx.Done():
			return &r.Process, nil
		default:
		}
	}
}

// waitServing waits for the dev server to answer and prints its URL.
func waitServing(ctx context.Context, c *agent.Client, base string, port int, out io.Writer) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			Processes []procInfo `json:"processes"`
			Ports     []portInfo `json:"ports"`
		}
		if err := c.Do("GET", base+"/runtime", nil, &st); err != nil {
			return err
		}
		for _, p := range st.Processes {
			if p.Name != "dev" {
				continue
			}
			if p.State != "running" {
				fmt.Fprintln(out, "The dev server stopped; its output:")
				_, err := followLogs(ctx, c, base, "dev", false, out)
				if err == nil {
					err = fmt.Errorf("the dev server exited")
				}
				return err
			}
			if p.Health == "healthy" {
				var mine []portInfo
				for _, pt := range st.Ports {
					if pt.Port == port {
						mine = append(mine, pt)
					}
				}
				printPorts(mine, c, out)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	fmt.Fprintf(out, "Still starting after 90 s; check `armageddon runtime logs --follow`.\n")
	return nil
}

func composeCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", composeUsage)
	}
	verb := args[0]
	fs := flag.NewFlagSet("compose "+verb, flag.ExitOnError)
	wsRef := fs.String("workspace", "", "workspace (default: the replica containing the current directory)")
	file := fs.String("file", "", "up: the Compose file, relative to the workspace (default: Compose's own default)")
	fs.Parse(reorder(args[1:]))
	return withClient(func(c *agent.Client) error {
		id, err := workspaceArg(c, *wsRef, nil)
		if err != nil {
			return err
		}
		base := "/api/workspaces/" + id + "/compose"
		var r struct {
			Project  string `json:"project"`
			Warnings []string
			Services []struct {
				Service, State, Status, Health string
				Ports                          []struct {
					HostIP    string `json:"host_ip"`
					Published int    `json:"published"`
					Target    int    `json:"target"`
				}
			} `json:"services"`
		}
		switch verb {
		case "up":
			fmt.Println("Checking the Compose file and starting its services on the server…")
			if err := postJSON(c, base+"/up", map[string]string{"file": *file}, &r); err != nil {
				return err
			}
		case "down":
			if err := postJSON(c, base+"/down", nil, &r); err != nil {
				return err
			}
			fmt.Printf("Stopped %s (volumes kept).\n", r.Project)
			return nil
		case "ps":
			if err := c.Do("GET", base, nil, &r); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s", composeUsage)
		}
		for _, w := range r.Warnings {
			fmt.Println("note:", w)
		}
		if len(r.Services) == 0 {
			fmt.Println("No Compose services.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tSTATE\tPORTS")
		for _, s := range r.Services {
			var ports []string
			for _, p := range s.Ports {
				ports = append(ports, fmt.Sprintf("%s:%d->%d", p.HostIP, p.Published, p.Target))
			}
			state := s.State
			if s.Health != "" {
				state += " (" + s.Health + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Service, state, strings.Join(ports, ", "))
		}
		return tw.Flush()
	})
}

func portsCmd(args []string) error {
	fs := flag.NewFlagSet("ports", flag.ExitOnError)
	wsRef := fs.String("workspace", "", "workspace (default: the replica containing the current directory)")
	fs.Parse(reorder(args))
	return withClient(func(c *agent.Client) error {
		id, err := workspaceArg(c, *wsRef, fs.Args())
		if err != nil {
			return err
		}
		var ports []portInfo
		if err := c.Do("GET", "/api/workspaces/"+id+"/ports", nil, &ports); err != nil {
			return err
		}
		if len(ports) == 0 {
			fmt.Println("Nothing in the workspace listens on a port.")
			return nil
		}
		printPorts(ports, c, os.Stdout)
		return nil
	})
}
