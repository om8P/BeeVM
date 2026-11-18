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
. scripts/lib-server.sh

TENANT="${DEMO_TENANT:-demo}"
ADDR="${DEMO_ADDR:-127.0.0.1:8080}"
URL="http://$ADDR"

say "building"
srv_build

srv_check_free "$ADDR"

say "checking the setup"
if ! ./bin/beevmd doctor --addr "$ADDR"; then
  echo "error: fix the FAIL lines above first" >&2
  exit 1
fi

say "API key for tenant $TENANT"
srv_key "$TENANT"
echo "created a key for $TENANT"

say "starting the server on $ADDR (Ctrl-C stops it and every VM)"
# shellcheck disable=SC2086 # DEMO_SERVE_ARGS is meant to split into flags
srv_start "$ADDR" --log-format text ${DEMO_SERVE_ARGS:-}

./bin/beevm login --url "$URL" --key "$KEY"
export BEEVM_URL="$URL" BEEVM_API_KEY="$KEY"

say "deploying three functions"
./bin/beevm deploy hello bin/functions/hello-go-binary
./bin/beevm deploy hello-python examples/functions/hello-python
./bin/beevm deploy hello-node examples/functions/hello-node

say "calling each one (the first call of a function restores a VM; later calls are warm)"
# Wait for the three function snapshots (up to 30 s), then call.
srv_wait_snapshots 3
for fn in hello hello-python hello-node; do
  ./bin/beevm invoke "$fn" --payload '{"name":"demo"}' -v
done

bold=$(printf '\033[1m'); plain=$(printf '\033[0m')
cat <<MSG

${bold}BeeVM is running.${plain} In another terminal (the key is saved, nothing to export):

  ./bin/beevm invoke hello-python --payload '{"name":"you"}' -v
  ./bin/beevm vms
  ./bin/beevm init python my-fn && ./bin/beevm deploy my-fn my-fn

Load test (bench load reads the key from the environment):
  BEEVM_API_KEY=$KEY ./bin/beevmd bench load --fn hello,hello-python,hello-node --rate 20 --duration 20s

Dashboard: $URL/ui/   (paste the key above)

Press Ctrl-C here to stop.
MSG
wait "$SERVER"
