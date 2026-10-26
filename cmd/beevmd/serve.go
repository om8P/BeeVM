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
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/store"
)

// envOr returns the environment variable's value, or def if it is empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

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
	runnerKind := fs.String("runner", "snapshot", "how VMs start: snapshot (restore from a base snapshot) or cold (full boot every call)")
	// Artifact store (S3 API). Without a bucket, code stays on this node's disk only.
	s3Bucket := fs.String("s3-bucket", os.Getenv("BEEVM_S3_BUCKET"), "S3 bucket for function code (env BEEVM_S3_BUCKET); empty = local disk only")
	s3Endpoint := fs.String("s3-endpoint", os.Getenv("BEEVM_S3_ENDPOINT"), "S3 endpoint URL for MinIO etc. (env BEEVM_S3_ENDPOINT); empty = AWS S3")
	s3Region := fs.String("s3-region", envOr("BEEVM_S3_REGION", "us-east-1"), "S3 region (env BEEVM_S3_REGION)")
	s3PathStyle := fs.Bool("s3-path-style", os.Getenv("BEEVM_S3_ENDPOINT") != "", "use path-style S3 URLs (needed for MinIO; default on when an endpoint is set)")
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

	if *s3Bucket == "" {
		logger.Warn("no S3 bucket configured: function code is stored on this node only (set BEEVM_S3_BUCKET to use an artifact store)")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		blobs, err := store.NewS3(ctx, store.S3Config{
			Bucket: *s3Bucket, Region: *s3Region, Endpoint: *s3Endpoint, PathStyle: *s3PathStyle,
		})
		if err == nil {
			err = blobs.EnsureBucket(ctx)
		}
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: artifact store:", err)
			fmt.Fprintln(os.Stderr, "(is MinIO running, and are AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY set? see deploy/minio.env)")
			return 1
		}
		code.WithRemote(blobs)
		logger.Info("artifact store ready", "bucket", *s3Bucket, "endpoint", *s3Endpoint, "region", *s3Region)
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

	var run runner.Runner
	switch *runnerKind {
	case "cold":
		run = runner.NewColdRunner(base, *maxVMs, *queueWait)
	case "snapshot":
		snaps, err := snapshot.New(filepath.Join(home, "snapshots"), base, func(f string, a ...any) {
			logger.Info(fmt.Sprintf(f, a...))
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if n, err := snaps.RemoveStale(); err == nil && n > 0 {
			logger.Info("removed snapshots that no longer match the images", "count", n)
		}
		run = runner.NewSnapshotRunner(base, snaps, *maxVMs, *queueWait, logger)
		// Build the default-size snapshot now, so the first call does not wait for it.
		go func() {
			if _, err := snaps.Ensure(context.Background(), registry.DefaultMemoryMiB, registry.DefaultVCPUs); err != nil {
				logger.Error("could not build the default snapshot; calls will boot instead", "error", err)
			}
		}()
	default:
		fmt.Fprintf(os.Stderr, "error: --runner must be snapshot or cold, not %q\n", *runnerKind)
		return 2
	}

	handler := api.New(api.Deps{
		Registry: reg, Code: code, Logger: logger, Runner: run,
	}).Handler()
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", *addr, "max_vms", *maxVMs, "runner", *runnerKind, "home", home)

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
