#!/usr/bin/env bash
# make demo: one command from a set-up machine to a server with three functions ready to call.
#
#   1. builds bin/beevmd, bin/beevm and the example functions, runs beevmd doctor
#   2. creates the tenant "demo" (or a new key for it, if it exists)
#   3. starts `beevmd serve` (text logs) in this terminal, saves the key with `beevm login`
#      so any other terminal can use the CLI right away
#   4. deploys hello (Go binary), hello-python and hello-node, and calls each once
#
# Ctrl-C stops the server and every VM. Settings: DEMO_TENANT (default demo), DEMO_ADDR
# (default 127.0.0.1:8080), DEMO_SERVE_ARGS (extra flags for serve, e.g. "--keep-alive 2s").
set -euo pipefail
cd "$(dirname "$0")/.."

TENANT="${DEMO_TENANT:-demo}"
ADDR="${DEMO_ADDR:-127.0.0.1:8080}"
URL="http://$ADDR"
GO="${GO:-/usr/local/go/bin/go}"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

say "building"
"$GO" build -o bin/beevmd ./cmd/beevmd
"$GO" build -o bin/beevm ./cmd/beevm
make -s examples >/dev/null

if curl -fsS "$URL/healthz" >/dev/null 2>&1; then
  echo "error: something already answers on $URL (stop that server first, or set DEMO_ADDR)" >&2
  exit 1
fi

say "checking the setup"
if ! ./bin/beevmd doctor --addr "$ADDR"; then
  echo "error: fix the FAIL lines above first" >&2
  exit 1
fi

say "API key for tenant $TENANT"
if ./bin/beevmd tenant ls | awk 'NR > 1 {print $1}' | grep -qx "$TENANT"; then
  out=$(./bin/beevmd key add "$TENANT")
else
  out=$(./bin/beevmd tenant add "$TENANT")
fi
KEY=$(printf '%s\n' "$out" | grep -oE 'bvm_[A-Za-z0-9_-]+' | head -1)
if [ -z "$KEY" ]; then
  echo "error: could not read the new key from:" >&2
  printf '%s\n' "$out" >&2
  exit 1
fi
echo "created a key for $TENANT"

say "starting the server on $ADDR (Ctrl-C stops it and every VM)"
# shellcheck disable=SC2086 # DEMO_SERVE_ARGS is meant to split into flags
./bin/beevmd serve --addr "$ADDR" --log-format text ${DEMO_SERVE_ARGS:-} &
SERVER=$!
trap 'kill -INT "$SERVER" 2>/dev/null; wait "$SERVER" 2>/dev/null' EXIT INT TERM

for _ in $(seq 1 100); do
  curl -fsS "$URL/healthz" >/dev/null 2>&1 && break
  if ! kill -0 "$SERVER" 2>/dev/null; then
    echo "error: the server stopped; see its log above" >&2
    exit 1
  fi
  sleep 0.1
done

./bin/beevm login --url "$URL" --key "$KEY"
export BEEVM_URL="$URL" BEEVM_API_KEY="$KEY"

say "deploying three functions"
./bin/beevm deploy hello bin/functions/hello-go-binary
./bin/beevm deploy hello-python examples/functions/hello-python
./bin/beevm deploy hello-node examples/functions/hello-node

say "calling each one (the first call of a function restores a VM; later calls are warm)"
# Each deploy builds a function snapshot in the background (after the server's runtime
# snapshots, which take a few seconds on a fresh home). Wait for them, so the calls below
# show the fast path; after 30 s call anyway.
for _ in $(seq 1 60); do
  [ "$(./bin/beevmd snapshot ls 2>/dev/null | grep -c '^fn-.*current$')" -ge 3 ] && break
  sleep 0.5
done
for fn in hello hello-python hello-node; do
  ./bin/beevm invoke "$fn" --payload '{"name":"demo"}' -v
done

bold=$(printf '\033[1m'); plain=$(printf '\033[0m')
cat <<EOF

${bold}BeeVM is running.${plain} In another terminal (the key is saved, nothing to export):

  ./bin/beevm invoke hello-python --payload '{"name":"you"}' -v
  ./bin/beevm vms
  ./bin/beevm init python my-fn && ./bin/beevm deploy my-fn my-fn

Load test (bench load reads the key from the environment):
  BEEVM_API_KEY=$KEY ./bin/beevmd bench load --fn hello,hello-python,hello-node --rate 20 --duration 20s

Dashboard: $URL/ui/   (paste the key above)

Press Ctrl-C here to stop.
EOF
wait "$SERVER"
