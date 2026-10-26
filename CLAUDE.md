# BeeVM

Group project for Advanced Cloud Computing, **Case Study 06: Lightweight MicroVM FaaS Runtime Engine**.
BeeVM is a Function-as-a-Service runtime: it runs user functions inside Firecracker MicroVMs on a bare-metal Linux node, using warm pools and snapshot restore for fast cold starts.

- Full design: `docs/architecture.md` (read it before starting a milestone).
- Progress log: `docs/progress.md` (read it first; update it at the end of every session).
- Milestone guide: `docs/milestones.md` (13 session-sized milestones; this supersedes the 9 milestones listed in the architecture doc).
- Case study source: `Advanced Cloud System Software Design_ 30 Case Studies.docx`. Only group 06's title, summary and overview-table row are valid. Its detailed sections 1–5 are a copy-paste error about an HNSW vector engine — ignore them.

## Grading criteria (from the case study)
- Cold-start latency **< 50 ms** (via snapshot restore)
- Tenant isolation, verified by tests
- MicroVM density per node

## Decisions (do not change without asking)
- Control plane in **Go** (1.24+; installed 1.27.3), `firecracker-go-sdk` (pinned to a main-branch commit, see `go.mod`; last tag is from 2022), `go-chi/chi`, SQLite via `modernc.org/sqlite`.
- Host ↔ guest communication over **vsock**, not the network.
- **One network namespace per VM**, so restored snapshots can keep the same guest IP.
- Snapshots live on **local NVMe**; S3/MinIO is only the source and backup.
- Guest runtimes: **Python 3.12, Node.js 20, static binaries**; Alpine rootfs, `vmlinux` 6.1.
- Isolation: Firecracker **jailer**, cgroups v2, seccomp, nftables between tenants.
- Web dashboard (`web/`) and Prometheus/Grafana (`deploy/monitoring/`) are **optional and swappable**: they only use the public REST/SSE API and `/metrics`. Core code must never import them.
- Component names: API Gateway & HTTP Router, Function Registry, Scheduler & Warm Pool, Snapshot Manager, VM Manager, Isolation & Network, Observability.

## Environment
- Windows 11 host; code runs inside **WSL2 Ubuntu 24.04**.
- `/dev/kvm` exists in WSL (owned by `root:kvm`); the user must be in the `kvm` group (done in milestone 1 setup script).
- Final benchmarks on a cloud Linux node with nested virtualization or a short bare-metal (i3en.metal) run.

## Working style
- The user is a CS student and wants to understand the code: explain new concepts briefly as you go.
- One milestone per session. Plan first, then build, then test, then update `docs/progress.md`.
- Integration tests that need KVM use the build tag `//go:build kvm`.

## Commands
Run everything from a **WSL terminal** inside this folder (`/mnt/c/Users/ommga/Desktop/ACC/BeeVM`). Source code stays on Windows; big files (kernel, rootfs, snapshots, run folders) live in `$BEEVM_HOME` (default `~/.beevm`) on the fast WSL disk.

- `make setup`: one-time, re-runnable install of Go, Firecracker, kernel and rootfs (asks for sudo).
- `make images`: rebuild kernel and rootfs only (no sudo).
- `make boot`: boot a MicroVM and get a shell (`reboot -f` to exit).
- `make boot-test`: boot automatically, check it worked, print boot time.
- `make clean-images`: delete images and run folders.
- `make build`: build `bin/beevmd` (server) and `bin/beevm` (user CLI).
- Run the service: `./bin/beevmd tenant add <name>` (prints the API key once; also `tenant ls`, `key add|ls|revoke`), then `./bin/beevmd serve [--addr 127.0.0.1:8080] [--max-vms 8]`. Users: `export BEEVM_API_KEY=...`, then `beevm deploy <name> <binary>`, `beevm invoke <name> --payload '{...}' [-v]`, `beevm ls`, `beevm rm <name>`. API: `PUT /api/v1/functions/{name}` (multipart), `POST /invoke/{name}`; keys go in `Authorization: Bearer`.
- Data lives in `$BEEVM_HOME`: `beevm.db` (SQLite registry), `functions/<sha256>` (code), `run/serve/` (the server's VM folders, cleared on start), `run/vms/` (`boot`/`invoke` commands).
- `./bin/beevmd boot [--hold 30s] [--keep-logs]`: boot one VM through the Go code, show console, stop, clean up.
- `make examples`: build example functions (static) into `bin/functions/`. `./bin/beevmd invoke --code bin/functions/hello-go-binary --payload '{"name":"Om"}' --repeat 5`: boot a VM with the guest agent, upload the binary, run it, clean up.
- The guest agent (`cmd/beevm-agent`) is built into the rootfs by `make rootfs` / `make images`; rebuild images after changing agent code.
- `make test`: Go unit tests (no KVM). `make test-kvm`: tests that boot real VMs (`//go:build kvm`), all packages including the end-to-end API test. `make vet`: go vet.
- Calling `go` from Windows tools via `wsl.exe` mangles quotes and `$VARS`; put multi-line commands in a script file and run `wsl.exe -- bash <file>`, or use `make`.

(Add more as they come to exist.)
