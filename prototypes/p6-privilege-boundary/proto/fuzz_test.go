package proto

import (
	"encoding/binary"
	"testing"
)

// FuzzDecodeRequest drives the request decoder with arbitrary bytes. The
// contract (§11 P6) requires the helper to have no path that executes a
// caller-chosen program as root; a decoder that panics or accepts a malformed
// request would undermine that. The property checked here: DecodeRequest
// never panics, and anything it accepts passes validate() (closed op set,
// constrained workspace id, allowlisted env).
func FuzzDecodeRequest(f *testing.F) {
	seeds := []Request{
		{Op: OpCreateWorkspaceUser, Workspace: "abcd1234"},
		{Op: OpSpawnInWorkspace, Workspace: "abcd1234", Kind: KindPTYShell, Argv: []string{"/bin/bash"}, Env: []string{"TERM=xterm"}},
		{Op: OpSignalWorkspace, Workspace: "abcd1234", Signal: 15},
		{Op: OpSetWorkspaceLimits, Workspace: "abcd1234", MemoryBytes: 1 << 30, PidsMax: 100},
	}
	for _, s := range seeds {
		b := mustFrame(s)
		f.Add(b[4:]) // JSON body only; the harness re-frames
	}
	f.Add([]byte(`{"op":"RunAsRoot","argv":["/bin/sh"]}`))
	f.Add([]byte(`{"op":"SpawnInWorkspace","ws":"../../etc","kind":"pty-shell","argv":["x"]}`))
	f.Add([]byte(`{"op":"SpawnInWorkspace","ws":"abcd1234","kind":"pty-shell","argv":["x"],"env":["LD_PRELOAD=/evil.so"]}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeRequest(body)
		if err != nil {
			if req != nil {
				t.Fatal("non-nil request on error")
			}
			return
		}
		if !validOps[req.Op] {
			t.Fatalf("accepted unknown op %q", req.Op)
		}
		if !ValidWorkspace(req.Workspace) {
			t.Fatalf("accepted invalid workspace %q", req.Workspace)
		}
		if err := req.validate(); err != nil {
			t.Fatalf("accepted request that fails validate(): %v", err)
		}
		if req.Op == OpSpawnInWorkspace {
			for _, e := range req.Env {
				if !validEnv(e) {
					t.Fatalf("accepted non-allowlisted env %q", e)
				}
			}
		}
	})
}

// FuzzReadFrame checks the framing reader never over-allocates or panics.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 'a', 'b', 'c'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ReadFrame(newLimitedReader(b))
	})
}

func mustFrame(v any) []byte {
	buf := make([]byte, 4)
	body := mustJSON(v)
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	return append(buf, body...)
}

func TestClosedOpSet(t *testing.T) {
	// A request naming a program to run as root has no representation.
	for _, body := range []string{
		`{"op":"Exec","argv":["/bin/sh"]}`,
		`{"op":"CreateWorkspaceUser","ws":"abcd1234","extra":"x"}`, // unknown field
		`{"op":"SpawnInWorkspace","ws":"abcd1234","kind":"root-shell","argv":["x"]}`,
		`{"op":"DeleteWorkspaceUser","ws":"root"}`,
		`{"op":"PrepareWorkspaceDirs","ws":"../etc"}`,
	} {
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("decoder accepted forbidden request: %s", body)
		}
	}
}
