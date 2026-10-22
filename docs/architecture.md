# BeeVM — Architecture & Technology Plan

## Context

BeeVM is the group project for Advanced Cloud Computing, Case Study 06: **Lightweight MicroVM FaaS Runtime Engine**.
From the overview table: cloud primitives are *bare-metal cloud instances + cloud storage*; the system layer is a
*Firecracker MicroVM orchestrator with warm-snapshot pooling*. Grading criteria: **cold start < 50 ms**,
**tenant isolation verification**, **MicroVM density per node**.

(The doc's detailed sections 1–5 for group 06 are a copy-paste error about an HNSW vector engine and are ignored.)

Decisions made with the user:
- Control plane in **Go**
- Develop on **WSL2 (nested KVM)**, final benchmarks on a **cloud VM / short bare-metal run**
- User functions: **Python, Node.js, and static Linux binaries**
- Both a **web dashboard** and **CLI + Prometheus/Grafana**, built so either one can be deleted without touching the core

Project lives in `C:\Users\ommga\Desktop\ACC\BeeVM\` (currently holds only the case-study .docx). Code runs inside WSL2 Ubuntu.

---

## High-level architecture

```
                 ┌──────────────┐   ┌──────────────┐   ┌───────────────────┐
  Clients ──────▶│  beevm CLI   │   │ Web Dashboard│   │ Prometheus+Grafana│   (optional, swappable)
  (curl/k6)      └──────┬───────┘   └──────┬───────┘   └────────┬──────────┘
                        │ REST            │ REST + SSE          │ scrape /metrics
                        ▼                 ▼                     ▼
 ┌────────────────────────────── beevmd (Go daemon, one per bare-metal node) ──────────────────────────────┐
 │  API Gateway & HTTP Router  (/invoke/{fn}, /api/v1/*, /metrics, /events)  + tenant API-key auth          │
 │        │                                                                                                 │
 │  Function Registry (SQLite)  ◀──▶  Artifact Store client (S3 API: MinIO local / AWS S3 cloud)            │
 │        │                                                                                                 │
 │  Scheduler & Warm Pool Manager  ── per-function pools, keep-alive TTL, min/max idle, LRU eviction        │
 │        │                                                                                                 │
 │  Snapshot Manager  ── runtime snapshots + per-function snapshots on local NVMe, backup to S3             │
 │        │                                                                                                 │
 │  VM Manager (firecracker-go-sdk) ── create / restore / pause / destroy MicroVMs                         │
 │        │                                                                                                 │
 │  Isolation Provisioner: Jailer (chroot+uid) · cgroups v2 · seccomp · network namespaces · TAP · nftables │
 └────────┼────────────────────────────────────────────────────────────────────────────────────────────────┘
          ▼  vsock (HTTP over vsock)
 ┌──────────────── Firecracker MicroVM (×N) ────────────────┐
 │  Guest kernel (vmlinux 6.1) + ext4 rootfs (Alpine)       │
 │  beevm-agent (Go static binary, PID 1 helper)            │
 │  Runtime: python3 shim | node shim | exec(binary)        │
 └──────────────────────────────────────────────────────────┘
```

---

## Components and technologies

| # | Component | What it does | Technology |
|---|-----------|--------------|------------|
| 1 | **API Gateway / Router** | `POST /invoke/{fn}`, admin REST (`/api/v1/functions`, `/vms`, `/tenants`), live events (SSE), `/metrics` | Go 1.23+, `net/http` + `go-chi/chi`, SSE |
| 2 | **Auth / tenancy** | Per-tenant API keys; every function and VM tagged with tenant | SHA-256 hashed keys in SQLite |
| 3 | **Function Registry** | Name, tenant, runtime (`python`/`node`/`binary`), memory MB, vCPU, timeout, code hash, snapshot paths | SQLite via `modernc.org/sqlite` (pure Go, no CGO) |
| 4 | **Artifact Store** | Stores uploaded function packages (zip/binary), snapshot backups | **MinIO** locally, **AWS S3** in cloud — same code via `aws-sdk-go-v2` S3 client |
| 5 | **Scheduler & Warm Pool** | `Acquire(fn)` → idle VM, else restore from snapshot; `Release(vm)` → back to pool or destroy; keep-alive TTL, min-idle prewarm, max VMs per node, LRU eviction under memory pressure | Go goroutines/channels; VM state machine: `Restoring → Idle → Busy → Recycling → Destroyed` |
| 6 | **Snapshot Manager** | Two tiers: **runtime snapshot** (booted VM with agent + Python/Node loaded) and **function snapshot** (runtime + this function's code imported). Stored on local NVMe, copied to S3 | Firecracker snapshot API (`PUT /snapshot/create`, `PUT /snapshot/load`), file-backed memory with lazy page loading (mmap) |
| 7 | **VM Manager** | Starts Firecracker processes, sets kernel/rootfs/drives/vsock/machine config, restores snapshots, kills VMs | **Firecracker** (latest v1.x), **firecracker-go-sdk** |
| 8 | **Network Provisioner** | One **network namespace per VM** containing a TAP with the *same* guest IP (so restored snapshots work unchanged); veth pair to host; NAT; default-deny egress between tenants | `vishvananda/netlink`, `vishvananda/netns`, **nftables** (`google/nftables`) |
| 9 | **Isolation Provisioner** | Jailer (chroot + unique UID/GID per VM), **cgroups v2** (`cpu.max`, `memory.max`, `pids.max`) under `beevm/<tenant>/<vm>`, Firecracker seccomp filters | Firecracker **jailer**, `containerd/cgroups/v3` |
| 10 | **Guest Agent** | Runs inside every VM; listens on vsock; receives `{code, payload}`; runs handler; returns result, stdout, duration | Go static binary `beevm-agent`, HTTP over `mdlayher/vsock` |
| 11 | **Runtime shims** | Python: persistent process importing `handler.py` → `handler(event)`. Node: same with `index.js`. Binary: exec with JSON on stdin, JSON on stdout | Python 3.12, Node.js 20, small shim scripts |
| 12 | **Guest images** | Kernel + 3 rootfs images: `base` (agent only, for binaries), `python`, `node` | Firecracker CI `vmlinux-6.1`, rootfs built from **Alpine** via Docker export → `mkfs.ext4` script |
| 13 | **Observability** | Metrics: cold/warm latency histograms, restore time, pool size, VMs per node, memory per VM. Structured logs | `prometheus/client_golang`, Go `log/slog` (JSON) |
| 14 | **CLI** `beevm` | `deploy`, `invoke`, `ls`, `rm`, `vms`, `logs`, `bench` | Go + `spf13/cobra` |
| 15 | **Web Dashboard** *(optional module)* | Live VM grid (state colours), pool sizes, invoke button with timing, cold vs warm latency chart, density counter | **React + Vite + TypeScript**, **Recharts**, Tailwind; built to static files served by `beevmd` at `/ui` (or standalone) |
| 16 | **Grafana stack** *(optional module)* | Pre-built dashboard JSON for latency/density/pool metrics | **Prometheus + Grafana** in Docker Compose (profile `monitoring`) |
| 17 | **Benchmarks** | Cold boot vs snapshot restore vs warm hit; throughput; max VMs per node; isolation attack tests | Go bench tool (`cmd/beevm-bench`), **k6** for HTTP load, Python + **matplotlib** for charts |
| 18 | **Infra (cloud run)** | Spin up benchmark node reproducibly | **Terraform** (GCP N2 with nested virt, or AWS `c5.metal`/`i3en.metal`) + cloud-init setup script |

**Swappable dashboards rule:** the web dashboard and Grafana talk to `beevmd` *only* through the public REST/SSE API and `/metrics`. No core code imports either. Deleting `web/` or `deploy/monitoring/` needs no other change (Makefile targets and Compose profiles are separate).

---

## Invocation flow (cold → warm)

1. `POST /invoke/hello` with tenant key → router authenticates, looks up `hello` in registry.
2. `pool.Acquire("hello")`:
   - **Warm hit** (idle VM exists): take it (~1 ms).
   - **Snapshot restore** (no idle VM): create netns + cgroup + jailer dir → `firecracker` loads the **function snapshot** from NVMe (memory mmap'd, pages loaded lazily) → resume (target **5–20 ms**, well under 50 ms).
   - **First ever call / no snapshot**: restore **runtime snapshot**, push code over vsock, agent imports it, then take a **function snapshot** for next time.
3. Router sends payload to agent over vsock → agent runs handler → returns JSON.
4. `pool.Release(vm)`: back to idle pool with keep-alive TTL, or destroyed if pool is full / tenant limit hit.
5. Metrics recorded (`beevm_invoke_seconds{path="warm|restore|boot"}`), event pushed to SSE.

---

## Repository layout (`BeeVM/`)

```
cmd/beevmd/            daemon entrypoint
cmd/beevm/             CLI
cmd/beevm-agent/       guest agent (built static, baked into rootfs)
cmd/beevm-bench/       benchmark driver
internal/api/          HTTP handlers, auth, SSE
internal/registry/     SQLite function + tenant store
internal/store/        S3/MinIO artifact client
internal/pool/         scheduler, warm pools, VM state machine
internal/snapshot/     snapshot create/restore/backup
internal/vmm/          firecracker-go-sdk wrapper
internal/netns/        netns, TAP, veth, nftables
internal/isolation/    jailer config, cgroups v2
internal/agentproto/   shared request/response types (host ↔ agent)
internal/metrics/      Prometheus collectors
guest/kernel/          kernel fetch script + config notes
guest/rootfs/          Dockerfiles: base, python, node + build-rootfs.sh
guest/shims/           python & node handler shims
examples/functions/    hello-python, hello-node, hello-go-binary, cpu-heavy, memory-hog
web/                   (optional) React dashboard
deploy/monitoring/     (optional) prometheus.yml, grafana dashboards
deploy/docker-compose.yml   MinIO (+ monitoring profile)
bench/                 k6 scripts, plot_results.py, results/
infra/terraform/       cloud benchmark node
scripts/setup-wsl.sh   WSL2 + KVM + Firecracker install
docs/                  architecture.md, demo-script.md, report notes
Makefile
```

---

## Environment

- **Dev:** Windows 11 → WSL2 **Ubuntu 24.04**, `nestedVirtualization=true` in `%UserProfile%\.wslconfig`, `/dev/kvm` accessible, Docker (for MinIO, Grafana, rootfs builds), Go 1.23+, Node 20 (dashboard build).
- **Benchmark:** Terraform-created Linux node (GCP N2 nested-virt for cheap runs; optional short `i3en.metal` run to match the table), AWS S3 bucket instead of MinIO.

---

## Build milestones

1. **Foundations** — setup script, kernel + base rootfs, boot one MicroVM by hand, then from Go (`internal/vmm`).
2. **Agent + invoke** — `beevm-agent` over vsock, binary runtime, `POST /invoke` end-to-end (boot path).
3. **Registry + store** — SQLite registry, MinIO uploads, `beevm deploy/invoke/ls`.
4. **Snapshots** — runtime snapshots, then function snapshots; per-VM netns so restores don't clash.
5. **Warm pool** — pool manager, keep-alive, prewarm, limits.
6. **Python + Node runtimes** — rootfs images and shims.
7. **Isolation** — jailer, cgroups v2 limits, nftables tenant egress rules.
8. **Observability + UIs** — Prometheus metrics, Grafana dashboard, React dashboard.
9. **Benchmarks + cloud run** — latency, density, isolation tests; charts; report.

---

## Verification

- `go test ./...` for unit tests (pool state machine, registry, auth); integration tests tagged `//go:build kvm` that need `/dev/kvm`.
- **Functional:** deploy and invoke `hello-python`, `hello-node`, `hello-go-binary` via CLI and curl; check correct outputs.
- **Cold start:** `beevm bench coldstart` → report p50/p99 for boot vs snapshot restore vs warm; pass if restore p99 < 50 ms on the benchmark node.
- **Isolation tests** (examples in `examples/functions/`): fork bomb stopped by `pids.max`; memory hog killed by `memory.max` without affecting neighbours; CPU hog throttled by `cpu.max`; tenant A cannot reach tenant B's VM over the network; guest cannot see host filesystem (jailer chroot).
- **Density:** start idle 128 MB VMs until memory limit; report max VMs/node and memory per VM (snapshot pages shared via mmap).
- **UI check:** dashboard shows VMs changing state live during a k6 load run; Grafana panels match.
- **Swappability check:** delete `web/` (or `deploy/monitoring/`) on a branch → `make build` and all tests still pass.
