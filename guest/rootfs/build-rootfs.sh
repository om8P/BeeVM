#!/usr/bin/env bash
# Build $BEEVM_HOME/images/rootfs.ext4 from the Alpine minirootfs. No sudo, no Docker.
# `fakeroot` makes files look root-owned; `mkfs.ext4 -d` fills a new filesystem from a folder.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../../scripts/common.sh"

OUT="$IMAGES_DIR/rootfs.ext4"
SIZE_MB=64
TARBALL="$CACHE_DIR/alpine-minirootfs-$ALPINE_VERSION-x86_64.tar.gz"
BRANCH="v${ALPINE_VERSION%.*}"

command -v fakeroot >/dev/null  || die "fakeroot missing (run: make setup)"
command -v mkfs.ext4 >/dev/null || die "mkfs.ext4 missing (run: make setup)"

# Rebuild if missing or if init is newer than the image.
if [ -f "$OUT" ] && [ "$OUT" -nt "$HERE/init" ] && [ "$OUT" -nt "$0" ]; then
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
  mkfs.ext4 -q -F -d '$WORK/root' -L beevm-root '$OUT.part' ${SIZE_MB}M
"
mv "$OUT.part" "$OUT"
log "rootfs: $OUT ($(du -h "$OUT" | cut -f1) on disk)"
