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
	HeaderSnapshot   = "X-BeeVM-Snapshot"   // for "restore": "function" (code already inside) or "runtime"
	HeaderLoadMS     = "X-BeeVM-Load-Ms"    // python/node: time spent importing the code
)

// Function describes a deployed function.
type Function struct {
	Name       string    `json:"name"`
	Runtime    string    `json:"runtime"`
	Handler    string    `json:"handler,omitempty"`
	CodeSHA256 string    `json:"code_sha256"`
	CodeSize   int64     `json:"code_size"`
	MemoryMiB  int64     `json:"memory_mib"`
	VCPUs      int64     `json:"vcpus"`
	TimeoutMS  int64     `json:"timeout_ms"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// FunctionConfig is the optional "config" part of a deploy. Zero fields mean "use the default":
// runtime "binary", the runtime's default handler (python: handler.handler, node:
// index.handler). Python and Node.js code is uploaded as a zip package.
type FunctionConfig struct {
	Runtime   string `json:"runtime,omitempty"`
	Handler   string `json:"handler,omitempty"`
	MemoryMiB int64  `json:"memory_mib,omitempty"`
	VCPUs     int64  `json:"vcpus,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
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
	Addr     string    `json:"addr,omitempty"` // network address for the tenant's other VMs (isolated servers only)
	// PrevID is only set in "vm" events: the VM had this ID before (a "pending-N" placeholder
	// while it was being created). Replace that entry.
	PrevID string `json:"prev_id,omitempty"`
}

// VMList is the answer to GET /api/v1/vms.
type VMList struct {
	VMs []VM `json:"vms"`
}

// Event stream (GET /api/v1/events, Server-Sent Events). Each message has an "event:" line
// with one of these names and a "data:" line with JSON:
const (
	EventHello  = "hello"  // Hello: the full picture; sent first, and again if the stream fell behind
	EventVM     = "vm"     // VM: one VM changed state (State "destroyed" = remove it)
	EventInvoke = "invoke" // InvokeEvent: a call finished
)

// Hello is the "hello" event: the caller's VMs right now.
type Hello struct {
	VMs    []VM `json:"vms"`
	MaxVMs int  `json:"max_vms"` // VMs on the whole node at most (0 = no warm pool)
}

// InvokeEvent is the "invoke" event: one finished call of a deployed function.
type InvokeEvent struct {
	Function   string    `json:"function"`
	Runtime    string    `json:"runtime"`
	Status     int       `json:"status"` // HTTP status of the call
	Start      string    `json:"start,omitempty"`
	Snapshot   string    `json:"snapshot,omitempty"`
	VMID       string    `json:"vm,omitempty"`
	BootMS     int64     `json:"boot_ms"`
	UploadMS   int64     `json:"upload_ms"`
	LoadMS     int64     `json:"load_ms"`
	FunctionMS int64     `json:"function_ms"`
	DurationMS float64   `json:"duration_ms"` // whole request inside the server, µs precision
	At         time.Time `json:"at"`          // when it finished
}

// Node is the answer to GET /api/v1/node: how full this node is. It counts every tenant's VMs
// (numbers only, no names), because density is a property of the node.
type Node struct {
	MaxVMs            int            `json:"max_vms"`
	VMs               int            `json:"vms"`
	TenantVMs         int            `json:"tenant_vms"`   // of VMs, the caller's own (counted at the same moment)
	States            map[string]int `json:"states"`       // restoring, idle, busy, recycling
	MeasuredVMs       int            `json:"measured_vms"` // VMs whose memory could be read
	PSSBytes          uint64         `json:"pss_bytes"`    // summed over the measured VMs
	RSSBytes          uint64         `json:"rss_bytes"`    // summed; shared pages counted in every VM
	MemTotalBytes     uint64         `json:"mem_total_bytes"`
	MemAvailableBytes uint64         `json:"mem_available_bytes"`
}

// Error is the body of every error answer. For a failed function (HTTP 502) ExitCode and
// Stderr say what happened inside the VM.
type Error struct {
	Error    string `json:"error"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}
