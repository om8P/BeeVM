# BeeVM progress log

Update this at the end of every working session: what got done, what's broken, what's next.
Details of each milestone: `docs/milestones.md`.

## Milestones

| # | Milestone | Status |
|---|-----------|--------|
| 1 | Environment and a hand-booted MicroVM | Not started |
| 2 | Boot and stop a VM from Go | Not started |
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
