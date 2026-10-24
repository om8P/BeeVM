package vmm

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// fakeFirecracker listens on a Unix socket like Firecracker's vsock bridge: it expects
// "CONNECT <port>\n", answers with reply, and then serves HTTP on the connection.
type fakeFirecracker struct {
	path       string
	handshakes atomic.Int32
	gotPort    atomic.Int32
}

// startFake starts the fake. reply is the handshake answer ("" means close without answering).
func startFake(t *testing.T, reply string, handler http.Handler) *fakeFirecracker {
	t.Helper()
	// Unix socket paths are limited to ~108 bytes, so keep the folder name short.
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeFirecracker{path: filepath.Join(dir, "v.sock")}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go http.Serve(&handshakeListener{Listener: ln, f: f, reply: reply}, handler) //nolint:errcheck
	return f
}

// handshakeListener does the CONNECT/OK exchange for every accepted connection.
type handshakeListener struct {
	net.Listener
	f     *fakeFirecracker
	reply string
}

func (l *handshakeListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			conn.Close()
			continue
		}
		l.f.handshakes.Add(1)
		if port, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT "))); err == nil {
			l.f.gotPort.Store(int32(port))
		}
		if l.reply == "" {
			conn.Close()
			continue
		}
		_, _ = io.WriteString(conn, l.reply)
		return conn, nil
	}
}

func agentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agentproto.HealthResponse{Status: "ok", AgentVersion: "fake"})
	})
	mux.HandleFunc("PUT /function", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(agentproto.UploadResponse{SHA256: "abc", Size: int64(len(b))})
	})
	mux.HandleFunc("POST /invoke", func(w http.ResponseWriter, r *http.Request) {
		var req agentproto.InvokeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		out, _ := json.Marshal(map[string]json.RawMessage{"echo": req.Payload})
		_ = json.NewEncoder(w).Encode(agentproto.InvokeResponse{Output: out})
	})
	mux.HandleFunc("POST /nofunction", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(agentproto.ErrorResponse{Error: "no function uploaded yet"})
	})
	return mux
}

func TestAgentClientHealthInvokeUpload(t *testing.T) {
	f := startFake(t, "OK 1073741824\n", agentHandler())
	c := newAgentClient(f.path)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := c.Health(ctx)
	if err != nil || h.Status != "ok" || h.AgentVersion != "fake" {
		t.Fatalf("Health: %+v, %v", h, err)
	}
	if got := f.gotPort.Load(); got != agentproto.Port {
		t.Errorf("handshake asked for port %d, want %d", got, agentproto.Port)
	}

	up, err := c.UploadFunction(ctx, strings.NewReader("hello-binary"), int64(len("hello-binary")))
	if err != nil || up.Size != 12 {
		t.Fatalf("Upload: %+v, %v", up, err)
	}

	out, err := c.Invoke(ctx, json.RawMessage(`{"n":1}`), time.Second)
	if err != nil || out.Error != "" || string(out.Output) != `{"echo":{"n":1}}` {
		t.Fatalf("Invoke: %+v, %v", out, err)
	}

	// All three calls must have shared one connection (keep-alive), so one handshake.
	if n := f.handshakes.Load(); n != 1 {
		t.Errorf("want 1 handshake (connection reuse), got %d", n)
	}
}

func TestAgentClientHTTPErrorShowsAgentMessage(t *testing.T) {
	f := startFake(t, "OK 1\n", agentHandler())
	c := newAgentClient(f.path)
	defer c.Close()
	var out agentproto.InvokeResponse
	err := c.do(context.Background(), http.MethodPost, "/nofunction", nil, -1, &out)
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "no function uploaded") {
		t.Errorf("got %v", err)
	}
}

func TestDialRejectedHandshake(t *testing.T) {
	f := startFake(t, "NOPE\n", agentHandler())
	_, err := dialVsock(context.Background(), f.path, agentproto.Port)
	if err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Errorf("got %v", err)
	}
}

func TestDialGuestNotListening(t *testing.T) {
	// Firecracker closes the connection when nothing listens in the guest.
	f := startFake(t, "", agentHandler())
	_, err := dialVsock(context.Background(), f.path, agentproto.Port)
	if err == nil || !strings.Contains(err.Error(), "not listening") {
		t.Errorf("got %v", err)
	}
}

func TestDialNoSocket(t *testing.T) {
	_, err := dialVsock(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), agentproto.Port)
	if err == nil {
		t.Error("expected an error")
	}
}

func TestDialHandshakeRespectsDeadline(t *testing.T) {
	// A bridge that accepts but never answers must not hang the caller.
	dir, _ := os.MkdirTemp("", "fc")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, say nothing
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := dialVsock(ctx, path, agentproto.Port); err == nil {
		t.Error("expected a timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("dial took %v, deadline was ignored", time.Since(start))
	}
}
