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
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/api"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/metrics"
	"github.com/om8P/BeeVM/internal/pool"
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

// newLogger makes the server's logger: JSON lines (for log tools) or text (for reading).
func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level must be debug, info, warn or error, not %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	}
	return nil, fmt.Errorf("--log-format must be json or text, not %q", format)
}

// runServe starts the HTTP API.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on (localhost only by default)")
	maxVMs := fs.Int("max-vms", 8, "most VMs running at the same time")
	queueWait := fs.Duration("queue-wait", 5*time.Second, "how long a call waits for a free VM slot before getting 503")
	runnerKind := fs.String("runner", "warm", "how calls get a VM: warm (keep VMs between calls, new ones restored from a snapshot), snapshot (restore for every call) or cold (full boot every call)")
	keepAlive := fs.Duration("keep-alive", 60*time.Second, "warm runner: how long an idle VM is kept for its function's next call")
	// Artifact store (S3 API). Without a bucket, code stays on this node's disk only.
	s3Bucket := fs.String("s3-bucket", os.Getenv("BEEVM_S3_BUCKET"), "S3 bucket for function code (env BEEVM_S3_BUCKET); empty = local disk only")
	s3Endpoint := fs.String("s3-endpoint", os.Getenv("BEEVM_S3_ENDPOINT"), "S3 endpoint URL for MinIO etc. (env BEEVM_S3_ENDPOINT); empty = AWS S3")
	s3Region := fs.String("s3-region", envOr("BEEVM_S3_REGION", "us-east-1"), "S3 region (env BEEVM_S3_REGION)")
	s3PathStyle := fs.Bool("s3-path-style", os.Getenv("BEEVM_S3_ENDPOINT") != "", "use path-style S3 URLs (needed for MinIO; default on when an endpoint is set)")
	isoFlags := addIsolationFlags(fs)
	withMetrics := fs.Bool("metrics", true, "serve Prometheus metrics at GET /metrics on --addr (no API key: keep --addr on localhost or firewalled)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "json", "json (one JSON object per line) or text")
	_ = fs.Parse(args)

	logger, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	var mets *metrics.Metrics
	if *withMetrics {
		mets = metrics.New()
	}
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

	// Isolation (--isolation jailer, needs sudo): every VM gets its own jail, UID and cgroup.
	// Files root creates in BEEVM_HOME are given back to their owner, now (the database's
	// journal files, so `beevmd tenant add` still works without sudo) and on exit.
	iso, err := isoFlags.open(home, &base, logger, *maxVMs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer closeIsolation(iso)
	if iso != nil {
		if _, err := iso.ChownBack(); err != nil {
			logger.Warn("could not give files back to their owner", "error", err)
		}
		logger.Info("isolation on", "mode", iso.Name(), "cpu_limit_percent", *isoFlags.cpuPercent, "uid_base", *isoFlags.uidBase,
			"egress", *isoFlags.egress)
	}

	// Runtimes: binaries always run; python and node need their image (make images).
	images := runtimeImages(home)
	var available []string
	for rt, path := range images {
		if _, err := os.Stat(path); err == nil {
			available = append(available, rt)
		} else {
			logger.Warn("runtime not available: its image is missing (run: make images)", "runtime", rt, "image", path)
		}
	}
	sort.Strings(available)

	// openSnapshots sets up the Snapshot Manager, removes snapshots that no longer fit (other
	// images, or functions that were deleted or changed), and builds the default-size runtime
	// snapshots in the background, so the first call does not wait for them.
	var startup sync.WaitGroup
	openSnapshots := func() (*snapshot.Manager, error) {
		snaps, err := snapshot.New(filepath.Join(home, "snapshots"), base, func(f string, a ...any) {
			logger.Info(fmt.Sprintf(f, a...))
		})
		if err != nil {
			return nil, err
		}
		for rt, path := range images {
			snaps.SetRootfs(rt, path)
		}
		if mets != nil {
			snaps.SetObserver(mets.ObserveSnapshotBuild)
		}
		if n, err := snaps.RemoveStale(); err == nil && n > 0 {
			logger.Info("removed snapshots that no longer match the images", "count", n)
		}
		if codes, err := reg.FunctionCodes(); err == nil {
			n, _ := snaps.RemoveOrphans(func(id int64, sha string) bool { return codes[id] == sha })
			if n > 0 {
				logger.Info("removed snapshots of deleted or changed functions", "count", n)
			}
		}
		startup.Add(1)
		go func() {
			defer startup.Done()
			for _, rt := range append([]string{agentproto.RuntimeBinary}, available...) {
				if _, err := snaps.EnsureRuntime(context.Background(), rt, registry.DefaultMemoryMiB, registry.DefaultVCPUs); err != nil {
					logger.Error("could not build a runtime snapshot; its calls will boot instead", "runtime", rt, "error", err)
				}
			}
		}()
		return snaps, nil
	}

	var run runner.Runner
	switch *runnerKind {
	case "cold":
		c := runner.NewColdRunner(base, *maxVMs, *queueWait)
		for rt, path := range images {
			c.SetRootfs(rt, path)
		}
		run = c
	case "snapshot", "warm":
		snaps, err := openSnapshots()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if *runnerKind == "snapshot" {
			s := runner.NewSnapshotRunner(base, snaps, *maxVMs, *queueWait, logger)
			for rt, path := range images {
				s.SetRootfs(rt, path)
			}
			run = s
		} else {
			pcfg := pool.Config{MaxVMs: *maxVMs, KeepAlive: *keepAlive, QueueWait: *queueWait, Logger: logger}
			if mets != nil {
				pcfg.OnRecycle = mets.ObserveRecycle
			}
			w := runner.NewWarmRunner(base, snaps, pcfg, logger)
			for rt, path := range images {
				w.SetRootfs(rt, path)
			}
			// VMs per state and memory per VM, read whenever Prometheus scrapes.
			mets.Register(metrics.NewPoolCollector(func() []pool.VMInfo { return w.VMs(0) }, w.MaxVMs()))
			run = w
		}
	default:
		fmt.Fprintf(os.Stderr, "error: --runner must be warm, snapshot or cold, not %q\n", *runnerKind)
		return 2
	}
	// The warm pool keeps VMs between calls, and snapshot builds run VMs too: wait for the
	// start-up builds and destroy the pool's VMs on the way out.
	closeRunner := func() {
		startup.Wait()
		if c, ok := run.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				logger.Warn("warm pool did not close cleanly", "error", err)
			}
		}
	}

	handler := api.New(api.Deps{
		Registry: reg, Code: code, Logger: logger, Runner: run, Metrics: mets,
	}).Handler()
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", *addr, "metrics", *withMetrics, "max_vms", *maxVMs, "runner", *runnerKind, "keep_alive", keepAlive.String(), "runtimes", append([]string{agentproto.RuntimeBinary}, available...), "home", home, "isolation", *isoFlags.mode)

	select {
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, "error:", err)
		closeRunner()
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
		closeRunner()
		return 1
	}
	closeRunner()
	logger.Info("stopped")
	return 0
}
