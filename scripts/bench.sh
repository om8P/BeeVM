#!/usr/bin/env bash
# make bench: the M12 benchmark run. Produces one folder with raw data, charts and report.md:
#
#   1. cold start per runtime (boot, restore, function snapshot, warm): beevmd bench coldstart
#   2. throughput and mixed traffic through the HTTP API: k6 (bench/k6/)
#   3. density, one function (best case: all VMs share one snapshot's pages)
#   4. density, 8 different functions (worst case: nothing shared)
#   5. the newest isolation table and jailer density run, if `make test-isolation` /
#      `make bench-jailer` were run (both need sudo)
#   6. charts and report.md: bench/plot_results.py
#
# Settings (environment): BENCH_QUICK=1 demo run (about 5 minutes: 20 cold starts per mode, short k6 stages, up to 200 idle VMs), BENCH_RUNS cold starts
# per mode (default 100, quick 20), BENCH_ADDR (default 127.0.0.1:8080), BENCH_RESERVE memory kept
# free during the density runs (default 1GiB). Run in Legion Performance mode, plugged in.
set -euo pipefail
PY="${BEEVM_PYTHON:-/usr/bin/python3}"  # the system python has apt's matplotlib; conda's python3 may come first on PATH
cd "$(dirname "$0")/.."
. scripts/lib-server.sh

QUICK="${BENCH_QUICK:-0}"
if [ "$QUICK" = "1" ]; then RUNS="${BENCH_RUNS:-20}"; DMAX=200; DSTEP=20; else RUNS="${BENCH_RUNS:-100}"; DMAX=2000; DSTEP=20; fi
ADDR="${BENCH_ADDR:-127.0.0.1:8080}"
URL="http://$ADDR"
RESERVE="${BENCH_RESERVE:-1GiB}"
OUT="bench/results/m12/$(date +%Y%m%d-%H%M%S)"
REPO="$PWD"

command -v k6 >/dev/null 2>&1 || { echo "error: k6 not installed; run: make bench-setup" >&2; exit 1; }
"$PY" -c "import matplotlib" 2>/dev/null || { echo "error: python3-matplotlib not installed; run: make bench-setup" >&2; exit 1; }
mkdir -p "$OUT/k6"

say "building"
srv_build
srv_check_free "$ADDR"

say "checking the setup"
./bin/beevmd doctor --addr "$ADDR" | tee "$OUT/doctor.txt"
if grep -q '^FAIL' "$OUT/doctor.txt"; then echo "error: fix the FAIL lines first" >&2; exit 1; fi
if grep -qi 'battery' "$OUT/doctor.txt"; then
  printf '\033[1mwarning: on battery. Results will be about 2x slower; plug in and use Performance mode.\033[0m\n'
fi
{
  echo "date: $(date -Iseconds)"
  echo "kernel: $(uname -r)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //') x $(nproc)"
  echo "memory_mib: $(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)"
  echo "commit: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  echo "quick: $QUICK"
  echo "runs: $RUNS"
} > "$OUT/env.txt"

# --- 1. cold start (no server may run: the benchmark builds its own snapshots) ---
for spec in binary:bin/functions/hello-go-binary python:examples/functions/hello-python node:examples/functions/hello-node; do
  rt="${spec%%:*}"; code="${spec#*:}"
  say "cold start: $rt ($RUNS runs per mode)"
  ./bin/beevmd bench coldstart --runs "$RUNS" --mode boot,restore,function,warm --code "$code" --out "$OUT/coldstart-$rt"
done

# --- helpers for the server runs ---
deploy_three() {
  ./bin/beevm deploy hello bin/functions/hello-go-binary
  ./bin/beevm deploy hello-python examples/functions/hello-python
  ./bin/beevm deploy hello-node examples/functions/hello-node
}

# More open files and processes: hundreds of Firecracker processes with sockets each.
ulimit -n 65536 2>/dev/null || true
ulimit -u 32768 2>/dev/null || true

# --- 2. k6: throughput and mixed traffic ---
say "k6 throughput and mixed traffic"
srv_key bench
export BEEVM_URL="$URL" BEEVM_API_KEY="$KEY"
srv_start "$ADDR" --log-level warn --max-vms 128 --keep-alive 60s  # 32 callers over 3 functions need > 32 VMs; a tighter cap measures eviction churn, not warm calls
deploy_three
srv_wait_snapshots 3
(cd "$OUT/k6" && k6 run -q -e QUICK="$QUICK" -e FNS=hello,hello-python,hello-node -e BEEVM_URL="$URL" "$REPO/bench/k6/warm.js" --out csv=warm.csv)
(cd "$OUT/k6" && k6 run -q -e QUICK="$QUICK" -e FNS=hello,hello-python,hello-node -e BEEVM_URL="$URL" -e RATE=50 "$REPO/bench/k6/mixed.js" --out csv=mixed.csv)
srv_stop

# --- 3. density, one function ---
say "density: one function"
srv_start "$ADDR" --log-level warn --max-vms "$DMAX" --keep-alive 1h
./bin/beevm deploy hello bin/functions/hello-go-binary
srv_wait_snapshots 1
./bin/beevmd bench density --fn hello --step "$DSTEP" --max "$DMAX" --reserve "$RESERVE" --label one-function --out "$OUT"
srv_stop

# --- 4. density, 8 functions ---
say "density: 8 different functions"
srv_start "$ADDR" --log-level warn --max-vms "$DMAX" --keep-alive 1h
names=""
for i in 1 2 3 4 5 6 7 8; do
  ./bin/beevm deploy "hello-$i" bin/functions/hello-go-binary
  names="$names${names:+,}hello-$i"
done
srv_wait_snapshots 8
./bin/beevmd bench density --fn "$names" --step "$DSTEP" --max "$DMAX" --reserve "$RESERVE" --label eight-functions --out "$OUT"
srv_stop

# --- 5. isolation table and jailer density from earlier sudo runs ---
scripts/bench-collect.sh "$OUT"

# --- 6. charts and report ---
say "charts and report"
"$PY" bench/plot_results.py "$OUT"
echo
echo "Done: $OUT/report.md"
