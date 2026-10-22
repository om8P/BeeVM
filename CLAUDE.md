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
- Control plane in **Go** (1.23+), `firecracker-go-sdk`, `go-chi/chi`, SQLite via `modernc.org/sqlite`.
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
(Fill in as they come to exist.)
