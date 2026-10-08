package helper

import (
	"encoding/json"
	"strings"
	"testing"
)

const project = "arm-01hzx0abcdefgh"

// goodCompose is `docker compose -p arm-… config --format json` for a
// Postgres and Redis project, as Compose v2 prints it.
const goodCompose = `{
  "name": "arm-01hzx0abcdefgh",
  "networks": {"default": {"name": "arm-01hzx0abcdefgh_default", "ipam": {}}},
  "services": {
    "db": {
      "command": null, "entrypoint": null,
      "environment": {"POSTGRES_PASSWORD": "dev"},
      "image": "postgres:16",
      "networks": {"default": null},
      "ports": [{"mode": "ingress", "target": 5432, "published": "5432", "protocol": "tcp"}],
      "volumes": [{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data", "volume": {}}],
      "healthcheck": {"test": ["CMD", "pg_isready"], "interval": "5s"},
      "x-note": "dropped"
    },
    "cache": {
      "command": null, "entrypoint": null, "image": "redis:7",
      "networks": {"default": null},
      "ports": [{"mode": "ingress", "host_ip": "0.0.0.0", "target": 6379, "published": "6379", "protocol": "tcp"}],
      "depends_on": {"db": {"condition": "service_healthy", "required": true}},
      "tmpfs": ["/tmp"],
      "deploy": {"resources": {"limits": {"memory": "268435456"}}}
    }
  },
  "volumes": {"pgdata": {"name": "arm-01hzx0abcdefgh_pgdata"}},
  "x-top": 1
}`

func TestNormalizeComposeAcceptsServices(t *testing.T) {
	out, warns, err := NormalizeCompose([]byte(goodCompose), project)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(out, &doc)
	svcs := doc["services"].(map[string]any)
	for _, name := range []string{"db", "cache"} {
		for _, p := range svcs[name].(map[string]any)["ports"].([]any) {
			if ip := p.(map[string]any)["host_ip"]; ip != "127.0.0.1" {
				t.Errorf("%s: port bound to %v, want 127.0.0.1", name, ip)
			}
		}
	}
	if _, ok := svcs["db"].(map[string]any)["x-note"]; ok {
		t.Error("extension fields were kept")
	}
	if _, ok := doc["x-top"]; ok {
		t.Error("top-level extension fields were kept")
	}
	if doc["name"] != project {
		t.Errorf("project name %v", doc["name"])
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "0.0.0.0") {
		t.Errorf("warnings: %q", warns)
	}
}

