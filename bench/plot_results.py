#!/usr/bin/env python3
"""Charts and report.md for one `make bench` run folder.

    python3 bench/plot_results.py bench/results/m12/<stamp>

Reads (all optional, missing parts are skipped and said so in the report):
  coldstart-<runtime>/coldstart-*.csv   beevmd bench coldstart (binary, python, node)
  k6/warm.csv, k6/mixed.csv             k6 --out csv
  density-<scenario>-*.csv              beevmd bench density
  isolation.md, env.txt, doctor.txt     copied in by scripts/bench.sh

Writes PNG charts and report.md into the same folder. Needs matplotlib, nothing else.
"""
import csv
import glob
import math
import os
import sys
from datetime import datetime

NL = chr(10)
MIB = 1024 * 1024

# Categorical slots from the dataviz reference palette (light surface), in fixed order.
BLUE, ORANGE, AQUA, YELLOW, MAGENTA, GREEN = "#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300"
SURFACE, INK, INK2, GRID = "#fcfcfb", "#0b0b0b", "#52514e", "#e1e0d9"
RUNTIME_COLOR = {"binary": BLUE, "python": ORANGE, "node": AQUA}
SCENARIO_COLOR = {"one-function": BLUE, "eight-functions": ORANGE, "jailer": AQUA}
TIERS = ["boot", "restore", "function", "warm"]
TIER_LABEL = {"boot": "boot", "restore": "restore (runtime snapshot)", "function": "function snapshot", "warm": "warm"}


def pct(values, p):
    """Nearest-rank percentile, the same rule as the Go benchmarks."""
    if not values:
        return float("nan")
    s = sorted(values)
    rank = max(1, math.ceil(p / 100.0 * len(s)))
    return s[min(rank, len(s)) - 1]


def read_csv(path):
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


def num(row, key, default=0.0):
    try:
        return float(row[key])
    except (KeyError, ValueError, TypeError):
        return default


# ---------- loading ----------

def load_coldstart(folder):
    """{runtime: {mode: [row dicts]}}"""
    out = {}
    for d in sorted(glob.glob(os.path.join(folder, "coldstart-*"))):
        if not os.path.isdir(d):
            continue
        runtime = os.path.basename(d)[len("coldstart-"):]
        files = sorted(glob.glob(os.path.join(d, "coldstart-*.csv")))
        if not files:
            continue
        modes = {}
        for row in read_csv(files[-1]):
            modes.setdefault(row["mode"], []).append(row)
        out[runtime] = modes
    return out


def load_density(folder):
    """{scenario: [row dicts]} (newest file per scenario)."""
    newest = {}
    for path in sorted(glob.glob(os.path.join(folder, "density-*.csv"))):
        rows = read_csv(path)
        if rows:
            newest[rows[0]["scenario"]] = rows
    return newest


def parse_ts(v):
    try:
        return float(v)
    except ValueError:
        return datetime.fromisoformat(v.replace("Z", "+00:00")).timestamp()


def load_k6(path):
    """Raw k6 csv rows reduced to [(time, metric, value, status)]."""
    out = []
    with open(path, newline="") as f:
        for row in csv.DictReader(f):
            out.append((parse_ts(row["timestamp"]), row["metric_name"], float(row["metric_value"]), row.get("status", "")))
    return out


def k6_levels(rows, levels=6):
    """Split the http_req_duration points of a ramping-vus run into equal time slices (one per VU level).

    Returns [{vus, calls_per_s, p50, p99, failed}], slice i = 2**i virtual users.
    """
    pts = [(t, v, s) for t, m, v, s in rows if m == "http_req_duration"]
    if not pts:
        return []
    t0 = min(t for t, _, _ in pts)
    t1 = max(t for t, _, _ in pts)
    width = max((t1 - t0) / levels, 1e-9)
    out = []
    for i in range(levels):
        lo, hi = t0 + i * width, t0 + (i + 1) * width
        sl = [(v, s) for t, v, s in pts if lo <= t < hi or (i == levels - 1 and t == t1)]
        ok = [v for v, s in sl if s in ("200", "")]
        out.append({
            "vus": 2 ** i,
            "calls_per_s": len(sl) / width,
            "p50": pct(ok, 50),
            "p99": pct(ok, 99),
            "failed": len(sl) - len(ok),
        })
    return out


def k6_by_start(rows):
    """{start kind: [ms]} from the custom beevm_call_ms_* trends."""
    out = {}
    for _, m, v, _ in rows:
        if m.startswith("beevm_call_ms_"):
            out.setdefault(m[len("beevm_call_ms_"):], []).append(v)
    return out


