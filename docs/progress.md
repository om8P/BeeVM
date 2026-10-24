# BeeVM progress log

Update this at the end of every working session: what got done, what's broken, what's next.
Details of each milestone: `docs/milestones.md`.

## Milestones

| # | Milestone | Status |
|---|-----------|--------|
| 1 | Environment and a hand-booted MicroVM | Done, needs refinement (boot ~1.8 s under WSL2; no `kvm` test yet) |
| 2 | Boot and stop a VM from Go | Done |
| 3 | Guest agent over vsock, first function runs | Done |
| 4 | HTTP API, registry and CLI | Not started |
| 5 | Artifact store with MinIO/S3 | Not started |
| 6 | Snapshots and fast restore (the core) | Not started |
| 7 | Warm pool | Not started |
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
