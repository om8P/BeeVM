#!/usr/bin/env bash
# Optional module (M11): the web dashboard in web/ (React + Vite + TypeScript).
#   bash scripts/ui.sh install   download Node.js (pinned, checksum-verified) into $BEEVM_HOME
#   bash scripts/ui.sh build     install Node if needed, npm ci, type-check and build web/dist
#   bash scripts/ui.sh dev       Vite dev server on http://127.0.0.1:5173/ui/ (API calls go to :8080)
#   bash scripts/ui.sh test      unit tests of the dashboard (vitest)
# beevmd serve shows web/dist at http://127.0.0.1:8080/ui/. Deleting this script, web/ and the
# Makefile's ui targets removes the module; nothing else depends on it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$here/common.sh"

web="$(cd "$here/../web" && pwd)"
node_dir="$BEEVM_HOME/node-$HOST_NODE_VERSION"

install_node() {
  if [ -x "$node_dir/bin/node" ]; then
    skip "node $HOST_NODE_VERSION"
    return
  fi
  local name="node-v$HOST_NODE_VERSION-linux-x64"
  local tb="$CACHE_DIR/$name.tar.xz"
  download "https://nodejs.org/dist/v$HOST_NODE_VERSION/$name.tar.xz" "$tb" "$HOST_NODE_SHA256"
  rm -rf "$node_dir.part"
  mkdir -p "$node_dir.part"
  tar -xJf "$tb" -C "$node_dir.part" --strip-components=1
  mv "$node_dir.part" "$node_dir"
}

# npm_in_web ARGS: run npm in web/ with the pinned Node first on PATH.
npm_in_web() {
  (cd "$web" && PATH="$node_dir/bin:$PATH" npm "$@")
}

# deps: install exactly what package-lock.json lists (skipped when nothing changed).
deps() {
  if [ -f "$web/node_modules/.package-lock.json" ] && [ "$web/node_modules/.package-lock.json" -nt "$web/package-lock.json" ]; then
    skip "web/node_modules"
    return
  fi
  log "installing dashboard packages (npm ci)"
  npm_in_web ci --no-audit --no-fund
}

case "${1:-build}" in
  install) install_node ;;
  build)
    install_node; deps
    log "building web/dist"
    npm_in_web run build
    log "done: beevmd serve shows it at http://127.0.0.1:8080/ui/"
    ;;
  dev)
    install_node; deps
    # File change events do not cross from Windows into WSL for /mnt/c: poll instead.
    case "$web" in /mnt/*) export BEEVM_UI_POLL=1 ;; esac
    npm_in_web run dev
    ;;
  test) install_node; deps; npm_in_web test ;;
  *) die "usage: $0 install|build|dev|test" ;;
esac
