//go:build linux

// Command beevm-agent runs inside every BeeVM MicroVM. It listens on vsock port
// agentproto.Port and serves the agent protocol (see internal/agentproto).
//
// It is built as a static binary (CGO_ENABLED=0) and baked into the rootfs as
// /sbin/beevm-agent. The init script starts it; if it exits, init reboots the VM.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/om8P/BeeVM/internal/agent"
	"github.com/om8P/BeeVM/internal/agentproto"
)

const version = "0.1.0"

func main() {
	// stderr is the serial console, so the host can see these lines in console.log.
	log.SetFlags(0)
	log.SetPrefix("beevm-agent: ")

	ln, err := vsock.Listen(agentproto.Port, nil)
	if err != nil {
		log.Fatalf("listen on vsock port %d: %v", agentproto.Port, err)
	}
	srv := &http.Server{
		Handler:           agent.New(agentproto.FunctionPath, version).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("v%s listening on vsock port %d", version, agentproto.Port)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	log.Print("stopped")
}
