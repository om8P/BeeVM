package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/OmMhatre-99/BeeVM/internal/agentproto"
)

// dialVsock opens a connection to a port inside the guest.
//
// Firecracker exposes the guest's vsock as a Unix socket on the host (udsPath). To reach a
// guest port, connect to that socket and send "CONNECT <port>\n"; Firecracker answers
// "OK <hostport>\n" and from then on the connection is a plain byte stream to the guest.
// If nothing is listening in the guest yet, Firecracker just closes the connection.
func dialVsock(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("connect to vsock socket: %w", err)
	}
	// The handshake must not hang forever; use the caller's deadline (or 5 s).
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = conn.SetDeadline(deadline)

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	// Read the reply one byte at a time so no byte of the stream that follows is consumed.
	var line []byte
	one := make([]byte, 1)
	for len(line) < 64 {
		if _, err := conn.Read(one); err != nil {
			conn.Close()
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("guest agent not listening on vsock port %d (connection closed by firecracker)", port)
			}
			return nil, fmt.Errorf("vsock handshake: %w", err)
		}
		if one[0] == '\n' {
			break
		}
		line = append(line, one[0])
	}
	if !strings.HasPrefix(string(line), "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: unexpected reply %q", line)
	}
	_ = conn.SetDeadline(time.Time{}) // clear: later requests use their own context
	return conn, nil
}

// AgentClient talks to the guest agent of one VM over vsock, using plain HTTP.
// It keeps one connection open between calls.
type AgentClient struct {
	http      *http.Client
	transport *http.Transport
}

func newAgentClient(udsPath string) *AgentClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialVsock(ctx, udsPath, agentproto.Port)
		},
		MaxIdleConns:       1,
		DisableCompression: true,
	}
	return &AgentClient{http: &http.Client{Transport: tr}, transport: tr}
}

// Close drops the idle connection.
func (c *AgentClient) Close() { c.transport.CloseIdleConnections() }

// do sends a request and decodes a JSON reply into out. Non-2xx replies become errors.
func (c *AgentClient) do(ctx context.Context, method, path string, body io.Reader, size int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://beevm-agent"+path, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e agentproto.ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("agent returned %d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode reply: %w", err)
	}
	return nil
}

// Health asks the agent whether it is up.
func (c *AgentClient) Health(ctx context.Context) (agentproto.HealthResponse, error) {
	var h agentproto.HealthResponse
	err := c.do(ctx, http.MethodGet, agentproto.PathHealth, nil, -1, &h)
	return h, err
}

// UploadFunction sends the function binary read from r (size bytes) to the agent.
func (c *AgentClient) UploadFunction(ctx context.Context, r io.Reader, size int64) (agentproto.UploadResponse, error) {
	var u agentproto.UploadResponse
	err := c.do(ctx, http.MethodPut, agentproto.PathFunction, r, size, &u)
	return u, err
}

// UploadFunctionFile uploads the binary at path.
func (c *AgentClient) UploadFunctionFile(ctx context.Context, path string) (agentproto.UploadResponse, error) {
	f, err := os.Open(path)
	if err != nil {
		return agentproto.UploadResponse{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return agentproto.UploadResponse{}, err
	}
	return c.UploadFunction(ctx, f, st.Size())
}

// Invoke runs the uploaded function once. timeout is the function's own limit inside the
// guest (0 = the agent's default). ctx should allow a little longer than timeout, so the
// agent's "timeout" answer arrives before the host gives up.
//
// A function that fails (non-zero exit, timeout, bad output) is NOT a Go error: look at
// the returned Error field. A Go error means the call itself failed.
func (c *AgentClient) Invoke(ctx context.Context, payload json.RawMessage, timeout time.Duration) (agentproto.InvokeResponse, error) {
	body, err := json.Marshal(agentproto.InvokeRequest{Payload: payload, TimeoutMS: timeout.Milliseconds()})
	if err != nil {
		return agentproto.InvokeResponse{}, fmt.Errorf("encode request: %w", err)
	}
	var out agentproto.InvokeResponse
	err = c.do(ctx, http.MethodPost, agentproto.PathInvoke, bytes.NewReader(body), int64(len(body)), &out)
	return out, err
}
