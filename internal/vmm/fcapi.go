package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// fcAPI is a minimal client for Firecracker's REST API, which it serves on a Unix socket.
// The SDK does the same for booting; for restoring we use this small client because every
// millisecond counts there (the SDK checks for the socket only every 10 ms).
type fcAPI struct {
	http *http.Client
	sock string
}

func newFCAPI(sock string) *fcAPI {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableCompression: true,
	}
	return &fcAPI{http: &http.Client{Transport: tr}, sock: sock}
}

// waitReady polls until Firecracker accepts connections on its API socket.
func (a *fcAPI) waitReady(ctx context.Context, every time.Duration, exited <-chan struct{}) error {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		var d net.Dialer
		if c, err := d.DialContext(ctx, "unix", a.sock); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("firecracker exited before its API socket was ready")
		case <-ctx.Done():
			return fmt.Errorf("waiting for firecracker API socket: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// put sends a PUT request with a JSON body. Firecracker answers 204 on success, or an error
// status with {"fault_message": "..."}.
func (a *fcAPI) put(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var fault struct {
		Message string `json:"fault_message"`
	}
	if json.Unmarshal(reply, &fault) == nil && fault.Message != "" {
		return fmt.Errorf("firecracker %s: %d: %s", path, resp.StatusCode, fault.Message)
	}
	return fmt.Errorf("firecracker %s: %d: %s", path, resp.StatusCode, bytes.TrimSpace(reply))
}

// snapshotLoad is the body of PUT /snapshot/load.
type snapshotLoad struct {
	SnapshotPath string     `json:"snapshot_path"`
	MemBackend   memBackend `json:"mem_backend"`
	ResumeVM     bool       `json:"resume_vm"`
}

type memBackend struct {
	// "File": Firecracker maps the memory file into the VM (private, copy-on-write), so pages
	// are read only when the guest touches them and many VMs can share one file.
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

func (a *fcAPI) loadSnapshot(ctx context.Context, statePath, memPath string) error {
	return a.put(ctx, "/snapshot/load", snapshotLoad{
		SnapshotPath: statePath,
		MemBackend:   memBackend{BackendType: "File", BackendPath: memPath},
		ResumeVM:     true,
	})
}

// close drops idle connections.
func (a *fcAPI) close() {
	if tr, ok := a.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}
