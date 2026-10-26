#!/usr/bin/env bash
# Local S3 server for development: MinIO, built from pinned source (no Docker needed).
#   bash scripts/minio.sh install   build it if missing
#   bash scripts/minio.sh run       install if needed, then run in the foreground (Ctrl-C stops)
# Credentials and ports come from deploy/minio.env.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$here/common.sh"

GO="${GO:-/usr/local/go/bin/go}"
MINIO_BIN="$BIN_DIR/minio-$MINIO_GO_VERSION"

install_minio() {
  if [ -x "$MINIO_BIN" ]; then skip "minio $MINIO_GO_VERSION"; return 0; fi
  log "building MinIO $MINIO_GO_VERSION from source (a few minutes the first time)"
  mkdir -p "$BIN_DIR"
  tmp="$(mktemp -d)"
  (cd "$tmp" && GOBIN="$tmp" "$GO" install "github.com/minio/minio@$MINIO_GO_VERSION")
  mv "$tmp/minio" "$MINIO_BIN"
  rm -rf "$tmp"
}

case "${1:-run}" in
  install) install_minio ;;
  run)
    install_minio
    set -a; source "$here/../deploy/minio.env"; set +a
    mkdir -p "$BEEVM_HOME/minio"
    log "MinIO: S3 API on :9000, console on http://127.0.0.1:9001 (user $MINIO_ROOT_USER)"
    exec "$MINIO_BIN" server "$BEEVM_HOME/minio" --address ":9000" --console-address ":9001"
    ;;
  *) die "usage: minio.sh install|run" ;;
esac
