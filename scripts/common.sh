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

# Runtime images (M8, checked 2026-11-05). apk.static is Alpine's package manager as one
# static binary; it verifies every package with the keys from the minirootfs above.
# Alpine keeps only the newest build per branch, so package versions are pinned by major
# version (`~`), and the installed versions are written to /etc/beevm/versions in the image.
# Python 3.14 + Node.js 24 are what Alpine 3.24 ships (3.12/20 are not available; Node 20 is
# end-of-life since 2026-04). Decided with the user on 2026-11-05.
export APK_STATIC_VERSION="3.0.9-r0"
export APK_STATIC_SHA256="0a1a0682375f66a83b7b903f6e4638ccfe312547d50d336b5f2be9fc391281be"
export PYTHON_PKGS="python3~3.14 python3-pycache-pyc0~3.14"
export NODE_PKGS="nodejs~24"

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
  # A stalled connection (under 1 KB/s for a minute) fails instead of hanging forever.
  curl -fL --retry 3 --connect-timeout 20 --speed-limit 1024 --speed-time 60 --progress-bar -o "$dest.part" "$url"
  mv "$dest.part" "$dest"
  if [ -n "$sha" ]; then
    echo "$sha  $dest" | sha256sum -c --quiet - || { rm -f "$dest"; die "checksum mismatch for $dest"; }
  fi
}

# MinIO (local S3 server for development). MinIO archived its project in 2026 and no longer
# serves binaries (410 Gone), so scripts/minio.sh builds this pinned source version with Go.
# The Go checksum database guarantees the source is what it was when we pinned it.
export MINIO_GO_VERSION="v0.0.0-20260212201848-7aac2a2c5b7c"

# Monitoring (M10, optional; checked 2026-11-06): Prometheus and Grafana as plain release
# tarballs, no Docker. Prometheus 3.16.0 was one day old, so the previous release is pinned.
# The Grafana tarball is ~480 MB (downloaded once into $BEEVM_HOME/cache).
export PROMETHEUS_VERSION="3.15.1"
export PROMETHEUS_SHA256="6a726885328ad9efebbf36e876ce2fb6225468a71fd1e7ce3ba666d5f4d97a84"
export GRAFANA_VERSION="13.3.0"
export GRAFANA_SHA256="cec8f98cc59bc1a1945833b652371a073be678c5c159a9b1695ad63c04fae5a7"