def k6_rate(rows):
    pts = [t for t, m, _, _ in rows if m == "http_req_duration"]
    if len(pts) < 2:
        return 0.0
    return len(pts) / max(max(pts) - min(pts), 1e-9)


# ---------- charts ----------

def style(plt):
    plt.rcParams.update({
        "figure.facecolor": SURFACE, "axes.facecolor": SURFACE, "savefig.facecolor": SURFACE,
        "text.color": INK, "axes.labelcolor": INK2, "xtick.color": INK2, "ytick.color": INK2,
        "axes.edgecolor": GRID, "axes.grid": True, "grid.color": GRID, "grid.linewidth": 0.8,
        "axes.spines.top": False, "axes.spines.right": False, "font.size": 10,
        "legend.frameon": False, "axes.axisbelow": True,
    })


def save(fig, folder, name):
    path = os.path.join(folder, name)
    fig.tight_layout()
    fig.savefig(path, dpi=200)
    return name


def chart_coldstart(plt, folder, data):
    """First response (what a caller waits): p50 bar, p99 tick, per tier and runtime."""
    fig, ax = plt.subplots(figsize=(8, 4.2))
    runtimes = [r for r in ("binary", "python", "node") if r in data]
    tiers = [t for t in TIERS if any(t in data[r] for r in runtimes)]
    width = 0.8 / max(len(runtimes), 1)
    for j, rt in enumerate(runtimes):
        for i, tier in enumerate(tiers):
            rows = data[rt].get(tier)
            if not rows:
                continue
            v = [num(r, "first_response_ms") for r in rows]
            x = i + (j - (len(runtimes) - 1) / 2) * width
            ax.bar(x, pct(v, 50), width * 0.9, color=RUNTIME_COLOR[rt], label=rt if i == 0 else None)
            ax.plot([x - width * 0.35, x + width * 0.35], [pct(v, 99)] * 2, color=INK, lw=2)
    ax.set_yscale("log")
    ax.set_xticks(range(len(tiers)))
    ax.set_xticklabels([TIER_LABEL[t] for t in tiers])
    ax.set_ylabel("first response, ms (log)")
    ax.set_title("What a caller waits for: first response (bar = p50, black tick = p99)", loc="left", fontsize=10)
    ax.legend(ncol=3, loc="upper right")
    return save(fig, folder, "coldstart.png")


def chart_cdf(plt, folder, data):
    """CDF of VM ready: the grading chart (target 50 ms)."""
    fig, ax = plt.subplots(figsize=(8, 4.2))
    styles = {"restore": "--", "function": "-"}
    for rt in ("binary", "python", "node"):
        for tier, ls in styles.items():
            rows = data.get(rt, {}).get(tier)
            if not rows:
                continue
            v = sorted(num(r, "ready_ms") for r in rows)
            ax.plot(v, [(i + 1) / len(v) for i in range(len(v))], ls, color=RUNTIME_COLOR[rt], lw=2,
                    label=rt + ", " + ("function snapshot" if tier == "function" else "runtime snapshot"))
    ax.axvline(50, color=INK, lw=1)
    ax.text(50.5, 0.04, "50 ms target", color=INK2, fontsize=9)
    ax.set_xlabel("VM ready after restore, ms (agent answered)")
    ax.set_ylabel("share of restores")
    ax.set_ylim(0, 1.02)
    ax.set_title("Cold start by snapshot restore: cumulative distribution", loc="left", fontsize=10)
    ax.legend(loc="lower right", fontsize=8)
    return save(fig, folder, "vm_ready_cdf.png")


def chart_breakdown(plt, folder, data):
    """Where the time of a restore goes (p50 per phase, runtime snapshot + upload + import)."""
    phases = [("spawn_ms", "spawn", BLUE), ("load_ms", "snapshot load", ORANGE), ("agent_ms", "agent ready", AQUA),
              ("upload_ms", "upload code", YELLOW), ("import_ms", "import", MAGENTA), ("invoke_ms", "call", GREEN)]
    runtimes = [r for r in ("binary", "python", "node") if "restore" in data.get(r, {})]
    if not runtimes:
        return None
    fig, ax = plt.subplots(figsize=(8, 3.6))
    for y, rt in enumerate(runtimes):
        left = 0.0
        for key, label, color in phases:
            v = pct([num(r, key) for r in data[rt]["restore"]], 50)
            if v and v > 0:
                ax.barh(y, v, left=left, color=color, edgecolor=SURFACE, linewidth=2, label=label if y == 0 else None)
                left += v
        ax.text(left + 1, y, "%.0f ms" % left, va="center", color=INK2, fontsize=9)
    ax.set_yticks(range(len(runtimes)))
    ax.set_yticklabels(runtimes)
    ax.invert_yaxis()
    ax.set_xlabel("ms (p50 of each phase)")
    ax.set_title("Restore from the runtime snapshot, then first call", loc="left", fontsize=10)
    ax.legend(ncol=3, loc="lower right", fontsize=8)
    ax.grid(axis="y", visible=False)
    return save(fig, folder, "restore_breakdown.png")


