"""Design diagrams for the report (matplotlib only): request flow with the three start paths,
the warm-pool VM state machine, and the isolation layers around one function."""
import sys
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import FancyBboxPatch, FancyArrowPatch

OUT = sys.argv[1]
BLUE, ORANGE, AQUA, YELLOW, GREY = "#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#898781"
INK, INK2 = "#0b0b0b", "#52514e"
plt.rcParams.update({"font.family": "DejaVu Sans", "font.size": 10})


def box(ax, x, y, w, h, text, edge=INK2, fill="#f4f6f8", bold=False, size=9.5):
    ax.add_patch(FancyBboxPatch((x, y), w, h, boxstyle="round,pad=0.02,rounding_size=0.08",
                                linewidth=1.4, edgecolor=edge, facecolor=fill))
    ax.text(x + w / 2, y + h / 2, text, ha="center", va="center", fontsize=size, color=INK,
            fontweight="bold" if bold else "normal")


def arrow(ax, x1, y1, x2, y2, color=INK2, text=None, dy=0.12, rad=0.0):
    ax.add_patch(FancyArrowPatch((x1, y1), (x2, y2), arrowstyle="-|>", mutation_scale=13,
                                 linewidth=1.4, color=color, connectionstyle="arc3,rad=%s" % rad))
    if text:
        ax.text((x1 + x2) / 2, (y1 + y2) / 2 + dy, text, ha="center", va="bottom", fontsize=8.5, color=color)


def flow():
    fig, ax = plt.subplots(figsize=(11, 6.2))
    ax.set_xlim(0, 11); ax.set_ylim(0, 6.2); ax.axis("off")
    box(ax, 0.2, 4.9, 1.9, 0.9, "Client\nbeevm CLI / curl / k6", bold=True)
    box(ax, 2.6, 4.9, 2.4, 0.9, "API gateway\nauth (tenant key) +\nregistry lookup", bold=True)
    box(ax, 5.6, 4.9, 2.3, 0.9, "Warm pool\nAcquire(tenant, fn,\ncode, size)", bold=True)
    arrow(ax, 2.1, 5.35, 2.6, 5.35)
    ax.text(1.15, 4.6, "POST /invoke/{fn}", ha="center", fontsize=8.5, color=INK2)
    arrow(ax, 5.0, 5.35, 5.6, 5.35)
    box(ax, 8.5, 5.0, 2.3, 0.75, "WARM: idle VM of this\nfunction exists", edge=AQUA, fill="#e8f7f1")
    box(ax, 8.5, 3.55, 2.3, 0.95, "RESTORE: load function\nsnapshot (code already\nimported) ~45 ms", edge=BLUE, fill="#eaf2fc")
    box(ax, 8.5, 2.1, 2.3, 0.95, "RESTORE: runtime snapshot\n+ upload + import\n(no function snapshot yet)", edge=BLUE, fill="#eaf2fc")
    box(ax, 8.5, 0.75, 2.3, 0.95, "BOOT: kernel boot\n(no snapshot / fallback)\n~950 ms", edge=ORANGE, fill="#fdeee8")
    arrow(ax, 7.9, 5.35, 8.5, 5.38, color=AQUA, text="hit")
    arrow(ax, 6.75, 4.9, 8.5, 4.0, color=BLUE, text="miss", dy=0.05)
    arrow(ax, 6.75, 4.9, 8.5, 2.6, color=BLUE)
    arrow(ax, 6.75, 4.9, 8.5, 1.2, color=ORANGE)
    box(ax, 2.6, 2.3, 4.6, 1.5, "Firecracker MicroVM (KVM)\nguest agent on vsock: /resume (clock + entropy),\n/package, /load, /invoke\n-> function runs as user fn, answer as JSON", edge=INK2)
    arrow(ax, 8.5, 3.0, 7.2, 3.05, color=INK2, text="VM ready", dy=-0.38)
    box(ax, 2.6, 0.5, 4.6, 1.1, "Response to client with timing headers\nX-BeeVM-Start, -Snapshot, -Boot-Ms, -Upload-Ms,\n-Load-Ms, -Function-Ms; VM goes back to Idle", edge=INK2)
    arrow(ax, 4.9, 2.3, 4.9, 1.6, color=INK2)
    arrow(ax, 2.6, 1.05, 1.15, 4.9, color=INK2, rad=-0.25)
    ax.text(0.25, 0.15, "Snapshot Manager builds runtime snapshots at server start and a function snapshot in the background after every deploy.",
            fontsize=8.5, color=INK2)
    fig.tight_layout(); fig.savefig(OUT + "/diag-flow.png", dpi=200); plt.close(fig)


