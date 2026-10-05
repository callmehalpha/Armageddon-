// Package helper is the privilege boundary of contract §2.5.
//
// The Armageddon server runs as the unprivileged `armageddon` user. Every
// privileged action goes through `armageddon helper`, a root process that
// serves a closed, typed set of operations on a 0600 unix socket reachable
// only by the server uid (checked with SO_PEERCRED). The helper has no
// request type that runs a caller-chosen program as root: SpawnInWorkspace
// always runs as the workspace user, with no_new_privs.
//
// The server talks to the helper through the Client interface. Two
// implementations exist: the socket client (production) and an in-process
// dev client that runs everything as the current user (non-root
// development and unit tests, no isolation).
//
// Wire format: every message is a frame of a 4-byte big-endian length
// followed by that many bytes of JSON. Unknown operations and unknown
// fields are rejected. File descriptors (stdio, PTY masters) travel as
// SCM_RIGHTS ancillary data on the frame that needs them.
package helper

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MaxFrame bounds a single message. Requests are small (argv + env).
const MaxFrame = 256 << 10

// DefaultSocket is where the helper listens unless configured otherwise.
const DefaultSocket = "/run/armageddon/helper.sock"

// Op is a helper operation. The set is closed: see Operations.
type Op string

const (
	OpCreateWorkspaceUser  Op = "CreateWorkspaceUser"
	OpDeleteWorkspaceUser  Op = "DeleteWorkspaceUser"
	OpPrepareWorkspaceDirs Op = "PrepareWorkspaceDirs"
	OpSpawnInWorkspace     Op = "SpawnInWorkspace"
	OpSignalWorkspace      Op = "SignalWorkspace"
	OpSetWorkspaceLimits   Op = "SetWorkspaceLimits"
	// OpRepairDataOwnership is the one-time upgrade of an MVP data
	// directory (written by a root server) to the split layout: root-owned
	// entries under the data directory become owned by the server user.
	// Workspace-owned directories are never entered.
	OpRepairDataOwnership Op = "RepairDataOwnership"
)

// opSpec describes which request fields an operation takes. Anything else
// set on the request is an error.
type opSpec struct {
	workspace bool
	spawn     bool
	signal    bool
	limits    bool
}

var ops = map[Op]opSpec{
	OpCreateWorkspaceUser:  {workspace: true},
	OpDeleteWorkspaceUser:  {workspace: true},
	OpPrepareWorkspaceDirs: {workspace: true},
	OpSpawnInWorkspace:     {workspace: true, spawn: true},
	OpSignalWorkspace:      {workspace: true, signal: true},
	OpSetWorkspaceLimits:   {workspace: true, limits: true},
	OpRepairDataOwnership:  {},
}

