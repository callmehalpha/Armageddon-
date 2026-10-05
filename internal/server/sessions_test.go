package server

import "testing"

func TestSessionRegistry(t *testing.T) {
	var r sessionRegistry
	var closed []string
	unA := r.Register("ws1", SessionTerminal, "u1", func(reason string) { closed = append(closed, "a:"+reason) })
	r.Register("ws1", SessionCodeServer, "u1", func(reason string) { closed = append(closed, "b:"+reason) })
	r.Register("ws2", SessionTerminal, "u2", func(reason string) { closed = append(closed, "c:"+reason) })
	if c := r.Count("ws1"); c[SessionTerminal] != 1 || c[SessionCodeServer] != 1 {
		t.Fatalf("count = %v", c)
	}
	unA()
	unA() // idempotent
	if n := r.CloseAll("ws1", "handoff"); n != 1 || len(closed) != 1 || closed[0] != "b:handoff" {
		t.Fatalf("CloseAll = %d, closed = %v", n, closed)
	}
	if r.Count("ws2")[SessionTerminal] != 1 {
		t.Fatal("another workspace's session was affected")
	}
}
