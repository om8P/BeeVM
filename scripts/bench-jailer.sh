#!/usr/bin/env bash
# make bench-jailer: the density run with --isolation jailer (every VM gets its own jail, cgroup,
# UID and network namespace). The server runs through sudo; everything else (tenant, deploys, the
# density command) runs as you. Result: bench/results/m12/jailer/density-jailer-<time>.{csv,md}.
# Run `make bench-report DIR=<run folder>` afterwards to add it to the charts.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/lib-server.sh

ADDR="${BENCH_ADDR:-127.0.0.1:8080}"
URL="http://$ADDR"
MAX="${BENCH_JAILER_MAX:-300}"
RESERVE="${BENCH_RESERVE:-1GiB}"
OUT=bench/results/m12/jailer

sudo -v
say "building"
srv_build
srv_check_free "$ADDR"
mkdir -p "$OUT"

srv_key bench
export BEEVM_URL="$URL" BEEVM_API_KEY="$KEY"
say "server with --isolation jailer"
ulimit -n 65536 2>/dev/null || true
SRV_PREFIX="sudo" srv_start "$ADDR" --log-level warn --isolation jailer --max-vms "$MAX" --net-prealloc 32 --keep-alive 1h
./bin/beevm deploy hello bin/functions/hello-go-binary
srv_wait_snapshots 1
./bin/beevmd bench density --fn hello --step 10 --max "$MAX" --reserve "$RESERVE" --label jailer --out "$OUT"
srv_stop
# Files written by the root server in the results folder belong to you again.
sudo chown -R "$(id -u):$(id -g)" bench/results 2>/dev/null || true
echo "Done: $OUT"