def chart_throughput(plt, folder, levels):
    if not levels:
        return None
    fig, (a, b) = plt.subplots(1, 2, figsize=(9, 3.8))
    x = [l["vus"] for l in levels]
    a.plot(x, [l["calls_per_s"] for l in levels], "-o", color=BLUE, lw=2, ms=6)
    a.set_xscale("log", base=2)
    a.set_xticks(x)
    a.set_xticklabels([str(v) for v in x])
    a.set_xlabel("concurrent callers (k6 virtual users)")
    a.set_ylabel("calls per second")
    a.set_title("Throughput on warm VMs", loc="left", fontsize=10)
    b.plot(x, [l["p50"] for l in levels], "-o", color=BLUE, lw=2, ms=6, label="p50")
    b.plot(x, [l["p99"] for l in levels], "-o", color=ORANGE, lw=2, ms=6, label="p99")
    b.set_xscale("log", base=2)
    b.set_xticks(x)
    b.set_xticklabels([str(v) for v in x])
    b.set_xlabel("concurrent callers (k6 virtual users)")
    b.set_ylabel("round trip, ms")
    b.set_title("Latency under load", loc="left", fontsize=10)
    b.legend()
    return save(fig, folder, "throughput.png")


def chart_density(plt, folder, density):
    if not density:
        return None
    fig, axes = plt.subplots(1, 3, figsize=(12, 3.8))
    a, b, c = axes
    for name, rows in sorted(density.items()):
        color = SCENARIO_COLOR.get(name, INK2)
        vms = [num(r, "vms") for r in rows]
        a.plot(vms, [num(r, "host_per_vm") * num(r, "vms") / MIB for r in rows], "-o", color=color, lw=2, ms=4, label=name)
        b.plot(vms, [num(r, "pss_per_vm") / MIB for r in rows], "-", color=color, lw=2, label=name + " PSS")
        b.plot(vms, [num(r, "host_per_vm") / MIB for r in rows], "--", color=color, lw=2, label=name + " host")
        c.plot(vms, [num(r, "probe_p99_ms") for r in rows], "-o", color=color, lw=2, ms=4, label=name)
    a.set_title("Host memory used by idle VMs", loc="left", fontsize=10)
    a.set_xlabel("idle VMs")
    a.set_ylabel("MiB")
    b.set_title("Memory per VM (solid PSS, dashed host)", loc="left", fontsize=10)
    b.set_xlabel("idle VMs")
    b.set_ylabel("MiB per VM")
    c.set_title("Warm call p99 with this many VMs", loc="left", fontsize=10)
    c.set_xlabel("idle VMs")
    c.set_ylabel("ms")
    a.legend(fontsize=8)
    return save(fig, folder, "density.png")


# ---------- report ----------

def md_table(head, rows):
    lines = ["| " + " | ".join(head) + " |", "|" + "|".join("---" for _ in head) + "|"]
    lines += ["| " + " | ".join(str(c) for c in r) + " |" for r in rows]
    return NL.join(lines)


def fmt(v, digits=1):
    return "n/a" if v is None or (isinstance(v, float) and math.isnan(v)) else ("%." + str(digits) + "f") % v


def coldstart_tables(data):
    rows = []
    for rt in ("binary", "python", "node"):
        for tier in TIERS:
            r = data.get(rt, {}).get(tier)
            if not r:
                continue
            ready = [num(x, "ready_ms") for x in r]
            first = [num(x, "first_response_ms") for x in r]
            rows.append([rt, TIER_LABEL[tier], len(r), fmt(pct(ready, 50)), fmt(pct(ready, 99)), fmt(pct(first, 50)), fmt(pct(first, 99))])
    return md_table(["runtime", "tier", "runs", "VM ready p50", "VM ready p99", "first response p50", "first response p99"], rows)


def grading(data):
    """Honest check of the 50 ms target on the restore tiers."""
    lines = []
    for rt in ("binary", "python", "node"):
        for tier in ("restore", "function"):
            r = data.get(rt, {}).get(tier)
            if not r:
                continue
            v = [num(x, "ready_ms") for x in r]
            p50, p99 = pct(v, 50), pct(v, 99)
            verdict = "p50 under 50 ms" if p50 < 50 else "p50 OVER 50 ms"
            verdict += ", p99 under 50 ms" if p99 < 50 else ", p99 OVER 50 ms"
            lines.append("- %s, %s: VM ready p50 %s ms, p99 %s ms (%d runs): %s" % (rt, TIER_LABEL[tier], fmt(p50), fmt(p99), len(v), verdict))
    return NL.join(lines)