// TestNormalizeComposeRejects is the §2.5 rejection list (plan M8.4): each
// case must be refused with an error that names the problem.
func TestNormalizeComposeRejects(t *testing.T) {
	cases := []struct {
		name, service, top, want string
	}{
		{"privileged", `"privileged": true`, "", "privileged"},
		{"cap_add", `"cap_add": ["SYS_ADMIN"]`, "", "cap_add"},
		{"host network", `"network_mode": "host"`, "", "network_mode"},
		{"other container's network", `"network_mode": "container:abc"`, "", "network_mode"},
		{"host pid", `"pid": "host"`, "", "pid"},
		{"host ipc", `"ipc": "host"`, "", "ipc"},
		{"userns host", `"userns_mode": "host"`, "", "userns_mode"},
		{"devices", `"devices": [{"source": "/dev/kvm", "target": "/dev/kvm"}]`, "", "devices"},
		{"security_opt", `"security_opt": ["seccomp=unconfined"]`, "", "security_opt"},
		{"bind mount of the host", `"volumes": [{"type": "bind", "source": "/", "target": "/host"}]`, "", "bind mounts"},
		{"bind mount inside the workspace", `"volumes": [{"type": "bind", "source": "/var/lib/armageddon/workspaces/x/tree", "target": "/app"}]`, "", "bind mounts"},
		{"docker socket", `"volumes": [{"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}]`, "", "Docker socket"},
		{"build", `"build": {"context": "."}`, "", "build"},
		{"container_name", `"container_name": "postgres"`, "", "container_name"},
		{"lifecycle hook", `"post_start": [{"command": "id", "privileged": true}]`, "", "post_start"},
		{"provider", `"provider": {"type": "evil"}`, "", "provider"},
		{"unknown key", `"oom_score_adj": -1000`, "", "oom_score_adj"},
		{"external volume", "", `"volumes": {"v": {"external": true, "name": "arm-other_v"}}`, "external"},
		{"bind volume via driver_opts", "", `"volumes": {"v": {"name": "` + project + `_v", "driver_opts": {"type": "none", "o": "bind", "device": "/etc"}}}`, "driver_opts"},
		{"another workspace's volume", "", `"volumes": {"v": {"name": "arm-other_pgdata"}}`, "custom name"},
		{"host network driver", "", `"networks": {"n": {"name": "` + project + `_n", "driver": "host"}}`, "driver"},
		{"secrets", "", `"secrets": {"s": {"file": "/etc/shadow"}}`, "secrets"},
		{"undeclared volume", `"volumes": [{"type": "volume", "source": "arm-other_pgdata", "target": "/d"}]`, "", "not declared"},
		{"volumes_from a container", `"volumes_from": ["container:victim"]`, "", "volumes_from"},
		{"syslog logging", `"logging": {"driver": "syslog"}`, "", "logging"},
		{"compose label", `"labels": {"com.docker.compose.project": "arm-other"}`, "", "reserved"},
		{"custom subnet", "", `"networks": {"n": {"name": "` + project + `_n", "ipam": {"config": [{"subnet": "10.0.0.0/8"}]}}}`, "ipam"},
		{"device reservation", `"deploy": {"resources": {"reservations": {"devices": [{"capabilities": ["gpu"]}]}}}`, "", "devices"},
	}
	for _, c := range cases {
		svc := `"image": "alpine"`
		if c.service != "" {
			svc += ", " + c.service
		}
		doc := `{"name": "x", "services": {"app": {` + svc + `}}`
		if c.top != "" {
			doc += ", " + c.top
		}
		doc += "}"
		_, _, err := NormalizeCompose([]byte(doc), project)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

func TestComposeRequestValidation(t *testing.T) {
	for _, f := range []string{"/etc/compose.yaml", "../other/compose.yaml", "a/../../b", "-f", "a//b"} {
		r := &Request{Op: OpComposeUp, Workspace: "01HZX0ABCDEFGH", Compose: &ComposeArgs{File: f}}
		if err := r.Validate(); err == nil {
			t.Errorf("compose file %q accepted", f)
		}
	}
	for _, f := range []string{"", "compose.yaml", "deploy/dev.compose.yml"} {
		r := &Request{Op: OpComposeUp, Workspace: "01HZX0ABCDEFGH", Compose: &ComposeArgs{File: f}}
		if err := r.Validate(); err != nil {
			t.Errorf("compose file %q: %v", f, err)
		}
	}
	if err := (&Request{Op: OpComposeUp, Workspace: "01HZX0ABCDEFGH"}).Validate(); err == nil {
		t.Error("ComposeUp without its arguments accepted")
	}
	if err := (&Request{Op: OpComposePs, Workspace: "01HZX0ABCDEFGH", Compose: &ComposeArgs{}}).Validate(); err == nil {
		t.Error("ComposePs with compose arguments accepted")
	}
}

func TestParseComposePs(t *testing.T) {
	ndjson := `{"Service":"db","Name":"arm-x-db-1","Image":"postgres:16","State":"running","Status":"Up 3 seconds","Health":"healthy","Publishers":[{"URL":"127.0.0.1","TargetPort":5432,"PublishedPort":5432,"Protocol":"tcp"}]}
{"Service":"cache","Name":"arm-x-cache-1","State":"running","Publishers":[{"URL":"","TargetPort":6379,"PublishedPort":0,"Protocol":"tcp"}]}`
	for _, in := range []string{ndjson, "[" + strings.ReplaceAll(ndjson, "\n", ",") + "]"} {
		svcs, err := parseComposePs([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(svcs) != 2 || svcs[0].Service != "cache" || len(svcs[0].Ports) != 0 || svcs[1].Ports[0].Published != 5432 {
			t.Fatalf("%+v", svcs)
		}
	}
	if svcs, err := parseComposePs([]byte("  \n")); err != nil || len(svcs) != 0 {
		t.Fatalf("empty: %v %v", svcs, err)
	}
}
