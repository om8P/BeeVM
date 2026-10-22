#!/usr/bin/env bash
# One-time setup inside WSL2 Ubuntu. Safe to re-run: finished steps are skipped.
# Needs sudo (you will be asked for your password).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
. "$ROOT/scripts/common.sh"
mkdir -p "$BIN_DIR" "$CACHE_DIR" "$IMAGES_DIR" "$RUN_DIR"

[ "$(uname -m)" = "x86_64" ] || die "this script supports x86_64 only"

# 1. Small tools
need=""
for t in jq fakeroot curl mkfs.ext4; do command -v "$t" >/dev/null || need=1; done
if [ -n "$need" ]; then
  log "installing packages (sudo)"
  sudo apt-get update -qq
  sudo apt-get install -y -qq jq fakeroot curl e2fsprogs
else skip "apt packages"; fi

# 2. KVM group
NEED_RELOGIN=""
if id -nG "$USER" | tr ' ' '\n' | grep -qx kvm; then
  skip "user is in kvm group"
elif getent group kvm | grep -qw "$USER"; then
  NEED_RELOGIN=1; log "you are in the kvm group but this shell has not picked it up yet"
else
  log "adding $USER to the kvm group (sudo)"
  sudo usermod -aG kvm "$USER"; NEED_RELOGIN=1
fi

# 3. Go
GO_BIN=/usr/local/go/bin/go
if [ -x "$GO_BIN" ] && "$GO_BIN" version | grep -q "go$GO_VERSION "; then
  skip "Go $GO_VERSION"
else
  download "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz" "$CACHE_DIR/go$GO_VERSION.tar.gz" "$GO_SHA256"
  log "installing Go $GO_VERSION to /usr/local/go (sudo)"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "$CACHE_DIR/go$GO_VERSION.tar.gz"
fi
if ! grep -q '/usr/local/go/bin' "$HOME/.profile"; then
  echo 'export PATH="$PATH:/usr/local/go/bin"' >> "$HOME/.profile"
  log "added Go to PATH in ~/.profile"
fi

# 4. Firecracker + jailer
if [ -x "$BIN_DIR/firecracker" ] && "$BIN_DIR/firecracker" --version | head -1 | grep -q "v$FC_VERSION"; then
  skip "Firecracker $FC_VERSION"
else
  TGZ="$CACHE_DIR/firecracker-v$FC_VERSION-x86_64.tgz"
  download "https://github.com/firecracker-microvm/firecracker/releases/download/v$FC_VERSION/firecracker-v$FC_VERSION-x86_64.tgz" "$TGZ" "$FC_SHA256"
  tmp="$(mktemp -d)"; tar -xzf "$TGZ" -C "$tmp"
  cp "$tmp/release-v$FC_VERSION-x86_64/firecracker-v$FC_VERSION-x86_64" "$BIN_DIR/firecracker"
  cp "$tmp/release-v$FC_VERSION-x86_64/jailer-v$FC_VERSION-x86_64" "$BIN_DIR/jailer"
  chmod +x "$BIN_DIR/firecracker" "$BIN_DIR/jailer"; rm -rf "$tmp"
fi

# 5. Kernel + rootfs
bash "$ROOT/guest/kernel/fetch-kernel.sh"
bash "$ROOT/guest/rootfs/build-rootfs.sh"

# 6. Summary
echo
log "firecracker : $("$BIN_DIR/firecracker" --version | head -1)"
log "go          : $(/usr/local/go/bin/go version)"
log "images      : $IMAGES_DIR"
if [ -n "$NEED_RELOGIN" ]; then
  echo
  log "ONE MORE STEP: run 'wsl --shutdown' in Windows PowerShell, then reopen the WSL terminal."
  log "(This makes the kvm group take effect.) Then run: make boot-test"
else
  log "all set. Try: make boot-test"
fi