def density_table(density):
    rows = []
    for name, r in sorted(density.items()):
        last = r[-1]
        rows.append([name, int(num(last, "vms")), fmt(num(last, "pss_per_vm") / MIB), fmt(num(last, "rss_per_vm") / MIB),
                     fmt(num(last, "host_per_vm") / MIB), fmt(num(last, "probe_p50_ms")), fmt(num(last, "probe_p99_ms"))])
    return md_table(["scenario", "idle VMs reached", "PSS MiB/VM", "RSS MiB/VM", "host MiB/VM", "warm p50 ms", "warm p99 ms"], rows)


def read_text(path):
    if os.path.exists(path):
        with open(path) as f:
            return f.read()
    return None


def build_report(folder, data, density, levels, mixed, charts):
    env = read_text(os.path.join(folder, "env.txt"))
    iso = read_text(os.path.join(folder, "isolation.md"))
    out = ["# BeeVM benchmark report", ""]
    if env:
        out += ["```text", env.rstrip(), "```", ""]
    out += ["Grading points: cold start under 50 ms, tenant isolation, MicroVM density per node.", ""]

    out += ["## 1. Cold start", ""]
    if data:
        out += [grading(data), "", coldstart_tables(data), ""]
        for c in ("vm_ready_cdf.png", "coldstart.png", "restore_breakdown.png"):
            if c in charts:
                out += ["![%s](%s)" % (c, c), ""]
    else:
        out += ["No cold start data in this folder.", ""]

    out += ["## 2. Throughput", ""]
    if levels:
        rows = [[l["vus"], fmt(l["calls_per_s"], 0), fmt(l["p50"]), fmt(l["p99"]), l["failed"]] for l in levels]
        out += [md_table(["virtual users", "calls/s", "p50 ms", "p99 ms", "failed"], rows), ""]
        if "throughput.png" in charts:
            out += ["![throughput.png](throughput.png)", ""]
    else:
        out += ["No k6 warm run in this folder.", ""]
    if mixed:
        rows = [[k, len(v), fmt(pct(v, 50)), fmt(pct(v, 99))] for k, v in sorted(mixed["by_start"].items())]
        out += ["Mixed traffic (hello, hello-python, hello-node, open loop): %s calls/s overall." % fmt(mixed["rate"], 0), "",
                md_table(["start kind", "calls", "p50 ms", "p99 ms"], rows), ""]

    out += ["## 3. Density", ""]
    if density:
        out += [density_table(density), ""]
        if "density.png" in charts:
            out += ["![density.png](density.png)", ""]
        out += ["PSS counts shared pages once, so it is the fair per-VM share; host is the real drop of the node's available memory per VM.", ""]
    else:
        out += ["No density data in this folder.", ""]

    out += ["## 4. Isolation", ""]
    if iso:
        body = [l for l in iso.splitlines() if not l.startswith("# ")]
        out += body + [""]
    else:
        out += ["No isolation table in this folder. Run `make test-isolation` (sudo), then `make bench-report DIR=" + folder + "`.", ""]
    return NL.join(out)


def main(argv):
    if len(argv) != 2:
        print("usage: plot_results.py <run folder>", file=sys.stderr)
        return 2
    folder = argv[1]
    if not os.path.isdir(folder):
        print("error: not a folder: " + folder, file=sys.stderr)
        return 2
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    style(plt)

    data = load_coldstart(folder)
    density = load_density(folder)
    levels, mixed = [], None
    warm = os.path.join(folder, "k6", "warm.csv")
    if os.path.exists(warm):
        levels = k6_levels(load_k6(warm))
    mix = os.path.join(folder, "k6", "mixed.csv")
    if os.path.exists(mix):
        rows = load_k6(mix)
        mixed = {"by_start": k6_by_start(rows), "rate": k6_rate(rows)}

    charts = []
    if data:
        charts += [chart_cdf(plt, folder, data), chart_coldstart(plt, folder, data), chart_breakdown(plt, folder, data)]
    charts += [chart_throughput(plt, folder, levels), chart_density(plt, folder, density)]
    charts = [c for c in charts if c]
    report = build_report(folder, data, density, levels, mixed, charts)
    with open(os.path.join(folder, "report.md"), "w") as f:
        f.write(report + NL)
    print("wrote %s and %d charts" % (os.path.join(folder, "report.md"), len(charts)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