// Operations returns the complete operation set, sorted.
func Operations() []Op {
	out := make([]Op, 0, len(ops))
	for op := range ops {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Kind is the class of a workspace process (SpawnInWorkspace).
type Kind string

const (
	KindPTYShell       Kind = "pty-shell"
	KindGitService     Kind = "git-service"
	KindRuntimeCommand Kind = "runtime-command"
	// Reserved for later phases (code-server M4.3, SSH endpoint M4.4).
	// They decode, and are refused as not implemented.
	KindCodeServer Kind = "code-server"
	KindSSHSession Kind = "ssh-session"
)

var kinds = map[Kind]bool{
	KindPTYShell: true, KindGitService: true, KindRuntimeCommand: true,
	KindCodeServer: false, KindSSHSession: false, // false: reserved
}

// Kinds returns every process kind in the enum, implemented or reserved.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ErrReservedKind is returned for process kinds that are in the enum but not
// implemented yet.
var ErrReservedKind = errors.New("process kind reserved for a later phase")

// Request is one helper request.
type Request struct {
	Op        Op          `json:"op"`
	Workspace string      `json:"workspace,omitempty"`
	Spawn     *SpawnArgs  `json:"spawn,omitempty"`
	Signal    *SignalArgs `json:"signal,omitempty"`
	Limits    *Limits     `json:"limits,omitempty"`
}

// SpawnArgs are the parameters of SpawnInWorkspace. Stdio (non-PTY kinds)
// travels as three SCM_RIGHTS descriptors on the request frame.
type SpawnArgs struct {
	Kind Kind     `json:"kind"`
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"` // filtered by EnvAllowed
	Cwd  string   `json:"cwd"`
	// PTY size for pty-shell.
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// SignalArgs are the parameters of SignalWorkspace.
type SignalArgs struct {
	Handle string `json:"handle"` // a handle from SpawnInWorkspace, or "all"
	Signal int    `json:"signal"`
}

// Limits are cgroup v2 limits for one workspace. Zero means unlimited.
type Limits struct {
	CPUMilli    int64 `json:"cpu_milli,omitempty"` // 1000 = one CPU
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	Pids        int64 `json:"pids,omitempty"`
}

// Response is the helper's reply. A SpawnInWorkspace connection carries a
// first Response (Handle set, PTY master as SCM_RIGHTS for pty-shell) and
// then a second one with Exit set when the process ends.
type Response struct {
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	UID    uint32      `json:"uid,omitempty"`
	GID    uint32      `json:"gid,omitempty"`
	User   string      `json:"user,omitempty"`
	Handle string      `json:"handle,omitempty"`
	Pid    int         `json:"pid,omitempty"`
	Exit   *ExitStatus `json:"exit,omitempty"`
	// Warning carries degraded-mode notes (e.g. no cgroup v2).
	Warning string `json:"warning,omitempty"`
}

// ExitStatus is how a spawned process ended.
type ExitStatus struct {
	Code   int `json:"code"`   // exit code, or -1 if killed by a signal
	Signal int `json:"signal"` // terminating signal, 0 if it exited
}

// ShellCode maps the status onto a shell-style exit code (128+signal).
func (e ExitStatus) ShellCode() int {
	if e.Signal != 0 {
		return 128 + e.Signal
	}
	return e.Code
}

var (
	wsIDRe     = regexp.MustCompile(`^[0-9A-Za-z]{8,40}$`)
	userNameRe = regexp.MustCompile(`^ws-[a-z0-9]{8,24}$`)
	handleRe   = regexp.MustCompile(`^[a-z0-9]{8,32}$`)
	envKeyRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidWorkspaceID reports whether id is a well-formed workspace ID.
func ValidWorkspaceID(id string) bool { return wsIDRe.MatchString(id) }

// ValidUserName reports whether name is a workspace user name.
func ValidUserName(name string) bool { return userNameRe.MatchString(name) }

// UserName derives the OS user name for a workspace ID.
func UserName(workspaceID string) string {
	id := strings.ToLower(workspaceID)
	if len(id) > 14 {
		id = id[len(id)-14:]
	}
	return "ws-" + id
}

// allowedSignals are the signals SignalWorkspace delivers.
var allowedSignals = map[int]bool{1: true, 2: true, 3: true, 9: true, 10: true, 12: true, 15: true, 18: true, 19: true, 28: true}

// EnvAllowed reports whether a caller-supplied environment variable may be
// passed to a workspace process. PATH, HOME, USER, LOGNAME and SHELL are
// always set by the helper itself.
func EnvAllowed(kv string) bool {
	k, _, ok := strings.Cut(kv, "=")
	if !ok || !envKeyRe.MatchString(k) {
		return false
	}
	switch k {
	case "TERM", "LANG", "TZ", "COLORTERM":
		return true
	}
	return strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "ARMAGEDDON_")
}

// Validate checks a decoded request against its operation's schema.
func (r *Request) Validate() error {
	spec, ok := ops[r.Op]
	if !ok {
		return fmt.Errorf("unknown operation %q", r.Op)
	}
	if spec.workspace != (r.Workspace != "") {
		if spec.workspace {
			return fmt.Errorf("%s: workspace is required", r.Op)
		}
		return fmt.Errorf("%s: unexpected workspace", r.Op)
	}
	if spec.workspace && !ValidWorkspaceID(r.Workspace) {
		return fmt.Errorf("%s: invalid workspace ID", r.Op)
	}
	if spec.spawn != (r.Spawn != nil) || spec.signal != (r.Signal != nil) || spec.limits != (r.Limits != nil) {
		return fmt.Errorf("%s: wrong parameter set", r.Op)
	}
	if s := r.Spawn; s != nil {
		if _, ok := kinds[s.Kind]; !ok {
			return fmt.Errorf("unknown process kind %q", s.Kind)
		}
		if len(s.Argv) == 0 || len(s.Argv) > 4096 || s.Argv[0] == "" {
			return errors.New("argv is required")
		}
		for _, a := range s.Argv {
			if strings.IndexByte(a, 0) >= 0 {
				return errors.New("argv contains NUL")
			}
		}
		if len(s.Env) > 512 {
			return errors.New("too many environment variables")
		}
		for _, kv := range s.Env {
			if strings.IndexByte(kv, 0) >= 0 {
				return errors.New("env contains NUL")
			}
		}
		if !filepath.IsAbs(s.Cwd) || filepath.Clean(s.Cwd) != s.Cwd || strings.IndexByte(s.Cwd, 0) >= 0 {
			return errors.New("cwd must be a clean absolute path")
		}
		if s.Kind != KindPTYShell && (s.Cols != 0 || s.Rows != 0) {
			return errors.New("cols/rows only apply to pty-shell")
		}
	}
	if s := r.Signal; s != nil {
		if s.Handle != "all" && !handleRe.MatchString(s.Handle) {
			return errors.New("invalid handle")
		}
		if !allowedSignals[s.Signal] {
			return fmt.Errorf("signal %d not allowed", s.Signal)
		}
	}
	if l := r.Limits; l != nil {
		if l.CPUMilli < 0 || l.MemoryBytes < 0 || l.Pids < 0 {
			return errors.New("limits must not be negative")
		}
	}
	return nil
}

func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after message")
	}
	return nil
}

// DecodeRequest parses and validates one request body (without the length
// prefix). It is the only way the helper reads requests.
func DecodeRequest(body []byte) (*Request, error) {
	if len(body) > MaxFrame {
		return nil, errors.New("message too large")
	}
	var r Request
	if err := decodeStrict(body, &r); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// DecodeResponse parses one response body.
func DecodeResponse(body []byte) (*Response, error) {
	var r Response
	if err := decodeStrict(body, &r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &r, nil
}

// Frame prefixes a JSON body with its length.
func Frame(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFrame {
		return nil, errors.New("message too large")
	}
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	copy(out[4:], body)
	return out, nil
}

// ReadFrame reads one length-prefixed body from r.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	return readBody(r, hdr)
}

func readBody(r io.Reader, hdr [4]byte) ([]byte, error) {
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, fmt.Errorf("frame of %d bytes exceeds the limit", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
