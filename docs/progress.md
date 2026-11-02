# BeeVM progress log

Update this at the end of every working session: what got done, what's broken, what's next.
Details of each milestone: `docs/milestones.md`.

## Milestones

| # | Milestone | Status |
|---|-----------|--------|
| 1 | Environment and a hand-booted MicroVM | Done, needs refinement (boot ~1.8 s under WSL2; no `kvm` test yet) |
| 2 | Boot and stop a VM from Go | Done |
| 3 | Guest agent over vsock, first function runs | Done |
| 4 | HTTP API, registry and CLI | Done |
| 5 | Artifact store with MinIO/S3 | Done |
| 6 | Snapshots and fast restore (the core) | Done, refined once (restore p50 45 ms; p99 60-67 ms on WSL2, set by KVM load noise and page faults under nested virtualization; page prefetch needs root on WSL, deferred to the cloud node) |
| 7 | Warm pool | Done (warm call p50 1.6 ms, p99 5.0 ms in the server; restore + first call 88 ms) |
| 8 | Python and Node.js runtimes, function snapshots | Not started |
| 9 | Isolation and networking | Not started |
| 10 | Observability (optional module) | Not started |
| 11 | Web dashboard (optional module) | Not started |
| 12 | Benchmarks and density | Not started |
| 13 | Cloud run, report and demo | Not started |

Status values: Not started · In progress · Done · Done, needs refinement (say what).

## Session log

### 2026-10-22: Planning
- Chose case study 06; found the doc's group 06 details are mismatched (ignored).
- Wrote the architecture plan: `docs/architecture.md`.
- Made the architecture diagrams (box version and beehive version) as a Claude design canvas: https://claude.ai/artifact/QEcUUmM4Tc2DiPHHEhFqYo
- Checked the laptop: WSL2 Ubuntu 24.04 with `/dev/kvm` available.
- Set up CLAUDE.md, git, and `docs/milestones.md` (13 session-sized milestones).
- **Next:** milestone 1.

### 2026-10-22: Milestone 1
- Added `make setup / images / boot / boot-test / clean-images` (run from WSL). Big files live in `~/.beevm` (`BEEVM_HOME`); source stays on Windows.
- Pinned: Firecracker 1.17.0, Go 1.27.3, Alpine 3.24.3 minirootfs, kernel `vmlinux-6.1.155` (taken from the Firecracker CI v1.15 bucket; v1.17 CI has no 6.1 kernel). All checksum-verified.
- Rootfs is built without Docker or sudo (`fakeroot` + `mkfs.ext4 -d`). Guest init is `guest/rootfs/init`.
- `.gitattributes` forces LF line endings (Windows git had `autocrlf=true`, which breaks shell scripts).
- Verified: `make boot-test` passes; `make setup` re-runs in under 1 s; `make clean-images && make setup && make boot-test` passes from scratch.
- **Baseline boot time (full cold boot, 1 vCPU, 128 MiB, nested KVM in WSL2): ~1.7-2.0 s** from process start to exit, kernel reaches init at ~1.3 s. Far above 50 ms, as expected: snapshot restore (M6) is what fixes this. Worth checking whether the slow kernel boot is WSL2 nested virtualization or boot args.
- **Next:** milestone 2 (boot and stop a VM from Go).

### 2026-10-23: Milestone 2
- Added Go module `github.com/om8P/BeeVM` and `internal/vmm` (config, VM lifecycle: New, Start, WaitForConsole, Stop, Kill, Cleanup) on top of `firecracker-go-sdk` (main-branch commit `6fb280e993d4`; its tests target Firecracker 1.4.1 but it works with our 1.17.0).
- `cmd/beevmd boot` boots one VM, tails the console, stops it gracefully and cleans up. Ctrl-C kills the VM and exits 130.
- Guest init now has modes via `beevm.mode=` (`selftest`, `idle`, none = shell). `idle` prints `BEEVM_READY`. The old `beevm.selftest=1` flag was renamed.
- Makefile: `build`, `test`, `test-kvm`, `vet`.
- Verified: 9 unit tests and 3 KVM tests pass; no leftover `firecracker` process or VM folder after normal run, Ctrl-C, missing-images error, and `--keep-logs` (keeps only `console.log`). `make boot-test` from M1 still passes.
- Numbers (WSL2, 1 vCPU, 128 MiB): ready in ~1.6-1.7 s after Start; graceful stop (Ctrl-Alt-Del) 55-90 ms.
- Learned: `SendCtrlAltDel` makes the guest kernel restart directly (default behaviour), so init's SIGINT trap never runs; that is fine. `Machine.Shutdown` may only be called once (closes a channel), so `Stop` guards it with `sync.Once`. SDK `Start` must get a long-lived context, not the caller's timeout context.
- Boot-time hint for M6: in the kernel log there is a ~460 ms gap around "clk: Disabling unused clocks" before the i8042 line; worth investigating when tuning.
- **Next:** milestone 3 (guest agent over vsock).

