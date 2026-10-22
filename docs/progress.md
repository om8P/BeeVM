# BeeVM progress log

Update this at the end of every working session: what got done, what's broken, what's next.

## Milestones (from docs/architecture.md)

| # | Milestone | Status |
|---|-----------|--------|
| 1 | Foundations: setup script, kernel + base rootfs, boot a MicroVM by hand, then from Go | Not started |
| 2 | Agent + invoke: `beevm-agent` over vsock, binary runtime, `POST /invoke` end to end | Not started |
| 3 | Registry + store: SQLite registry, MinIO uploads, CLI deploy/invoke/ls | Not started |
| 4 | Snapshots: runtime and function snapshots, netns per VM | Not started |
| 5 | Warm pool: pool manager, keep-alive, prewarm, limits | Not started |
| 6 | Python + Node runtimes: rootfs images and shims | Not started |
| 7 | Isolation: jailer, cgroups v2, nftables tenant rules | Not started |
| 8 | Observability + UIs: Prometheus, Grafana, React dashboard | Not started |
| 9 | Benchmarks + cloud run: latency, density, isolation tests, report | Not started |

## Session log

### 2026-10-22 — Planning
- Chose case study 06; found the doc's group 06 details are mismatched (ignored).
- Wrote the architecture plan: `docs/architecture.md`.
- Made the architecture diagrams (box version and beehive version) as a Claude design canvas: https://claude.ai/artifact/QEcUUmM4Tc2DiPHHEhFqYo
- Checked the laptop: WSL2 Ubuntu 24.04 with `/dev/kvm` available.
- **Next:** milestone 1.
