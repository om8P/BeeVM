#!/usr/bin/env bash
# Starts the full BeeVM stack for report screenshots and records real command output.
# Stop it with: touch report/assets/raw/STOP
set -uo pipefail
cd /mnt/c/Users/ommga/Desktop/ACC/BeeVM
export PATH=$PATH:/usr/local/go/bin
RAW=report/assets/raw
rm -f $RAW/*.txt $RAW/STOP
. scripts/lib-server.sh

redact() { sed -E 's/bvm_[A-Za-z0-9_-]+/bvm_****************/g'; }
# cap NAME CMD...: run CMD, save "$ CMD" + output (keys redacted) to $RAW/NAME.txt
cap() {
  local name="$1"; shift
  { printf '%s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "\$ $*"; "$@" 2>&1; } | redact > "$RAW/$name.txt"
}

srv_build
srv_check_free 127.0.0.1:8080
bash scripts/monitoring.sh run > $RAW/monitoring.log 2>&1 &
MON=$!
cap 01-doctor ./bin/beevmd doctor
srv_key demo
./bin/beevmd serve --log-format text --keep-alive 10s --max-vms 16 > $RAW/serve.log 2>&1 &
SERVER=$!
trap 'kill -INT $SERVER $MON 2>/dev/null; wait' EXIT
for i in $(seq 1 100); do curl -fsS 127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 0.1; done
./bin/beevm login --url http://127.0.0.1:8080 --key "$KEY" >/dev/null
export BEEVM_URL=http://127.0.0.1:8080 BEEVM_API_KEY="$KEY"
echo "$KEY" > $RAW/.key   # for the dashboard login only; deleted at the end

cap 02-whoami ./bin/beevm whoami
cap 03-deploy-binary ./bin/beevm deploy hello bin/functions/hello-go-binary
cap 04-deploy-python ./bin/beevm deploy hello-python examples/functions/hello-python
cap 05-deploy-node ./bin/beevm deploy hello-node examples/functions/hello-node
srv_wait_snapshots 3
cap 06-ls ./bin/beevm ls
cap 07-invoke-python-cold ./bin/beevm invoke hello-python --payload '{"name":"Om"}' -v
cap 08-invoke-python-warm ./bin/beevm invoke hello-python --payload '{"name":"Aditya"}' -v
cap 09-invoke-node ./bin/beevm invoke hello-node --payload '{"name":"ACC"}' -v
cap 10-invoke-binary ./bin/beevm invoke hello --payload '{"name":"BeeVM"}' -v
cap 11-prewarm ./bin/beevm prewarm hello -n 3
cap 12-vms ./bin/beevm vms
cap 13-snapshots ./bin/beevmd snapshot ls
cap 14-node bash -c 'curl -s -H "Authorization: Bearer $BEEVM_API_KEY" 127.0.0.1:8080/api/v1/node | python3 -m json.tool'
cap 15-metrics bash -c 'curl -s 127.0.0.1:8080/metrics | grep -E "^beevm_(invocations_total|vms\{|vm_memory_pss_bytes)" | head -20'
cap 16-load ./bin/beevmd bench load --fn hello,hello-python,hello-node --rate 20 --duration 20s
echo READY > $RAW/READY

# Keep traffic going for the screenshots: bursts (warm calls) and pauses (VMs reaped after the
# 10 s keep-alive, so the next burst restores from snapshots).
while [ ! -e $RAW/STOP ]; do
  ./bin/beevmd bench load --fn hello,hello-python,hello-node --rate 15 --duration 40s > /dev/null 2>&1
  [ -e $RAW/STOP ] && break
  sleep 15
done
tail -n 40 $RAW/serve.log | redact > $RAW/17-serve-log.txt
rm -f $RAW/.key $RAW/READY
echo stopped
