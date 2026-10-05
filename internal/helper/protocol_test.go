package helper

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The operation table is exactly the v0.1 set of contract §2.5, plus the
// data-directory upgrade operation (documented in the contract with the
// privilege split). ComposeUp/Down/Ps and ConfigurePortProxy are later
// phases. Adding an operation must be a deliberate contract change.
func TestOperationTableIsContractSet(t *testing.T) {
	want := []Op{
		"CreateWorkspaceUser",
		"DeleteWorkspaceUser",
		"PrepareWorkspaceDirs",
		"RepairDataOwnership",
		"SetWorkspaceLimits",
		"SignalWorkspace",
		"SpawnInWorkspace",
	}
	if got := Operations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operation table = %v, want %v", got, want)
	}
}

func TestProcessKinds(t *testing.T) {
	want := []Kind{"code-server", "git-service", "pty-shell", "runtime-command", "ssh-session"}
	if got := Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for _, k := range []Kind{KindCodeServer, KindSSHSession} {
		if kinds[k] {
			t.Errorf("%s should be reserved, not implemented", k)
		}
	}
}

// No operation takes a program to run as root: the only request carrying
// argv is SpawnInWorkspace, which always runs as the workspace user.
func TestOnlySpawnCarriesArgv(t *testing.T) {
	for op, spec := range ops {
		if spec.spawn && op != OpSpawnInWorkspace {
			t.Errorf("%s accepts spawn arguments", op)
		}
	}
}

const ws = "06GGQXZ2K8M4T6V1R3P5N7B9CD"

func TestDecodeRejects(t *testing.T) {
	cases := map[string]string{
		"unknown op":        `{"op":"RunAsRoot","workspace":"` + ws + `"}`,
		"unknown field":     `{"op":"CreateWorkspaceUser","workspace":"` + ws + `","command":"sh"}`,
		"unknown nested":    `{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"git-service","argv":["git"],"cwd":"/x","uid":0}}`,
		"missing workspace": `{"op":"CreateWorkspaceUser"}`,
		"bad workspace":     `{"op":"CreateWorkspaceUser","workspace":"../../etc"}`,
		"extra params":      `{"op":"CreateWorkspaceUser","workspace":"` + ws + `","limits":{}}`,
		"repair with ws":    `{"op":"RepairDataOwnership","workspace":"` + ws + `"}`,
		"unknown kind":      `{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"root-shell","argv":["sh"],"cwd":"/x"}}`,
		"empty argv":        `{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"git-service","argv":[],"cwd":"/x"}}`,
		"relative cwd":      `{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"git-service","argv":["git"],"cwd":"x"}}`,
		"unclean cwd":       `{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"git-service","argv":["git"],"cwd":"/a/../b"}}`,
		"bad signal":        `{"op":"SignalWorkspace","workspace":"` + ws + `","signal":{"handle":"all","signal":0}}`,
		"bad handle":        `{"op":"SignalWorkspace","workspace":"` + ws + `","signal":{"handle":"-1","signal":15}}`,
		"negative limit":    `{"op":"SetWorkspaceLimits","workspace":"` + ws + `","limits":{"pids":-1}}`,
		"trailing data":     `{"op":"RepairDataOwnership"}{}`,
		"not an object":     `[]`,
	}
	for name, body := range cases {
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
}

func TestDecodeAccepts(t *testing.T) {
	for _, body := range []string{
		`{"op":"CreateWorkspaceUser","workspace":"` + ws + `"}`,
		`{"op":"RepairDataOwnership"}`,
		`{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"pty-shell","argv":["/bin/bash","-l"],"cwd":"/d/workspaces/` + ws + `/tree","cols":80,"rows":24}}`,
		`{"op":"SignalWorkspace","workspace":"` + ws + `","signal":{"handle":"all","signal":15}}`,
		`{"op":"SetWorkspaceLimits","workspace":"` + ws + `","limits":{"cpu_milli":500,"memory_bytes":1073741824,"pids":256}}`,
	} {
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("rejected %s: %v", body, err)
		}
	}
}

func TestEnvAllowlist(t *testing.T) {
	for kv, ok := range map[string]bool{
		"TERM=xterm": true, "GIT_DIR=/x": true, "LC_ALL=C": true, "ARMAGEDDON_WORKSPACE=x": true,
		"LD_PRELOAD=/tmp/x.so": false, "PATH=/tmp": false, "HOME=/root": false, "GODEBUG=x": false,
		"BASH_ENV=/tmp/x": false, "noequals": false, "=x": false,
	} {
		if EnvAllowed(kv) != ok {
			t.Errorf("EnvAllowed(%q) = %v, want %v", kv, !ok, ok)
		}
	}
}

func TestUserName(t *testing.T) {
	n := UserName(ws)
	if !ValidUserName(n) || n != "ws-"+strings.ToLower(ws[len(ws)-14:]) {
		t.Fatalf("UserName = %q", n)
	}
	for _, bad := range []string{"root", "ws-", "ws-ABCDEFGH", "ws-abc", "ws-abcdefgh;", "armageddon"} {
		if ValidUserName(bad) {
			t.Errorf("ValidUserName(%q) = true", bad)
		}
	}
}

func TestFrameRoundTrip(t *testing.T) {
	req := &Request{Op: OpSignalWorkspace, Workspace: ws, Signal: &SignalArgs{Handle: "all", Signal: 15}}
	b, err := Frame(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := ReadFrame(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(body)
	if err != nil || !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// Oversized frames are refused before allocation.
	if _, err := ReadFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("accepted a 4 GiB frame header")
	}
}

// FuzzDecodeRequest: the decoder never panics, and whatever it accepts is
// a known operation with a valid parameter set that survives re-encoding.
func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"op":"CreateWorkspaceUser","workspace":"` + ws + `"}`))
	f.Add([]byte(`{"op":"SpawnInWorkspace","workspace":"` + ws + `","spawn":{"kind":"git-service","argv":["git","status"],"env":["GIT_DIR=/x"],"cwd":"/d"}}`))
	f.Add([]byte(`{"op":"SignalWorkspace","workspace":"` + ws + `","signal":{"handle":"all","signal":9}}`))
	f.Add([]byte(`{"op":"SetWorkspaceLimits","workspace":"` + ws + `","limits":{"pids":5}}`))
	f.Add([]byte(`{"op":"RepairDataOwnership"}`))
	f.Add([]byte(`{"op":"SpawnInWorkspace","spawn":null}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := DecodeRequest(data)
		if err != nil {
			return
		}
		if _, ok := ops[req.Op]; !ok {
			t.Fatalf("accepted unknown op %q", req.Op)
		}
		if err := req.Validate(); err != nil {
			t.Fatalf("accepted invalid request: %v", err)
		}
		if req.Spawn != nil && req.Op != OpSpawnInWorkspace {
			t.Fatalf("argv on %s", req.Op)
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		again, err := DecodeRequest(b)
		if err != nil {
			t.Fatalf("re-encoded request rejected: %v (%s)", err, b)
		}
		if !reflect.DeepEqual(normalize(again), normalize(req)) {
			t.Fatalf("round trip changed the request: %s", b)
		}
	})
}

// normalize treats nil and empty slices alike (omitempty).
func normalize(r *Request) *Request {
	c := *r
	if c.Spawn != nil {
		s := *c.Spawn
		if len(s.Env) == 0 {
			s.Env = nil
		}
		c.Spawn = &s
	}
	return &c
}
