# BeeVM milestones: one session each

This refines the 9 milestones in `docs/architecture.md` into 13 session-sized steps. Each step:
- builds on the previous one and leaves a **working, demoable** result,
- can be **refined later** in its own short session without breaking later steps,
- ends with: tests pass, `docs/progress.md` updated, git commit.

## How to run a session

**Start (copy, replace N):**
> Read CLAUDE.md, docs/progress.md and docs/milestones.md, then plan milestone N. Explain new concepts briefly.

**Finish:**
> Run the tests, update docs/progress.md, and commit.

**Refine (a short follow-up session on a finished milestone):**
> Read docs/progress.md and docs/milestones.md. Refine milestone N: [what to improve]. Don't change its public interfaces.

Good reasons to refine: a test is flaky, code is hard to read, you don't understand a part and want it simplified or explained, a benchmark number is poor.

---

## M1: Environment and a hand-booted MicroVM
- **Goal:** prove KVM and Firecracker work on this laptop.
- **Do:** setup script `scripts/setup-wsl.sh` (add user to `kvm` group, install Go 1.23+, download Firecracker and a `vmlinux` 6.1 kernel), build a tiny Alpine rootfs, boot one VM with the `firecracker` command line and a JSON config.
- **Done when:** you see Linux boot messages from inside a MicroVM, and `make setup` repeats it from scratch.
- **You learn:** what KVM, a kernel image and a root filesystem are.
- **Refine later:** make the setup script safe to re-run; document each step.

## M2: Boot and stop a VM from Go
- **Goal:** replace the command line with Go code.
- **Do:** `internal/vmm` using `firecracker-go-sdk` (create, start, stop, kill), `cmd/beevmd` skeleton with a `boot` test command, console output captured to a log file, a per-VM working directory.
- **Done when:** `go run ./cmd/beevmd boot` starts a VM, prints its console, and cleans up after itself. A `//go:build kvm` test does the same.
- **You learn:** Firecracker's REST-over-socket API and the VM lifecycle.
- **Refine later:** error handling, timeouts, no leftover processes or files after a crash.

## M3: Guest agent over vsock, first function runs
- **Goal:** talk to code inside the VM.
- **Do:** `cmd/beevm-agent` (static Go binary baked into the rootfs, starts at boot, listens on vsock), `internal/agentproto` request/response types, host-side client in `internal/vmm`, support for the **static binary** runtime (receive code, run it, return JSON).
- **Done when:** the host boots a VM, sends `{"payload": ...}`, and gets the function's output back, repeatably.
- **You learn:** vsock, init in a minimal Linux, why the agent is PID-1-adjacent.
- **Refine later:** timeouts, big payloads, agent logging, graceful shutdown.

## M4: HTTP API, registry and CLI
- **Goal:** invoke functions over HTTP, not by hand.
- **Do:** `internal/api` (chi router, `POST /invoke/{fn}`, admin REST), `internal/registry` (SQLite: functions, tenants, hashed API keys), `cmd/beevm` CLI (`deploy`, `invoke`, `ls`, `rm`), function code stored on local disk for now. One VM booted per request (slow but correct).
- **Done when:** `beevm deploy hello ./hello-bin` then `curl .../invoke/hello` returns the right result with a valid API key, and 401 without one.
- **You learn:** how a FaaS request flows end to end.
- **Refine later:** API shapes, input validation, nicer CLI output, unit tests for auth.

## M5: Artifact store with MinIO/S3
- **Goal:** function code lives in cloud-style storage, as the case study requires.
- **Do:** `deploy/docker-compose.yml` with MinIO, `internal/store` (aws-sdk-go-v2 S3 client), `deploy` uploads, the node fetches and caches by code hash.
- **Done when:** deploy uploads to MinIO, invoke downloads on first use and reuses the cache after. Switching to real S3 is a config change only.
- **You learn:** the S3 API and content-addressed caching.
- **Refine later:** cache eviction, retry on failed downloads.

