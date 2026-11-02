// Package apitypes holds the JSON types of BeeVM's public HTTP API and the header names it
// uses. The server (internal/api) and the clients (internal/apiclient, cmd/beevm) both import
// it, so neither side needs the other's dependencies and they cannot disagree.
package apitypes

import "time"

// Response headers added to a successful /invoke, so timings are visible without changing the
// function's own output.
const (
	HeaderBootMS     = "X-BeeVM-Boot-Ms"     // time until the VM's agent answered
	HeaderFunctionMS = "X-BeeVM-Function-Ms" // time the function itself ran
	HeaderTotalMS    = "X-BeeVM-Total-Ms"    // whole request, inside the server
	HeaderVMID       = "X-BeeVM-VM-Id"
	HeaderCodeCache  = "X-BeeVM-Code-Cache" // "hit" (code was on the node) or "miss" (fetched from the artifact store)
	HeaderFetchMS    = "X-BeeVM-Fetch-Ms"   // time spent fetching the code; 0 on a hit
	HeaderStart      = "X-BeeVM-Start"      // how the VM started: "warm" (reused from the pool), "restore" (from a snapshot) or "boot"
	HeaderUploadMS   = "X-BeeVM-Upload-Ms"  // time spent sending the code into the VM
)

// Function describes a deployed function.
type Function struct {
	Name       string    `json:"name"`
	Runtime    string    `json:"runtime"`
	CodeSHA256 string    `json:"code_sha256"`
	CodeSize   int64     `json:"code_size"`
	MemoryMiB  int64     `json:"memory_mib"`
	VCPUs      int64     `json:"vcpus"`
	TimeoutMS  int64     `json:"timeout_ms"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// FunctionConfig is the optional "config" part of a deploy. Zero fields mean "use the default".
type FunctionConfig struct {
	MemoryMiB int64 `json:"memory_mib,omitempty"`
	VCPUs     int64 `json:"vcpus,omitempty"`
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// FunctionList is the answer to GET /api/v1/functions.
type FunctionList struct {
	Functions []Function `json:"functions"`
}

// PrewarmResult is the answer to POST /api/v1/functions/{name}/prewarm.
type PrewarmResult struct {
	Requested int `json:"requested"`
	Started   int `json:"started"` // fewer than requested when the node had no more free slots
}

// VM is one of the caller's MicroVMs in the warm pool.
type VM struct {
	ID       string    `json:"id"`
	Function string    `json:"function"` // empty if the function was just removed
	State    string    `json:"state"`    // restoring, idle, busy, recycling
	Since    time.Time `json:"since"`    // when it entered State
	LastUsed time.Time `json:"last_used"`
	Calls    int64     `json:"calls"`
}

// VMList is the answer to GET /api/v1/vms.
type VMList struct {
	VMs []VM `json:"vms"`
}

// Error is the body of every error answer. For a failed function (HTTP 502) ExitCode and
// Stderr say what happened inside the VM.
type Error struct {
	Error    string `json:"error"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}
