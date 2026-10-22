#!/usr/bin/env bash
# Download the guest kernel (vmlinux 6.1) into $BEEVM_HOME/images. No sudo needed.
set -euo pipefail
. "$(dirname "$0")/../../scripts/common.sh"
download "$KERNEL_URL" "$IMAGES_DIR/vmlinux" "$KERNEL_SHA256"
log "kernel: $IMAGES_DIR/vmlinux ($(du -h "$IMAGES_DIR/vmlinux" | cut -f1))"
