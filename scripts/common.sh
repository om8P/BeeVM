# Shared settings for BeeVM scripts. Source this file; do not run it.

# Where big files live (kernel, rootfs, run folders). Must be on the WSL Linux disk.
export BEEVM_HOME="${BEEVM_HOME:-$HOME/.beevm}"

# Pinned versions (checked 2026-10-22).
export FC_VERSION="1.17.0"
export FC_SHA256="06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558"
export GO_VERSION="1.27.3"
export GO_SHA256="65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"
export ALPINE_VERSION="3.24.3"
export ALPINE_SHA256="f8e1cafba9b1b283b5d30d06f9885c8d2bc161ffe9083ade8e2e157813a53407"
# Kernel from Firecracker's CI bucket (kernels do not depend on the Firecracker version).
export KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
export KERNEL_SHA256="e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"

export IMAGES_DIR="$BEEVM_HOME/images"
export BIN_DIR="$BEEVM_HOME/bin"
export CACHE_DIR="$BEEVM_HOME/cache"
export RUN_DIR="$BEEVM_HOME/run"

log()  { printf '\033[1;34m[beevm]\033[0m %s\n' "$*"; }
skip() { printf '\033[1;32m[beevm]\033[0m already done: %s\n' "$*"; }
die()  { printf '\033[1;31m[beevm] error:\033[0m %s\n' "$*" >&2; exit 1; }

# download URL DEST SHA256  (skips if DEST exists and matches)
download() {
  local url="$1" dest="$2" sha="$3"
  if [ -f "$dest" ] && { [ -z "$sha" ] || echo "$sha  $dest" | sha256sum -c --quiet - 2>/dev/null; }; then
    skip "$(basename "$dest")"; return 0
  fi
  mkdir -p "$(dirname "$dest")"
  log "downloading $(basename "$dest")"
  curl -fL --retry 3 --progress-bar -o "$dest.part" "$url"
  mv "$dest.part" "$dest"
  if [ -n "$sha" ]; then
    echo "$sha  $dest" | sha256sum -c --quiet - || { rm -f "$dest"; die "checksum mismatch for $dest"; }
  fi
}
