# Shared helpers for scripts that start their own `beevmd serve` (demo.sh, bench.sh).
# Source this file; it expects to run from the repository root.
#
#   srv_build                      build bin/beevmd, bin/beevm and the example functions
#   srv_check_free ADDR            fail if something already answers on ADDR
#   srv_key TENANT                 create the tenant (or a new key for it); sets KEY
#   srv_start ADDR [serve flags]   start `beevmd serve` in the background; sets SERVER; waits for /healthz
#                                  (SRV_PREFIX=sudo runs it as root, for --isolation jailer)
#   srv_stop                       stop it with Ctrl-C semantics and wait (also runs on exit)
#   srv_wait_snapshots N           wait up to 30 s for N current function snapshots

GO="${GO:-/usr/local/go/bin/go}"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

srv_build() {
  "$GO" build -o bin/beevmd ./cmd/beevmd
  "$GO" build -o bin/beevm ./cmd/beevm
  make -s examples >/dev/null
}

srv_check_free() {
  if curl -fsS "http://$1/healthz" >/dev/null 2>&1; then
    echo "error: something already answers on http://$1 (stop that server first, or pick another address)" >&2
    exit 1
  fi
}

srv_key() {
  local tenant="$1" out
  if ./bin/beevmd tenant ls | awk 'NR > 1 {print $1}' | grep -qx "$tenant"; then
    out=$(./bin/beevmd key add "$tenant")
  else
    out=$(./bin/beevmd tenant add "$tenant")
  fi
  KEY=$(printf '%s\n' "$out" | grep -oE 'bvm_[A-Za-z0-9_-]+' | head -1)
  if [ -z "$KEY" ]; then
    echo "error: could not read the new key from:" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
}

srv_start() {
  local addr="$1"; shift
  ${SRV_PREFIX:-} ./bin/beevmd serve --addr "$addr" "$@" &
  SERVER=$!
  trap srv_stop EXIT INT TERM
  local i
  for i in $(seq 1 100); do
    curl -fsS "http://$addr/healthz" >/dev/null 2>&1 && return 0
    if ! kill -0 "$SERVER" 2>/dev/null; then
      echo "error: the server stopped; see its log above" >&2
      exit 1
    fi
    sleep 0.1
  done
  echo "error: the server did not answer on http://$addr within 10 s" >&2
  exit 1
}

srv_stop() {
  if [ -n "${SERVER:-}" ]; then
    kill -INT "$SERVER" 2>/dev/null || true
    wait "$SERVER" 2>/dev/null || true
    SERVER=""
  fi
}

# Each deploy builds a function snapshot in the background (after the runtime snapshots, which
# take a few seconds on a fresh home). Wait for them so calls show the fast path.
srv_wait_snapshots() {
  local want="$1" i
  for i in $(seq 1 60); do
    [ "$(./bin/beevmd snapshot ls 2>/dev/null | grep -c '^fn-.*current$')" -ge "$want" ] && return 0
    sleep 0.5
  done
}
