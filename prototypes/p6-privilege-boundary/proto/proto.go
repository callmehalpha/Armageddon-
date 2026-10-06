// Package proto is the wire protocol between the unprivileged server and the
// root helper (contract §2.5). Requests are a closed, typed operation set:
// there is no "run this program as root" request, by construction.
//
// Framing: each message is a 4-byte big-endian length prefix followed by that
// many bytes of JSON. The length is bounded (maxFrame) so a hostile or
// corrupt stream cannot force a huge allocation. JSON keeps the prototype
// legible; the real helper would use a fixed binary encoding, but the framing
// and the closed op set are what matter for the boundary.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxFrame = 1 << 20 // 1 MiB: far more than any request needs

// Op is the operation code. The set is closed and validated on decode.
type Op string

const (
	OpCreateWorkspaceUser  Op = "CreateWorkspaceUser"
	OpDeleteWorkspaceUser  Op = "DeleteWorkspaceUser"
	OpPrepareWorkspaceDirs Op = "PrepareWorkspaceDirs"
	OpSpawnInWorkspace     Op = "SpawnInWorkspace"
	OpSignalWorkspace      Op = "SignalWorkspace"
	OpSetWorkspaceLimits   Op = "SetWorkspaceLimits"
)

var validOps = map[Op]bool{
	OpCreateWorkspaceUser: true, OpDeleteWorkspaceUser: true,
	OpPrepareWorkspaceDirs: true, OpSpawnInWorkspace: true,
	OpSignalWorkspace: true, OpSetWorkspaceLimits: true,
}

// SpawnKind is the closed set of process kinds the helper will spawn.
type SpawnKind string

const (
	KindPTYShell       SpawnKind = "pty-shell"
	KindGitService     SpawnKind = "git-service"
	KindRuntimeCommand SpawnKind = "runtime-command"
)

var validKinds = map[SpawnKind]bool{KindPTYShell: true, KindGitService: true, KindRuntimeCommand: true}

// Request is the single decoded request type. Exactly the fields the Op uses
// are read by the helper; the decoder validates the shape before dispatch.
type Request struct {
	Op          Op        `json:"op"`
	Workspace   string    `json:"ws,omitempty"`
	Kind        SpawnKind `json:"kind,omitempty"`
	Argv        []string  `json:"argv,omitempty"`
	Env         []string  `json:"env,omitempty"` // allowlisted KEY=VALUE only
	Handle      string    `json:"handle,omitempty"`
	Signal      int       `json:"signal,omitempty"`
	WantPTY     bool      `json:"want_pty,omitempty"`
	NumFDs      int       `json:"num_fds,omitempty"` // stdio fds passed via SCM_RIGHTS
	MemoryBytes int64     `json:"memory_bytes,omitempty"`
	PidsMax     int64     `json:"pids_max,omitempty"`
	CPUWeight   int64     `json:"cpu_weight,omitempty"`
}

// Response is the helper's reply.
type Response struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Handle string `json:"handle,omitempty"`
	PID    int    `json:"pid,omitempty"`
	// NumFDs file descriptors accompany the reply (e.g. a PTY master).
	NumFDs int `json:"num_fds,omitempty"`
}

var (
	ErrTooLarge  = errors.New("frame exceeds maximum size")
	ErrUnknownOp = errors.New("unknown operation")
)

// WriteMsg frames and writes a value as JSON.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return ErrTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one length-prefixed frame. It never allocates more than
// maxFrame bytes regardless of the prefix.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, ErrTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// DecodeRequest parses and validates one frame into a Request. This is the
// function the fuzz test drives: it must never panic and must reject anything
// outside the closed operation set and field constraints.
func DecodeRequest(frame []byte) (*Request, error) {
	// DisallowUnknownFields closes the request to fields the helper does not
	// model, so a hostile encoder cannot smuggle extra data past it.
	dec := json.NewDecoder(newLimitedReader(frame))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if !validOps[req.Op] {
		return nil, ErrUnknownOp
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

func (r *Request) validate() error {
	// Every op needs a well-formed workspace id. ValidWorkspace is the single
	// chokepoint; the helper never derives a user name or path any other way.
	if !ValidWorkspace(r.Workspace) {
		return fmt.Errorf("invalid workspace id %q", r.Workspace)
	}
	switch r.Op {
	case OpSpawnInWorkspace:
		if !validKinds[r.Kind] {
			return fmt.Errorf("invalid spawn kind %q", r.Kind)
		}
		if len(r.Argv) == 0 {
			return errors.New("spawn requires argv")
		}
		if len(r.Argv) > 256 {
			return errors.New("argv too long")
		}
		for _, e := range r.Env {
			if !validEnv(e) {
				return fmt.Errorf("env entry not allowlisted: %q", e)
			}
		}
		if r.NumFDs < 0 || r.NumFDs > 8 {
			return errors.New("num_fds out of range")
		}
	case OpSignalWorkspace:
		if r.Signal <= 0 || r.Signal > 64 {
			return fmt.Errorf("invalid signal %d", r.Signal)
		}
	case OpSetWorkspaceLimits:
		if r.MemoryBytes < 0 || r.PidsMax < 0 || r.CPUWeight < 0 {
			return errors.New("negative limit")
		}
	}
	return nil
}

type limitedReader struct {
	b []byte
	i int
}

func newLimitedReader(b []byte) *limitedReader { return &limitedReader{b: b} }

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.i >= len(l.b) {
		return 0, io.EOF
	}
	n := copy(p, l.b[l.i:])
	l.i += n
	return n, nil
}

// DecodeResponse parses a helper response frame.
func DecodeResponse(frame []byte) (Response, error) {
	var r Response
	err := json.Unmarshal(frame, &r)
	return r, err
}
