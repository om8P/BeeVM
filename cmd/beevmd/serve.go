package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/om8P/BeeVM/internal/api"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// openRegistry opens the database in BEEVM_HOME, creating the folder if needed.
func openRegistry(home string) (*registry.Registry, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	return registry.Open(filepath.Join(home, "beevm.db"))
}

// runServe starts the HTTP API.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on (localhost only by default)")
	maxVMs := fs.Int("max-vms", 8, "most VMs running at the same time")
	queueWait := fs.Duration("queue-wait", 5*time.Second, "how long a call waits for a free VM slot before getting 503")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	home := beevmHome()

	reg, err := openRegistry(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer reg.Close()
	code, err := codestore.New(filepath.Join(home, "functions"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	// The server has its own VM folder. Anything in it is left over from a crashed run (this is
	// the only server using it), so clear it. beevmd boot and invoke use a different folder.
	base := vmConfig(home, "", 1, 128)
	base.BaseDir = filepath.Join(home, "run", "serve")
	if stale, _ := os.ReadDir(base.BaseDir); len(stale) > 0 {
		logger.Warn("removing VM folders left by a previous run", "count", len(stale))
		_ = os.RemoveAll(base.BaseDir)
	}
	if err := os.MkdirAll(base.BaseDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	handler := api.New(api.Deps{
		Registry: reg, Code: code, Logger: logger,
		Runner: runner.NewColdRunner(base, *maxVMs, *queueWait),
	}).Handler()
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", *addr, "max_vms", *maxVMs, "home", home)

	select {
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	case <-sigCtx.Done():
	}

	// Graceful shutdown: stop taking requests, let running calls finish (each one cleans up its
	// own VM). A second Ctrl-C ends the wait early.
	stop()
	logger.Info("shutting down, waiting for running calls to finish (Ctrl-C again to stop waiting)")
	force, forceStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer forceStop()
	ctx, cancel := context.WithTimeout(force, 90*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("shutdown did not finish cleanly", "error", err)
		_ = srv.Close() // cancels the remaining requests; their VMs are killed and cleaned up
		time.Sleep(500 * time.Millisecond)
		return 1
	}
	logger.Info("stopped")
	return 0
}
