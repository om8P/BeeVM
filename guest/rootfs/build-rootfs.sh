#!/usr/bin/env bash
# Build a guest root filesystem from the Alpine minirootfs, with our init script and the guest
# agent baked in. No sudo, no Docker.
#   build-rootfs.sh [binary]   -> $BEEVM_HOME/images/rootfs.ext4         (static binaries)
#   build-rootfs.sh python     -> $BEEVM_HOME/images/rootfs-python.ext4  (+ python3 + shim)
#   build-rootfs.sh node       -> $BEEVM_HOME/images/rootfs-node.ext4    (+ nodejs + shim)
# `fakeroot` makes files look root-owned; `mkfs.ext4 -d` fills a new filesystem from a folder.
# Runtime packages are installed with `apk.static` (Alpine's package manager as one static
# binary) into the unpacked folder: `apk --root DIR add ...` installs into DIR, not this system.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
SELF="$HERE/$(basename "$0")"
. "$ROOT/scripts/common.sh"

RUNTIME="${1:-binary}"
case "$RUNTIME" in
  binary) OUT="$IMAGES_DIR/rootfs.ext4";        SIZE_MB=64;  PKGS="";                      SHIM="" ;;
  python) OUT="$IMAGES_DIR/rootfs-python.ext4"; SIZE_MB=128; PKGS="$PYTHON_PKGS"; SHIM="$ROOT/guest/shims/python/shim.py" ;;
  node)   OUT="$IMAGES_DIR/rootfs-node.ext4";   SIZE_MB=160; PKGS="$NODE_PKGS";   SHIM="$ROOT/guest/shims/node/shim.js" ;;
  *) die "unknown runtime '$RUNTIME' (binary, python or node)" ;;
esac
NAME="$(basename "$OUT")"
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

# Rebuild if missing or if an input is newer than the image. Runtime images also depend on
# their shim and on the package pins in common.sh.
newer_inputs=("$HERE/init" "$SELF" "$AGENT_BIN")
[ -n "$SHIM" ] && newer_inputs+=("$SHIM" "$ROOT/scripts/common.sh")
up_to_date=1
[ -f "$OUT" ] || up_to_date=0
for f in "${newer_inputs[@]}"; do [ "$up_to_date" = 1 ] && [ "$OUT" -nt "$f" ] || up_to_date=0; done
if [ "$up_to_date" = 1 ]; then skip "$NAME"; exit 0; fi

download "https://dl-cdn.alpinelinux.org/alpine/$BRANCH/releases/x86_64/$(basename "$TARBALL")" "$TARBALL" "$ALPINE_SHA256"

APK=""
if [ -n "$PKGS" ]; then
  # Alpine keeps only the newest build of a package in a branch, so an old pinned version
  # disappears from the mirror. If this download fails with 404, look up the current
  # version in the branch's APKINDEX and update APK_STATIC_VERSION/SHA256 in common.sh.
  APK_PKG="$CACHE_DIR/apk-tools-static-$APK_STATIC_VERSION.apk"
  download "https://dl-cdn.alpinelinux.org/alpine/$BRANCH/main/x86_64/apk-tools-static-$APK_STATIC_VERSION.apk" \
    "$APK_PKG" "$APK_STATIC_SHA256"
  APK_DIR="$CACHE_DIR/apk-tools-static-$APK_STATIC_VERSION"
  if [ ! -x "$APK_DIR/sbin/apk.static" ]; then
    mkdir -p "$APK_DIR"
    tar -xzf "$APK_PKG" -C "$APK_DIR" sbin/apk.static 2>/dev/null || true
    [ -x "$APK_DIR/sbin/apk.static" ] || die "apk.static not found in $APK_PKG"
  fi
  APK="$APK_DIR/sbin/apk.static"
fi

mkdir -p "$IMAGES_DIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

log "building $NAME (${SIZE_MB} MB)"
rm -f "$OUT.part"
# apk.static is a static binary, so fakeroot's library trick does not reach it: the files it
# writes belong to us, not to (fake) root. The `chown -R 0:0` afterwards fixes that inside
# fakeroot, and mkfs.ext4 then records root as the owner. --no-scripts: package install
# scripts would need a real chroot (root); python3 and nodejs do not need them.
fakeroot sh -c "
  set -e
  mkdir '$WORK/root'
  tar -xzf '$TARBALL' -C '$WORK/root'
  if [ -n '$PKGS' ]; then
    '$APK' --root '$WORK/root' --keys-dir '$WORK/root/etc/apk/keys' \
      --repository 'https://dl-cdn.alpinelinux.org/alpine/$BRANCH/main' \
      --no-scripts --no-cache --no-interactive --quiet add $PKGS
    chown -R 0:0 '$WORK/root'
    mkdir -p '$WORK/root/usr/lib/beevm' '$WORK/root/etc/beevm'
    install -m 0644 '$SHIM' '$WORK/root/usr/lib/beevm/'
    echo '$RUNTIME' > '$WORK/root/etc/beevm/runtime'
    # Installed package versions, for 'what exactly is in this image?'
    awk '/^P:/{p=substr(\$0,3)} /^V:/{print p, substr(\$0,3)}' '$WORK/root/lib/apk/db/installed' \
      > '$WORK/root/etc/beevm/versions'
  fi
  install -m 0755 '$HERE/init' '$WORK/root/sbin/beevm-init'
  install -m 0755 '$AGENT_BIN' '$WORK/root/sbin/beevm-agent'
  mkfs.ext4 -q -F -d '$WORK/root' -L beevm-root '$OUT.part' ${SIZE_MB}M
"
mv "$OUT.part" "$OUT"
log "$NAME: $OUT ($(du -h "$OUT" | cut -f1) on disk)"
if [ -n "$PKGS" ]; then
  # debugfs reads a file out of the image without mounting it (no root needed).
  debugfs -R 'cat /etc/beevm/versions' "$OUT" 2>/dev/null | grep -E '^(python3|nodejs) ' | sed 's/^/[beevm] installed: /' || true
fi