### 2026-10-24: Milestone 3
- Guest agent: `cmd/beevm-agent` (static Go binary, baked into the rootfs as `/sbin/beevm-agent`) serves HTTP+JSON over vsock port 10000: `GET /health`, `PUT /function` (raw binary, stored in tmpfs `/run/beevm/function`), `POST /invoke` (payload on stdin, JSON on stdout, logs on stderr). Server logic is in `internal/agent` (no vsock code, testable with a normal HTTP server); shared types in `internal/agentproto`.
- Runner: each call runs in its own process group; a timeout kills the whole group; output limits (6 MB stdout, last 64 KB stderr); one call at a time per VM. Failures (exit code, timeout, bad JSON) come back as HTTP 200 with `error` set.
- Host: `internal/vmm/agent.go` (`dialVsock` does Firecracker's `CONNECT <port>` / `OK` handshake on `v.sock`; `AgentClient` with Health, UploadFunction, Invoke; one kept-alive connection). `VM` gets a vsock device (CID 3), `Agent()` and `WaitForAgent()`.
- Init has a new `beevm.mode=agent` (mounts tmpfs on /run, starts the agent, reboots the VM if the agent exits). `build-rootfs.sh` now builds the agent (CGO_ENABLED=0) and rebuilds the image when it changes.
- New: `beevmd invoke`, `make examples`, examples `hello-go-binary` and `testfn`.
- Verified: unit tests (agent 12, host client 6 with a fake Firecracker, plus the older ones) and KVM tests pass (20 correct echo calls, failing function, bad output, timeout then VM still works, hello example, invoke-before-upload gives 409). Nothing left behind after normal runs, timeout, failing function, Ctrl-C (exit 130) and `--keep-logs`. M1 `make boot-test` and M2 tests still pass.
- Numbers (WSL2, 1 vCPU, 128 MiB): agent answers 1.64-1.85 s after Start (cold boot, unchanged from M2); upload of a 2.5 MB function ~75 ms; warm invoke round trip 4-4.5 ms (median), first call ~11 ms; graceful stop 56-85 ms. Agent binary 6.8 MB, rootfs 20 MB on disk.
- Learned: `go get` for vsock also bumped `golang.org/x/*`; fine. Firecracker closes the connection when the guest port is not listening yet, so `WaitForAgent` just retries `/health` every 10 ms. Passing `$VARS` or `|` through `wsl.exe` still breaks: use script files or `make`.
- Not done on purpose (later): zombie reaping of daemonized grandchildren, streaming large outputs, agent auth, reconnecting after a snapshot restore (M6).
- **Next:** milestone 4 (HTTP API, registry and CLI).

### 2026-10-26: Milestone 4
- **Registry** (`internal/registry`, SQLite via modernc, pure Go): tenants, API keys (only the SHA-256 hash is stored; keys are `bvm_` + 43 random chars, 256 bits), functions (unique per tenant, settings: memory 64-1024 MiB, vCPUs 1-4, timeout 1-60 s). Schema version in `PRAGMA user_version` (migrations run on open), WAL mode so `beevmd tenant add` works while the server runs. **Code store** (`internal/codestore`): binaries under `$BEEVM_HOME/functions/<sha256>`, identical bytes stored once, old code deleted when no function uses it.
- **API** (`internal/api`, chi): `PUT/GET/DELETE /api/v1/functions[/{name}]`, `POST /invoke/{name}`, `GET /healthz`. Bearer-key auth (401 + `WWW-Authenticate`, revoked keys stop working immediately); another tenant's function is 404, never 403; deploy checks the ELF magic bytes; invoke answers 200 with the function's output (+ `X-BeeVM-Boot-Ms/Function-Ms/Total-Ms/Vm-Id` headers), 502 when the function fails (exit code and stderr in the body), 503 + `Retry-After` when no VM slot is free, 413/400 for bad payloads, 500 without internal details; one JSON log line per request (slog); panics become 500.
- **Runner** (`internal/runner`): `Runner` interface; `ColdRunner` boots one VM per call (agent mode), limited to `--max-vms` slots, waits `--queue-wait` for a slot, disposes of the VM with Kill+delete (no graceful stop needed). M6/M7 will add runners behind the same interface.
- **Commands:** `beevmd serve` (graceful shutdown: running calls finish, 2nd Ctrl-C stops waiting; own VM folder `run/serve/` cleared on start), `beevmd tenant add|ls`, `beevmd key add|ls|revoke` (tenants and keys are created on the server, no admin key on the network); `beevm deploy|invoke|ls|rm` (cobra; `internal/apiclient`; checks the binary is Linux x86-64 ELF and warns if dynamically linked; payload from flag, file or stdin).
- Verified: unit tests in 8 packages (registry 12, codestore 5, api 15, apiclient 7, runner 4, cmd/beevm 7, plus earlier ones); VM tests incl. 4 end-to-end tests (deploy+invoke, failing/slow function, 5 parallel calls through 2 slots, 503 when busy) and 4 ColdRunner tests; M1-M3 tests still pass. Hand check with a real server, the CLI and curl: 200 with key, 401 without/with wrong key, 502 for a failing function, 404 for unknown/removed functions, deploy replaces, 10 parallel invokes with 4 slots all 200 (waves of 4: ~2.1 s, 4.1 s, 6.1 s), Ctrl-C on the server during a call lets it finish and exits 0. No leftover firecracker processes or folders in any case.
- Numbers (WSL2, 1 vCPU, 128 MiB): one invoke over HTTP **2.5-2.6 s** = VM boot until agent ready 2.0-2.1 s (earlier runs 1.6-1.9 s; this machine varies) + ~0.45 s for everything else (copying the 64 MB rootfs, upload of 2.5 MB, the call itself 9-18 ms, kill and cleanup). Warm path does not exist yet.
- Decisions/differences from the plan: key prefix is 12 characters (`bvm_` + 8), not 8, to make prefixes unique enough to revoke by; client hang-up is logged as status 499; `beevmd serve` assumes it is the only server using `run/serve/`; a tiny race exists if two deploys of identical code race with a delete (code could be removed just before the second deploy registers it): acceptable for now.
- Noted for later: the per-call rootfs copy (~0.4 s) is a clear target for M6 tuning (read-only base + overlay, or reflink); `go get` for cobra upgraded an old indirect v0.0.3 to v1.10.2.
- **Next:** milestone 5 (artifact store with MinIO/S3; can be swapped later) or, per the order in `docs/milestones.md`, milestone 6 (snapshots, the < 50 ms core).

### 2026-10-26: Milestone 5
- **Artifact store** (`internal/store`): `Blobs` interface (Put/Get/Exists/Delete) with an `S3` implementation on aws-sdk-go-v2 (works for MinIO and AWS S3; credentials from the standard AWS chain, never flags) and an in-memory `Mem` for tests. Uploads send a SHA-256 checksum so the server verifies them. Keys are `functions/<sha256>`.
- **codestore is now a cache** in front of the store: `Publish` (upload if missing, so identical code uploads once), `Ensure` (local hit, or download, check the hash, move into place; parallel misses share one download via singleflight), `Delete` removes local and remote. Without a bucket it behaves exactly like M4.
- **API:** deploy uploads to S3 *before* registering (S3 down gives 503 + `Retry-After`, nothing registered); invoke calls `Ensure` and returns `X-BeeVM-Code-Cache: hit|miss` and `X-BeeVM-Fetch-Ms` (also in the request log; `beevm invoke -v` prints them). Cached code keeps working while S3 is down.
- **Config:** `beevmd serve --s3-bucket/--s3-endpoint/--s3-region/--s3-path-style` or `BEEVM_S3_*` env. No bucket = local-only with a warning. A configured but unreachable store makes the server refuse to start. Real AWS = unset endpoint + real credentials; nothing else changes.
- **Local MinIO:** `make minio` (no Docker), `make minio-up/minio-down` (Compose), settings in `deploy/minio.env` (throw-away dev credentials), `make test-s3`.
- **MinIO is archived:** dl.min.io now answers 410 Gone for every community binary, so `scripts/minio.sh` builds the pinned source (`MINIO_GO_VERSION` in `scripts/common.sh`, Go checksum database verifies it) instead of downloading. First build takes a few minutes. The Compose image tag (`quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z`) is from memory and **not tested** (no Docker in WSL here); if the pull fails use `make minio`. The code only speaks plain S3, so any S3-compatible server works.
- Verified: unit tests in 9 packages (store 5, codestore 10 new, api 8 new, run with `-race`), `make test-s3` against real MinIO, `make test-kvm` (all M1-M4 tests still pass). By hand with a real server + MinIO: deploy puts one object in the bucket; after wiping the local cache the first invoke is a miss (18 ms fetch) and the next a hit; deploying identical code under a second name adds no object; removing both functions empties the bucket; with MinIO stopped, deploying new code gives 503 while cached functions still run and the server refuses to start; without a bucket it runs local-only. No leftover processes.
- Not done on purpose (later): cache size limit/eviction, retries with backoff on failed downloads, presigned direct uploads from the CLI, a KVM end-to-end variant backed by S3 (covered by the manual run), snapshot backups to S3 (M6).
- Learned: S3 bucket names must be lowercase; HEAD responses have no body so "not found" must be recognised by status/error code; Git Bash on Windows rewrites `/mnt/...` paths unless `MSYS_NO_PATHCONV=1` is set when calling `wsl.exe`.
- **Next:** milestone 6 (snapshots and fast restore, the < 50 ms core).

### 2026-10-27: Milestone 6
- **Spike first** (KVM tests in `internal/vmm/snapshot_kvm_test.go`) confirmed the plan: a VM can be restored into a different folder when Firecracker runs inside the VM folder and the vsock socket path is relative (`v.sock`); 5 parallel restores from one memory file work (Firecracker maps it privately, copy-on-write).
- **Shared read-only rootfs** (`vmm.Config.SharedRootfs`): no per-VM copy any more (saved ~0.4 s per cold call too); the kernel mounts it `ro`; init mounts tmpfs on `/run` and `/tmp`. Tested: writing to `/` fails, `/tmp` works and is private per VM.
- **vmm:** `VM.Pause`, `VM.CreateSnapshot`, `vmm.Restore` (own small Firecracker API client `fcapi.go`: polls the API socket every 1 ms, `PUT /snapshot/load` with `mem_backend` File and `resume_vm`), `RestoreTimings{Spawn, Load}`, agent polling every 1 ms after restore, `VM.StartedAt`.
- **Restored VMs are clones:** the guest clock was 3-9 s behind and all copies share one random-number state. New agent endpoint `POST /resume` (host time + 64 random bytes): sets the clock, adds the bytes with `RNDADDENTROPY` and forces `RNDRESEEDCRNG`. VMGenID is not active in our guest (no ACPI driver), so this is needed. (Corrected 2026-11-02: VMGenID *is* active with Firecracker 1.17, see the M6 refine entry; the reseed stays as a second safeguard.) Tested: clock within 1 s, two clones give different random bytes.
- **Snapshot Manager** (`internal/snapshot`): key = hash of Firecracker version, kernel SHA-256, rootfs SHA-256 and path, boot args, memory, vCPUs (file hashes cached by size+mtime). `Ensure` builds once (single-flight, survives the caller hanging up) in `tmp-*` and renames into `snapshots/<key>/`; `List` marks stale snapshots, `RemoveStale`, `Remove`. Build = boot, wait for agent, **settle 1 s**, close connections, pause, snapshot (~2.1-2.5 s).
- **Runner:** shared slot limiter; `SnapshotRunner` (restore -> resume -> upload -> invoke -> kill), falls back to booting if a snapshot cannot be built (no retry for a minute per size). `Result.StartKind`, `UploadMS`; headers `X-BeeVM-Start`, `X-BeeVM-Upload-Ms`; `beevm invoke -v` shows them.
- **Commands:** `beevmd serve --runner snapshot|cold` (snapshot is default; builds the default snapshot at start-up and removes stale ones), `beevmd snapshot build|ls|rm`, `beevmd invoke --restore`, `beevmd bench coldstart`.
- Verified: unit tests in 10 packages (new: snapshot 8, runner 4, agent resume 3, bench 2, API header check) incl. `-race`; all KVM tests (new: 3 vmm snapshot tests, real snapshot build+restore, end-to-end API with SnapshotRunner) and all M1-M5 tests pass; by hand `serve` with both runners, `snapshot ls`, `invoke --restore`. No leftover processes or folders.
- **Benchmark** (`beevmd bench coldstart --runs 100`, WSL2 nested KVM, 1 vCPU, 128 MiB, page cache warm, one at a time):

  | ms | p50 | p90 | p99 | max |
  |---|---|---|---|---|
  | boot: VM ready | 936.8 | 1020.8 | 1186.1 | 1350.0 |
  | boot: first response | 955.8 | 1077.3 | 1213.9 | 1393.9 |
  | restore: spawn | 1.4 | 2.0 | 2.9 | 5.0 |
  | restore: load snapshot | 23.8 | 32.4 | 38.0 | 39.6 |
  | restore: VM ready (incl. resume) | **47.0** | 55.3 | **59.1** | 63.8 |
  | restore: upload 2.5 MB | 27.6 | 33.0 | 79.7 | 85.1 |
  | restore: invoke | 15.2 | 16.4 | 17.3 | 17.9 |
  | restore: first response | 91.0 | 100.1 | 138.0 | 149.9 |

  Restore is ~20x faster than boot. **p50 is under 50 ms, p99 (59 ms) is not** on WSL2. Over HTTP a restored call takes ~130-160 ms in total.
- Learned:
  - **Settle before snapshotting.** A snapshot taken right after the agent starts makes the first reply after *every* restore take ~850 ms (the guest kernel accepts the vsock connection in ~6 ms, but the agent answers only after ~850 ms). Letting the VM run 0.5 s or more first brings it to 5-15 ms. Measured with a settle sweep (0, 0.2, 0.5, 1, 1.5, 2 s).
  - **Lazy memory loading shows up in the first call.** After a restore, upload (28 ms vs 15 ms) and the call (15 ms vs 3 ms) are slower than after a boot: guest memory pages are read from the snapshot file the first time they are touched.
  - The SDK's `WithSnapshot` still runs its vsock setup (Firecracker refuses configuration before a load) and polls every 10 ms, hence the own minimal API client.
  - The Bash tool on this machine rewrites backslash escapes in heredocs (a `\n` in a Go string became a real line break several times); use the Edit/Write tools for code with escapes.
- **Refine later (M6 refine):** the snapshot load (24 ms p50) dominates; try smaller guest memory, hugepages, a cold-page-cache run, and measure on the cloud node (no nested virtualization). The kernel's 0.5 s i8042 probe at boot (`i8042.noaux` etc.) only matters for cold boots. Snapshot backup to S3 (deferred).
- **Next:** milestone 7 (warm pool: reuse running VMs for the ~1 ms path; restored VMs fill the pool).

### 2026-11-02: Milestone 6 refine (restore tuning)
- **Goal:** restore p99 under 50 ms (October: p50 47.0, p99 59.1 ms). Refine rule: no public interface changes (everything added is additive). Plan: measure first, cheap fixes, then experiments, 100 restores per change. Full write-up with every table: `developer-guide/guide.md` section 8.10. Raw results: `bench/results/m6refine/` (19 runs, git-ignored; file list in guide 8.10).
- **Result: p99 not reached on WSL2.** Final runs: VM ready p50 **45.2 / 45.8 ms**, p90 55.8 / 53.2, p99 **65.9 / 67.2 ms**. October's 59 ms was a quiet run; today's untouched baseline gave p99 61-77 ms. The agent phase's tail is fixed (p99 24-26 ms instead of 38-55 ms); what is left is KVM noise in the load and page-fault cost, both from nested virtualization.
- **Power mode first.** The first baseline was ~2.5x slower in every phase (restore ready p50 115-120 ms, agent 80-83 ms, upload 93-95 ms, invoke 44-45 ms, first response 257-260 ms; 15 boots 1844 ms). Cause: Legion Balance Mode, 31 % battery. In Performance mode, plugged in: 15 boots 942 ms, restore ready 46.8-48.7 ms. Those Balance-mode files were deleted when the baseline restarted; numbers are in the guide.
- **Where the ~46 ms go** (p50): spawn 1.5, load 23, agent 21 (October's data already showed the hidden ~21 ms: "vm ready" minus spawn minus load, 9-33 ms, steady; slow runs were slow because of the load, 11-40 ms).
  - *Load* is inside Firecracker: our request and Firecracker's own `load_snapshot` latency differ by 0.2-0.7 ms. A hand probe confirmed `PUT /metrics` is accepted before `/snapshot/load` and `FlushMetrics` writes one JSON line (`vmm_*` latencies stay 0). The console log of a kept restore showed 32 ms between "Snapshot CPU vendor ID" and "Artificially kick devices" (load 32.7 ms). `firecracker --level Trace` showed `event_start/event_end: build microvm from snapshot` = 10.3 of a 10.7 ms load, incl. building the VMGenID device (0x000dfff0, IRQ 7) and a VMClock notify; everything else < 0.5 ms. Load deciles over 100 runs: 11.4 ... 33.6 ms, evenly spread (no fast/slow groups) = host scheduling noise in nested KVM. `strace` is not installed (needs sudo).
  - *Agent*: ~520-590 page faults of the firecracker process during the first request (counted from `/proc/<pid>/stat` minflt + majflt) at ~37 µs each = ~20 ms; upload + invoke add ~1,410-1,470 faults at ~27 µs. Fewer faults = faster (330 faults: 14.1 ms; 426: 17.7 ms). The same VM's first three calls: 16.4, 6.3, 4.4 ms. Each first touch of a guest page needs an EPT fix-up through Hyper-V and the WSL kernel's KVM.
- **Kept (code):**
  - `vmm.VM.WaitAndResume(ctx)` + `vmm.ResumeInfo{Ready, Attempts, ClockAdjustedMS}`: polls with `/resume` directly (1 ms interval after restore, 500 ms per attempt); stops at once when the agent answers with an error (new typed `statusError` in `agent.go`, same message text). Used by `runner.restoreAndRun` (`BootMS` = `info.Ready`). Agent p50 22.2-23.3 -> 20.6-21.7 ms, p99 37.8-55.1 -> 24.4-43.9 ms (final runs 23.6-26.0). `attempts` was 1 in every run.
  - `snapshot.buildVM` calls `/resume` once before the 1 s settle (resume code already in guest memory). Same setup (throw-away snapshot, settle 1 s): agent 19.2 -> 18.0 / 19.2 ms, ready 46.2 -> 41.9 / 43.3 ms, faults ~unchanged (504 -> 519). No function upload before snapshotting (would leave code in every VM).
  - `snapshot.Inputs.BuildVersion` + constant `BuildVersion = 2` (1 = M6 procedure). Old snapshots `f98f99ee...` and `411a4fdd...` became stale; new `f6b97d9527d38143d667` built automatically in 2,106 ms. `snapshot ls` label now "stale (images or build changed)". `Manager.SetSettle` for experiments.
  - `vmm.Config.FirecrackerMetrics` + `VM.FirecrackerLoadTime` (`PUT /metrics` before the load, `FlushMetrics` after, read `latencies_us.load_snapshot`); `vmm.Config.CPUs` + `pinProcess` (pins every thread in `/proc/<pid>/task` once the API answers; the vCPU thread inherits it; a first version pinned right after spawn and could miss the API thread). `golang.org/x/sys` is now a direct dependency.
  - Benchmark (`cmd/beevmd/bench.go`): rows spawn, load, fc load*, connect*, agent, attempts*, faults agent, faults call (*optional, hidden when unused); CSV columns `fc_load_ms, connect_ms, agent_ms, attempts, faults_agent, faults_call` added; "Settings" line in the summary; flags `--probe resume|health`, `--fc-metrics`, `--cold-cache` (`posix_fadvise(DONTNEED)` on firecracker, kernel, rootfs, snapshot; no sudo; not while `serve` runs), `--settle D` (throw-away snapshot in `run/bench/snapshots`, deleted at the end; same key as the real one because settle is not in the key), `--gap D`, `--pin CPUS`. `run/bench` is now also removed at the end.
- **Tried, no effect** (p50 / p99 VM ready, ms): settle 0.5 s 46.2/60.2, 1 s 46.2/62.0, 2 s 47.7/61.0, 5 s 45.2/60.7; 64 MiB 46.6/77.1 (more faults in the call: 1,597, upload p99 142 ms), 256 MiB 45.5/74.4; 100 ms gap 46.9/65.0; pinned to CPU 2 47.6/73.6; plain 47.0/67.2. Load does not depend on memory size (it maps, not reads). Pinning cannot pick a fast core inside WSL (virtual CPUs float over P- and E-cores); it stays a bench flag.
- **Cold page cache** (30 runs): restore ready 84.3 ms p50 (spawn 6.5, load 23.9, agent 52.8, same fault count), first response 137.4 ms; boot 978.7 ms.
- **Verified:** unit tests in 11 packages (new: 7 in `vmm/tuning_test.go`, 4 bench, 2 snapshot + a key variant), `-race` for vmm/snapshot/runner/cmd/beevmd, `make test-kvm` all pass (new `TestRestoreWithMetricsAndPinning`; KVM log: spawn 1.4, load 16.2, ready 41.4 ms; our load 19.06 vs Firecracker's 18.79 ms), `go vet` incl. `-tags kvm`, `gofmt`. No leftover processes or folders.
- **Learned:**
  - Check the power mode before benchmarking (now in CLAUDE.md and guide Appendix D).
  - **VMGenID works** with Firecracker 1.17: a restored guest logs `random: crng reseeded due to virtual machine fork`. The M6 note "VMGenID not active" was wrong (corrected in the guide and learning notes). The agent's reseed stays as a second safeguard; the clock still needs `/resume` (Firecracker's VMClock device has no driver in our 6.1 guest).
  - Identical runs can differ by 15 ms at p99; compare within one session and repeat key runs.
  - Tooling: the first `go vet` over `/mnt/c` took ~5 min and a 400 s tool timeout killed `go test` (exit 15, no output): run long jobs in the background with a log. `python3` in Git Bash is Windows Python (a heredoc trick opened its prompt). `gofmt -w` fixed a struct alignment.
- Cleaned up: duplicated M5 entry here, duplicated artifact-store line in CLAUDE.md. Two stale snapshots stay until `serve` or `snapshot rm --stale` removes them.
- Committed as `e5d30d2` together with refine 2.

### 2026-11-02: Milestone 6 refine 2 (userfaultfd prefetch): spike only, deferred
- **Idea** (REAP paper, Ustiugov et al., ASPLOS 2021): a restored function touches nearly the same ~2,000 pages every time; record them once and copy them into the VM in one go at restore, instead of ~27-37 µs per fault. Tool: userfaultfd, through Firecracker's `mem_backend` type `Uffd`.
- **Firecracker 1.17 (from its docs):** our handler listens on a Unix socket (`backend_path`); Firecracker creates the userfaultfd, registers guest memory, connects and sends the fd plus the memory layout (region sizes, page size), then never talks again; the handler fills pages with `UFFDIO_COPY`, zeroes them on `UFFD_EVENT_REMOVE` (balloon only); if the handler dies Firecracker hangs (use timeouts; `SO_PEERCRED` shows the peer). On kernels >= 6.1 it uses `/dev/userfaultfd`, access by file permissions; docs suggest `sudo setfacl -m u:${USER}:rw /dev/userfaultfd` without the jailer. The docs do not name the JSON fields: read `src/firecracker/examples/uffd/` before building.
- **Spike in WSL:** `CONFIG_USERFAULTFD=y` (also `_WP`, `_MINOR`, `PTE_MARKER_UFFD_WP`), but `vm.unprivileged_userfaultfd = 0`, `/dev/userfaultfd` is `crw------- root root`, `CapEff: 0`, `ptrace_scope 1`. Test as normal user: full userfaultfd "Operation not permitted", user-mode-only OK (useless: KVM's faults happen in the kernel), `/dev/userfaultfd` "Permission denied". **Cannot work without a system change.**
- **Decision (user):** don't loosen the laptop's security setting (full userfaultfd is a known kernel-exploit helper; the setfacl would also reset on every WSL restart). Try it on the cloud node (M13, note added to `docs/milestones.md`), where Firecracker runs with root/jailer and without nested virtualization. Guide section 8.11.
- **Next:** milestone 7 (warm pool).

### 2026-11-03: Milestone 7 (warm pool)
- **Idea:** keep a VM after its call; the next call of the same function reuses it (no restore, no upload). Plan: `C:\Users\ommga\.claude\plans\give-plan-humming-wilkes.md` (agreed: prewarm as an on-demand endpoint, VM list included).
- **`internal/pool`** (no Firecracker imports, tested with fake VMs and a fake clock):
  - State machine `Restoring → Idle → Busy → Recycling → Destroyed` in `state.go`; one table of allowed moves, every other move is an error.
  - Pool key = tenant + function ID + code SHA-256 + memory + vCPUs: VMs are never shared between functions, even with identical code (a file in `/tmp` stays inside one function's VMs). A redeploy changes the key.
  - `Acquire`: newest idle VM of the key (warm), else a new one if a slot is free, else evict the least recently used idle VM of any function (LRU), else wait up to `QueueWait` (ErrBusy). Waiters sleep on a "broadcast channel" (closed and replaced on every change). The mutex is never held while creating or destroying a VM.
  - `MaxVMs` counts every state except Destroyed (idle VMs use memory too). Reaper goroutine: idle longer than `KeepAlive` → recycled. A VM whose process exits while idle is removed; while busy it is not reused.
  - `Release(healthy)`, `Prewarm` (only free slots, never evicts, parallel creates), `Drain(functionID)` (idle now, busy after its call, restoring when ready), `Stats(tenant)`, `Close(ctx)`.
- **`runner.WarmRunner`** (new default): creates VMs with the M6 path (restore + `/resume`, boot fallback via the shared `snapshotPicker`) and uploads the code once. After a call the VM is kept unless the agent/connection failed, the caller hung up, or the function timed out (`timeout after` prefix); a normal function failure keeps it. Refactor: `bootReady`, `restoreReady`, `uploadCode`, `callFunction` split out of the old helpers (behaviour unchanged). `Request` gained `TenantID, FunctionID, CodeSHA256`; `StartWarm = "warm"`; optional interfaces `Prewarmer`, `Drainer`, `VMLister`.
- **API/CLI:** `X-BeeVM-Start: warm` (boot and upload 0); redeploy and delete drain the function's VMs; `POST /api/v1/functions/{name}/prewarm?count=N` (1..max-vms; 404, 501 without a pool, 503 full) + `beevm prewarm <name> -n N`; `GET /api/v1/vms` (own tenant only) + `beevm vms`; `beevm invoke -v` prints `warm`.
- **Serve:** `--runner warm|snapshot|cold` (warm default), `--keep-alive 60s`; the pool is closed on shutdown (no VMs left). **Bench:** `--mode warm` (real WarmRunner, 1 slot; every measured call must be warm).
- **Verified:** unit tests incl. `-race` for pool (25 tests: every transition pair, warm hit, separate VMs for parallel calls, TTL with fake clock, LRU, busy, cancel, failed create, crash while idle/busy, drain incl. while creating, prewarm, close, 40-goroutine stress), runner (7), api (5), apiclient, bench table; `go vet` (also `-tags kvm`), gofmt; full `make test-kvm` (179 passing) with 4 new end-to-end warm tests: repeat calls reuse the VM (restore 105 ms vs warm 1-4 ms server total), reap after keep-alive leaves no process/folder, prewarm, redeploy and delete drain; two functions with the same code do not see each other's `/tmp/secret`; timeout replaces the VM, normal failure keeps it; 10 parallel calls on 2 slots = 2 restores + 8 warm. Hand check (throw-away `BEEVM_HOME`, keep-alive 3 s): restore 79 ms, then warm 2 / 1 ms (round trip 5-6 ms); `beevm vms` idle with 3 calls; reaped after 3 s; prewarm 2 VMs in 344 ms; Ctrl-C exit 0, nothing left.
- **Benchmark** (`bench coldstart --runs 100 --mode restore,warm`, WSL2):

  | ms | p50 | p90 | p99 | max |
  |---|---|---|---|---|
  | restore: vm ready | 44.2 | 52.2 | 60.8 | 61.4 |
  | restore: first response (incl. upload + call) | 88.4 | 101.2 | 113.6 | 156.3 |
  | **warm: first response** | **1.6** | 2.7 | **5.0** | 10.0 |

  The warm path is ~55x faster than a restore. Pool bookkeeping is microseconds; the rest is the vsock round trip and starting the function's process in the guest.
- **Learned / noted:**
  - The first call on a **prewarmed** VM took 16 ms, not ~1 ms: its code was uploaded but never run, so the guest pages for running it are still faulted in lazily (as in M6). Refine idea: a throw-away call during prewarm (needs a "warm-up payload" contract) or page prefetch on the cloud node.
  - `/tmp` persists between warm calls of the same function (as in AWS Lambda); never across functions.
  - Idle VMs are not paused (their vCPU is halted anyway); pausing to save CPU is a refine idea, measure first.
- **Refine later:** `go test -race` under KVM load, eviction policy (LRU across tenants could let one tenant push out another's VMs: per-tenant limits belong with M9), prewarm warm-up call, idle VM memory for M12 density.
- **Next:** milestone 9 (isolation and networking) per the must-do order, or milestone 8 (Python/Node runtimes).