## M6: Snapshots and fast restore (the core)
- **Goal:** the **< 50 ms** cold start.
- **Do:** `internal/snapshot`: boot a VM, wait until the agent is ready, pause, write the snapshot (memory + state) to local NVMe; restore it and resume; handle the vsock reconnect after restore. Add a first benchmark: boot vs restore time.
- **Done when:** restore-and-first-response is measured and reported with p50/p99 over 100 runs. Target restore under 50 ms (report honestly if WSL2 is slower).
- **You learn:** how snapshots and lazy memory loading make restores fast.
- **Refine later:** the hardest tuning session: measure, then tune kernel boot args, memory size, file placement.

## M7: Warm pool
- **Goal:** the ~1 ms warm path.
- **Do:** `internal/pool`: per-function idle pools, state machine (`Restoring → Idle → Busy → Recycling → Destroyed`), keep-alive TTL, prewarm count, max VMs per node, LRU eviction. `invoke` uses `Acquire`/`Release`.
- **Done when:** repeat calls hit warm VMs, idle ones are reaped after the TTL, and a unit test covers every state transition.
- **You learn:** concurrency in Go (goroutines, channels, locks) on a real problem.
- **Refine later:** race conditions (run `go test -race`), eviction policy.

## M8: Python and Node.js runtimes, function snapshots
- **Goal:** the languages from the plan, plus per-function snapshots.
- **Do:** `guest/rootfs` images for Python 3.12 and Node.js 20 with shims, **runtime snapshot** (interpreter loaded) and **function snapshot** (user code imported), deploy of zip packages.
- **Done when:** `hello-python` and `hello-node` run correctly and their restore times are measured and compared with the static binary.
- **You learn:** why snapshotting after loading code helps.
- **Refine later:** package size limits, more example functions.

## M9: Isolation and networking
- **Goal:** the "tenant isolation security" grading point.
- **Do:** Firecracker jailer (chroot plus unique UID), cgroups v2 limits (`cpu.max`, `memory.max`, `pids.max`), seccomp, **one network namespace per VM** with a TAP device and nftables rules between tenants. Attack functions in `examples/functions/`: fork bomb, memory hog, CPU hog, cross-tenant network probe, host file read.
- **Done when:** every attack is stopped or contained, with a test per attack, and a results table for the report.
- **You learn:** Linux namespaces, cgroups and why they matter for multi-tenant clouds.
- **Refine later:** add more attack cases, tighten rules.

## M10: Observability
- **Goal:** metrics you can graph.
- **Do:** `internal/metrics` with Prometheus histograms for cold, restore and warm latency, pool size, VMs per node and memory per VM, `/metrics` endpoint, JSON logs, optional Prometheus and Grafana in a Compose profile with a ready-made dashboard.
- **Done when:** Grafana shows live latency and density during a load run.
- **Optional module:** can be deleted without touching core code.

## M11: Web dashboard
- **Goal:** a live demo screen.
- **Do:** `web/` (React, Vite, TypeScript, Recharts): VM grid with state colours, pool sizes, an invoke button with timing, cold vs warm chart, density counter. Served from `beevmd` at `/ui` using the public REST/SSE API only.
- **Done when:** the dashboard updates live while you run a k6 load test.
- **Optional module:** can be deleted without touching core code. Skip if time is short.

## M12: Benchmarks and density
- **Goal:** the numbers for all three grading points.
- **Do:** `cmd/beevm-bench`, k6 scripts, `bench/plot_results.py`: cold start p50/p99 (boot vs restore vs warm), throughput, **max idle VMs per node** and memory per VM, isolation results table.
- **Done when:** `make bench` produces charts and a results file that goes straight into the report.
- **Refine later:** reruns on a different machine, clearer charts.

## M13: Cloud run, report and demo
- **Goal:** finish and present.
- **Do (optional cloud part):** Terraform for one nested-virtualization VM, rerun M12 there, compare with WSL2. **Always:** `docs/demo-script.md`, final architecture figures, README, report notes.
- **Done when:** a fresh clone plus the README reproduces the demo, and the demo script runs in under 10 minutes.

---

## Order and shortcuts

- **Must do, in order:** M1 → M2 → M3 → M4 → M6 → M7 → M9 → M12 (this covers all grading points).
- **Can swap or delay:** M5 (MinIO) any time after M4; M8 (Python/Node) after M6; M10 and M11 anytime after M7.
- **If time runs out:** drop M11 first, then M10, then the cloud part of M13.
