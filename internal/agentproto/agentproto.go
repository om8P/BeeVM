// Package agentproto holds the types and constants shared by the host (internal/vmm) and
// the guest agent (internal/agent), so the two sides can never disagree about the protocol.
//
// The protocol is plain HTTP/1.1 with JSON bodies, carried over vsock:
//
//	GET  /health    -> HealthResponse
//	PUT  /function  -> body is the raw function binary; replies UploadResponse
//	POST /invoke    -> InvokeRequest in, InvokeResponse out
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

// ErrorResponse is the body of 4xx/5xx replies (problems with the request or the agent itself).
type ErrorResponse struct {
	Error string `json:"error"`
}
