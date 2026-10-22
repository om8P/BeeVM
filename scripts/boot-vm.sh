#!/usr/bin/env bash
# Boot one Firecracker MicroVM from a JSON config.
#   scripts/boot-vm.sh             interactive: you get a shell inside the VM
#   scripts/boot-vm.sh --selftest  boots, checks for BEEVM_BOOT_OK, prints boot time
set -euo pipefail
. "$(dirname "$0")/common.sh"

MODE="interactive"; [ "${1:-}" = "--selftest" ] && MODE="selftest"
FC="$BIN_DIR/firecracker"
[ -x "$FC" ] || die "firecracker not found at $FC (run: make setup)"
[ -f "$IMAGES_DIR/vmlinux" ] || die "kernel missing (run: make images)"
[ -f "$IMAGES_DIR/rootfs.ext4" ] || die "rootfs missing (run: make images)"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || die "no access to /dev/kvm. Are you in the 'kvm' group? (run: id; then wsl --shutdown from Windows and reopen)"

RUN="$RUN_DIR/$(date +%Y%m%d-%H%M%S)-$$"
mkdir -p "$RUN"
# Copy the rootfs so the clean base image is never modified.
cp --reflink=auto "$IMAGES_DIR/rootfs.ext4" "$RUN/rootfs.ext4"

ARGS="console=ttyS0 reboot=k panic=1 pci=off init=/sbin/beevm-init"
[ "$MODE" = "selftest" ] && ARGS="$ARGS beevm.mode=selftest"

cat > "$RUN/vm-config.json" <<JSON
{
  "boot-source": {
    "kernel_image_path": "$IMAGES_DIR/vmlinux",
    "boot_args": "$ARGS"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "$RUN/rootfs.ext4",
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": 128 }
}
JSON

log "run folder: $RUN"
if [ "$MODE" = "interactive" ]; then
  "$FC" --no-api --config-file "$RUN/vm-config.json"
  exit 0
fi

start=$(date +%s%N)
timeout 20 "$FC" --no-api --config-file "$RUN/vm-config.json" > "$RUN/console.log" 2>&1 < /dev/null || true
end=$(date +%s%N)
ms=$(( (end - start) / 1000000 ))
if grep -q BEEVM_BOOT_OK "$RUN/console.log"; then
  log "PASS: VM booted and exited cleanly in ${ms} ms (process start to exit)"
else
  tail -n 30 "$RUN/console.log" >&2
  die "FAIL: BEEVM_BOOT_OK not found. Full log: $RUN/console.log"
fi
