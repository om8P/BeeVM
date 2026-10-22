# BeeVM progress log

Update this at the end of every working session: what got done, what's broken, what's next.
Details of each milestone: `docs/milestones.md`.

## Milestones

| # | Milestone | Status |
|---|-----------|--------|
| 1 | Environment and a hand-booted MicroVM | Done, needs refinement (boot ~1.8 s under WSL2; no `kvm` test yet) |
| 2 | Boot and stop a VM from Go | Done |
| 3 | Guest agent over vsock, first function runs | Not started |
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
- Added Go module `github.com/OmMhatre-99/BeeVM` and `internal/vmm` (config, VM lifecycle: New, Start, WaitForConsole, Stop, Kill, Cleanup) on top of `firecracker-go-sdk` (main-branch commit `6fb280e993d4`; its tests target Firecracker 1.4.1 but it works with our 1.17.0).
- `cmd/beevmd boot` boots one VM, tails the console, stops it gracefully and cleans up. Ctrl-C kills the VM and exits 130.
- Guest init now has modes via `beevm.mode=` (`selftest`, `idle`, none = shell). `idle` prints `BEEVM_READY`. The old `beevm.selftest=1` flag was renamed.
- Makefile: `build`, `test`, `test-kvm`, `vet`.
- Verified: 12 unit tests and 3 KVM tests pass; no leftover `firecracker` process or VM folder after normal run, Ctrl-C, missing-images error, and `--keep-logs` (keeps only `console.log`). `make boot-test` from M1 still passes.
- Numbers (WSL2, 1 vCPU, 128 MiB): ready in ~1.6-1.7 s after Start; graceful stop (Ctrl-Alt-Del) 55-90 ms.
- Learned: `SendCtrlAltDel` makes the guest kernel restart directly (default behaviour), so init's SIGINT trap never runs; that is fine. `Machine.Shutdown` may only be called once (closes a channel), so `Stop` guards it with `sync.Once`. SDK `Start` must get a long-lived context, not the caller's timeout context.
- Boot-time hint for M6: in the kernel log there is a ~460 ms gap around "clk: Disabling unused clocks" before the i8042 line; worth investigating when tuning.
- **Next:** milestone 3 (guest agent over vsock).
