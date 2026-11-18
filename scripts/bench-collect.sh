#!/usr/bin/env bash
# Copies the newest sudo-run results into a benchmark folder, so the report includes them:
#   bench/results/isolation-*.md                 (make test-isolation)  -> <dir>/isolation.md
#   bench/results/m12/jailer/density-jailer-*    (make bench-jailer)    -> <dir>/
# Usage: scripts/bench-collect.sh <run folder>   Missing files are reported, not an error.
set -euo pipefail
cd "$(dirname "$0")/.."
dir="${1:?usage: bench-collect.sh <run folder>}"
[ -d "$dir" ] || { echo "error: not a folder: $dir" >&2; exit 1; }

iso=$(ls -t bench/results/isolation-*.md 2>/dev/null | head -1 || true)
if [ -n "$iso" ]; then
  cp "$iso" "$dir/isolation.md"
  echo "isolation table: $iso"
else
  echo "note: no isolation table; run make test-isolation (sudo)"
fi

jd=$(ls -t bench/results/m12/jailer/density-jailer-*.csv 2>/dev/null | head -1 || true)
if [ -n "$jd" ]; then
  cp "$jd" "${jd%.csv}.md" "$dir/" 2>/dev/null || cp "$jd" "$dir/"
  echo "jailer density: $jd"
else
  echo "note: no jailer density; run make bench-jailer (sudo)"
fi