def states():
    fig, ax = plt.subplots(figsize=(11, 3.6))
    ax.set_xlim(0, 11); ax.set_ylim(0, 3.6); ax.axis("off")
    xs = [0.3, 2.5, 4.7, 6.9, 9.1]
    names = [("Restoring", BLUE, "#eaf2fc"), ("Idle", AQUA, "#e8f7f1"), ("Busy", ORANGE, "#fdeee8"),
             ("Recycling", GREY, "#f1f1ef"), ("Destroyed", INK2, "#ffffff")]
    for x, (n, e, f) in zip(xs, names):
        box(ax, x, 1.4, 1.6, 0.8, n, edge=e, fill=f, bold=True, size=10.5)
    arrow(ax, 1.9, 1.8, 2.5, 1.8, text="ready")
    arrow(ax, 4.1, 1.95, 4.7, 1.95, text="Acquire", dy=0.08)
    arrow(ax, 4.7, 1.65, 4.1, 1.65, text="Release(healthy)", dy=-0.42)
    arrow(ax, 6.3, 1.8, 6.9, 1.8, text="failed / timeout", dy=-0.68)
    arrow(ax, 8.5, 1.8, 9.1, 1.8, text="killed")
    arrow(ax, 3.3, 2.2, 7.7, 2.2, color=GREY, rad=-0.35, text="keep-alive expired, LRU eviction, tenant quota, drain on redeploy", dy=1.0)
    arrow(ax, 1.1, 1.4, 7.7, 1.4, color=GREY, rad=0.3, text="restore failed", dy=-1.33)
    fig.tight_layout(); fig.savefig(OUT + "/diag-states.png", dpi=200); plt.close(fig)


def layers():
    fig, ax = plt.subplots(figsize=(11, 5.2))
    ax.set_xlim(0, 11); ax.set_ylim(0, 5.2); ax.axis("off")
    rings = [
        (0.2, 0.2, 10.6, 4.8, "Host (Linux / WSL2): nftables table 'ip beevm' - per-tenant address sets, VM->host input dropped, forwarding only on bv<n> links", "#f4f6f8", INK2),
        (0.6, 0.6, 9.8, 3.9, "Network namespace beevm-<slot>: own tap0 (guest 172.16.0.2) NATed to a unique clone address 10.200.x.y", "#eaf2fc", BLUE),
        (1.0, 1.0, 9.0, 3.0, "cgroup v2 beevm/t<tenant>/<vm>: memory.max = guest + 64 MiB, pids.max 32, cpu.max = vCPUs x --cpu-limit", "#fff6e0", YELLOW),
        (1.4, 1.4, 8.2, 2.1, "Firecracker jailer: chroot with only this VM's files, unique UID 900000+slot, seccomp filter, no capabilities", "#fdeee8", ORANGE),
        (1.8, 1.8, 7.4, 1.25, "MicroVM: own Linux 6.1 kernel and read-only rootfs (KVM hardware virtualization)", "#e8f7f1", AQUA),
    ]
    for x, y, w, h, t, f, e in rings:
        ax.add_patch(FancyBboxPatch((x, y), w, h, boxstyle="round,pad=0.02,rounding_size=0.12", linewidth=1.5, edgecolor=e, facecolor=f))
        ax.text(x + 0.15, y + h - 0.12, t, ha="left", va="top", fontsize=8.6, color=INK)
    box(ax, 3.2, 1.9, 4.6, 0.6, "function process: user fn (uid 1000), RLIMIT_NPROC 256,\nleftovers frozen and killed after every call", edge=INK2, fill="#ffffff", size=8.6)
    fig.tight_layout(); fig.savefig(OUT + "/diag-layers.png", dpi=200); plt.close(fig)


flow(); states(); layers()
print("diagrams written")
