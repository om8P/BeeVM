// Package agentproto holds the types and constants shared by the host (internal/vmm) and
// the guest agent (internal/agent), so the two sides can never disagree about the protocol.
//
// The protocol is plain HTTP/1.1 with JSON bodies, carried over vsock:
//
//	GET  /health    -> HealthResponse
//	PUT  /function  -> body is the raw function binary; replies UploadResponse
//	POST /invoke    -> InvokeRequest in, InvokeResponse out
//	POST /resume    -> ResumeRequest in, ResumeResponse out (after a snapshot restore)
//	PUT  /package   -> body is a zip package (Python/Node.js); replies UploadResponse
//	POST /load      -> LoadRequest in, LoadResponse out (start the runtime, import the code)
package agentproto

import (
	"encoding/json"
	"time"
)

// Port is the vsock port the guest agent listens on.
const Port = 10000

// Paths of the endpoints.
const (
	PathHealth   = "/health"
	PathFunction = "/function"
	PathInvoke   = "/invoke"
	PathResume   = "/resume"
	PathPackage  = "/package"
	PathLoad     = "/load"
)

// FunctionPath is where the agent stores the uploaded function inside the guest. /run is a
// tmpfs (RAM), so the binary never touches the disk image and vanishes with the VM.
const FunctionPath = "/run/beevm/function"

// Limits (first version; refined in later milestones).
const (
	MaxUploadBytes  = 50 << 20 // function binary
	MaxPayloadBytes = 6 << 20  // request payload
	MaxStdoutBytes  = 6 << 20  // function output
	MaxStderrBytes  = 64 << 10 // only the last 64 KB of stderr is kept

	DefaultTimeout = 10 * time.Second
	MaxTimeout     = 60 * time.Second
)

// HealthResponse is returned by GET /health. The host polls it to learn the VM is ready.
type HealthResponse struct {
	Status       string `json:"status"` // "ok"
	AgentVersion string `json:"agent_version"`
	UptimeMS     int64  `json:"uptime_ms"`
}

// UploadResponse is returned by PUT /function.
type UploadResponse struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// InvokeRequest is the body of POST /invoke.
type InvokeRequest struct {
	Payload   json.RawMessage `json:"payload"`              // any JSON; goes to the function's stdin
	TimeoutMS int64           `json:"timeout_ms,omitempty"` // 0 means DefaultTimeout
}

// InvokeResponse is the result of POST /invoke. It is sent with HTTP 200 even when the
// function fails: the agent worked, the function did not. Check Error.
type InvokeResponse struct {
	Output     json.RawMessage `json:"output,omitempty"` // the function's stdout, when it is valid JSON
	Stdout     string          `json:"stdout,omitempty"` // only set when stdout was NOT valid JSON (first 4 KB)
	Stderr     string          `json:"stderr,omitempty"` // last 64 KB of stderr (the function's logs)
	ExitCode   int             `json:"exit_code"`        // -1 if it never started or was killed
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"` // empty means success
}

// ResumeRequest is sent once right after a VM was restored from a snapshot: the guest's
// clock is behind and its random-number generator is a copy of every other restored VM's.
type ResumeRequest struct {
	UnixNano int64  `json:"unix_nano"` // the host's current time
	Entropy  []byte `json:"entropy"`   // fresh random bytes from the host (base64 in JSON)
}

// ResumeResponse says how far the guest clock was moved.
type ResumeResponse struct {
	ClockAdjustedMS int64 `json:"clock_adjusted_ms"`
}

// Runtimes. A "binary" function is one executable started per call. "python" and "node"
// functions are zip packages that a long-lived runtime shim imports once and then calls.
const (
	RuntimeBinary = "binary"
	RuntimePython = "python"
	RuntimeNode   = "node"
)

// CodeDir is where the agent unpacks a zip package inside the guest (tmpfs, like FunctionPath).
const CodeDir = "/run/beevm/code"

// Package limits. The package is unpacked into RAM inside a VM that may only have 128 MiB,
// so the unpacked size is much smaller than the upload limit allows for binaries.
const (
	MaxUnpackedBytes  = 32 << 20
	MaxPackageEntries = 2000

	DefaultLoadTimeout = 30 * time.Second // starting the runtime + importing the code
)

// LoadRequest is the body of POST /load. It starts the runtime's shim if it is not running
// and, when Handler is set, imports the package in CodeDir and looks up the handler.
// An empty Handler only starts the interpreter (that is how runtime snapshots are built).
type LoadRequest struct {
	Runtime   string `json:"runtime"`              // RuntimePython or RuntimeNode
	Handler   string `json:"handler,omitempty"`    // "module.function" (python) or "file.function" (node)
	TimeoutMS int64  `json:"timeout_ms,omitempty"` // 0 means DefaultLoadTimeout
}

// LoadResponse is the result of a successful POST /load. A failing import is answered with
// HTTP 422 and an ErrorResponse that holds the error (traceback or stack).
type LoadResponse struct {
	LoadMS int64  `json:"load_ms"`
	Logs   string `json:"logs,omitempty"` // anything the module printed while importing
}

// ErrorResponse is the body of 4xx/5xx replies (problems with the request or the agent itself).
type ErrorResponse struct {
	Error string `json:"error"`
}
