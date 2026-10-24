#!/usr/bin/env bash
# Build $BEEVM_HOME/images/rootfs.ext4 from the Alpine minirootfs, with our init script and the
# guest agent baked in. No sudo, no Docker.
# `fakeroot` makes files look root-owned; `mkfs.ext4 -d` fills a new filesystem from a folder.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
. "$ROOT/scripts/common.sh"

OUT="$IMAGES_DIR/rootfs.ext4"
SIZE_MB=64
TARBALL="$CACHE_DIR/alpine-minirootfs-$ALPINE_VERSION-x86_64.tar.gz"
BRANCH="v${ALPINE_VERSION%.*}"
AGENT_BIN="$CACHE_DIR/beevm-agent"
GO="${GO:-/usr/local/go/bin/go}"

command -v fakeroot >/dev/null  || die "fakeroot missing (run: make setup)"
command -v mkfs.ext4 >/dev/null || die "mkfs.ext4 missing (run: make setup)"
[ -x "$GO" ] || GO="$(command -v go || true)"
[ -n "$GO" ] || die "go not found (run: make setup)"

# Build the guest agent as a static binary (CGO_ENABLED=0): Alpine uses musl, not glibc, so a
# normal dynamically linked binary would not start. The old binary is kept when nothing
# changed, so its timestamp tells us whether the image is out of date.
mkdir -p "$CACHE_DIR"
log "building guest agent"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" build -trimpath -ldflags="-s -w" -o "$AGENT_BIN.new" ./cmd/beevm-agent)
if [ -f "$AGENT_BIN" ] && cmp -s "$AGENT_BIN" "$AGENT_BIN.new"; then
  rm -f "$AGENT_BIN.new"
else
  mv "$AGENT_BIN.new" "$AGENT_BIN"
fi

# Rebuild if missing or if init, this script or the agent is newer than the image.
if [ -f "$OUT" ] && [ "$OUT" -nt "$HERE/init" ] && [ "$OUT" -nt "$0" ] && [ "$OUT" -nt "$AGENT_BIN" ]; then
  skip "rootfs.ext4"; exit 0
fi

download "https://dl-cdn.alpinelinux.org/alpine/$BRANCH/releases/x86_64/$(basename "$TARBALL")" "$TARBALL" "$ALPINE_SHA256"

mkdir -p "$IMAGES_DIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "building rootfs.ext4 (${SIZE_MB} MB)"
rm -f "$OUT.part"
fakeroot sh -c "
  set -e
  mkdir '$WORK/root'
  tar -xzf '$TARBALL' -C '$WORK/root'
  install -m 0755 '$HERE/init' '$WORK/root/sbin/beevm-init'
  install -m 0755 '$AGENT_BIN' '$WORK/root/sbin/beevm-agent'
  mkfs.ext4 -q -F -d '$WORK/root' -L beevm-root '$OUT.part' ${SIZE_MB}M
"
mv "$OUT.part" "$OUT"
log "rootfs: $OUT ($(du -h "$OUT" | cut -f1) on disk)"
