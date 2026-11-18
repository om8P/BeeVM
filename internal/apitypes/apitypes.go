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

// Function logs on a successful /invoke (a failed call has them in the 502 body instead). The
// caller asks for them with the request header HeaderLogType: "tail"; the answer then has
// HeaderLogBytes (size of the whole log the server got, "0" when the function printed nothing)
// and, if the log is not empty, HeaderLogResult: the last MaxLogTailBytes of it, base64-encoded
// (logs may hold any bytes, and headers may not). Same idea as AWS Lambda's X-Amz-Log-Type.
const (
	HeaderLogType   = "X-BeeVM-Log-Type"   // request: "tail" = send the end of the function's log
	HeaderLogResult = "X-BeeVM-Log-Result" // response: base64 of the log's last MaxLogTailBytes
	HeaderLogBytes  = "X-BeeVM-Log-Bytes"  // response: size of the whole log in bytes

	LogTypeTail = "tail"
	// MaxLogTailBytes keeps the header small enough for proxies (often limited to 8 KB).
	MaxLogTailBytes = 4096
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

// CoolResult is the answer to POST /api/v1/functions/{name}/cool and POST /api/v1/vms/cool:
// how many warm VMs were thrown away (busy ones count too: they go once their call ends).
type CoolResult struct {
	Cooled int `json:"cooled"`
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
	// EventFunction carries a FunctionEvent: a function was deployed (created or replaced) or
	// deleted. Added in the M11 refine, so clients need not poll the function list.
	EventFunction = "function"
)

// FunctionEvent is the "function" event.
type FunctionEvent struct {
	Action   string   `json:"action"`   // FunctionDeployed or FunctionDeleted
	Function Function `json:"function"` // for "deleted" only Name is set
}

// FunctionEvent actions.
const (
	FunctionDeployed = "deployed"
	FunctionDeleted  = "deleted"
)

// Snapshot is one snapshot on the node, as GET /api/v1/snapshots lists it.
type Snapshot struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"` // "runtime" or "function"
	Runtime   string    `json:"runtime"`
	Function  string    `json:"function,omitempty"` // function snapshots: the caller's function ("" = removed since)
	MemoryMiB int64     `json:"memory_mib"`
	VCPUs     int64     `json:"vcpus"`
	Isolation string    `json:"isolation,omitempty"` // "" = no isolation
	SizeBytes int64     `json:"size_bytes"`          // the memory file (most of it)
	BuildMS   int64     `json:"build_ms"`
	CreatedAt time.Time `json:"created_at"`
	Current   bool      `json:"current"` // false = stale: built from older images or settings, no longer used
}

// SnapshotList is the answer to GET /api/v1/snapshots: the node's runtime snapshots (shared
// by every tenant) and the caller's own function snapshots.
type SnapshotList struct {
	Runtime  []Snapshot `json:"runtime"`
	Function []Snapshot `json:"function"`
	// RuntimeDeleteAllowed says whether this server lets tenants delete runtime snapshots
	// (beevmd serve --allow-runtime-snapshot-delete). Function snapshots: always, own only.
	RuntimeDeleteAllowed bool `json:"runtime_delete_allowed"`
}

// Me is the answer to GET /api/v1/me: who the API key belongs to.
type Me struct {
	Tenant string `json:"tenant"`
}

// Hello is the "hello" event: the caller's VMs right now.
type Hello struct {
	VMs    []VM `json:"vms"`
	MaxVMs int  `json:"max_vms"` // VMs on the whole node at most (0 = no warm pool)
}

// Async invoke (like Lambda's "Event" invocation type): POST /invoke/{name}?async=1, or the
// request header HeaderInvocationType: "Event". The server puts the call in a queue and answers
// 202 InvokeAccepted at once; the result arrives later as an "invoke" event with the same
// RequestID (the function's output is not kept).
const (
	HeaderInvocationType = "X-BeeVM-Invocation-Type"
	InvocationEvent      = "Event"
)

// InvokeAccepted is the 202 answer to an async invoke.
type InvokeAccepted struct {
	RequestID string `json:"request_id"`
	Queued    int    `json:"queued"` // calls of this tenant waiting in the queue, this one included
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
	// Async calls only: the id the 202 answer gave, the time spent waiting in the queue
	// (included in DurationMS), and why it failed without a function result (expired, busy).
	RequestID string  `json:"request_id,omitempty"`
	Async     bool    `json:"async,omitempty"`
	QueuedMS  float64 `json:"queued_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
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
