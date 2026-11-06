#!/usr/bin/env bash
# Milestone 9, step 0: check that this machine can do what the isolation code will need.
# Run once as root:  sudo bash scripts/m9-preflight.sh
# It creates only throw-away things (names start with "beevm-pf") and removes them again.
# It does NOT change global settings: net.ipv4.ip_forward stays as it is.
set -u

[ "$(id -u)" -eq 0 ] || { echo "run as root: sudo bash $0"; exit 1; }

# Under sudo HOME is /root: find the real user's BeeVM folder.
if [ -z "${BEEVM_HOME:-}" ]; then
  home=$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)
  BEEVM_HOME="$home/.beevm"
fi
FC="$BEEVM_HOME/bin/firecracker"
JAILER="$BEEVM_HOME/bin/jailer"
JAIL_BASE="$BEEVM_HOME/jail-preflight"
UID_TEST=900000
CG=/sys/fs/cgroup/beevm-pf

pass=0; fail=0
ok()  { printf '\033[1;32mPASS\033[0m %s\n' "$*"; pass=$((pass+1)); }
bad() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; fail=$((fail+1)); }
info(){ printf '     %s\n' "$*"; }

cleanup() {
  ip netns del beevm-pf-a 2>/dev/null
  ip netns del beevm-pf-b 2>/dev/null
  ip link del bvpfa 2>/dev/null
  ip link del bvpfb 2>/dev/null
  rm -rf "$JAIL_BASE"
  for d in "$CG"/*/; do [ -d "$d" ] && rmdir "$d" 2>/dev/null; done
  rmdir "$CG" 2>/dev/null
}
trap cleanup EXIT
cleanup

echo "BEEVM_HOME=$BEEVM_HOME"
fwd_before=$(cat /proc/sys/net/ipv4/ip_forward)
info "net.ipv4.ip_forward before: $fwd_before"

# 1. Kernel options we rely on.
echo "== 1. kernel options"
cfg=$(zcat /proc/config.gz 2>/dev/null)
for opt in NF_TABLES NFT_NAT NFT_MASQ NFT_CT NF_CONNTRACK NF_NAT VETH TUN NET_NS CGROUP_PIDS MEMCG CFS_BANDWIDTH; do
  v=$(printf '%s\n' "$cfg" | grep -E "^CONFIG_${opt}=" | cut -d= -f2)
  if [ "$v" = y ] || [ "$v" = m ]; then ok "CONFIG_$opt=$v"; else bad "CONFIG_$opt missing"; fi
done
v=$(printf '%s\n' "$cfg" | grep -E '^CONFIG_NF_CT_NETLINK=' | cut -d= -f2)
info "CONFIG_NF_CT_NETLINK=${v:-missing} (only needed to flush conntrack; optional)"

# 2. cgroup v2 parent with cpu/memory/pids for the jailer.
echo "== 2. cgroups"
if mkdir -p "$CG" && echo "+cpu +memory +pids" > "$CG/cgroup.subtree_control"; then
  ok "created $CG, controllers: $(cat "$CG/cgroup.subtree_control")"
else
  bad "cannot create $CG or enable controllers"
fi

# 3. Run the jailer for real: chroot, mknod, cgroup leaf, drop to UID 900000, exec firecracker.
echo "== 3. jailer"
info "$("$JAILER" --version 2>&1 | head -1) / $("$FC" --version 2>&1 | head -1)"
mkdir -p "$JAIL_BASE"
out=$("$JAILER" --id pf1 --exec-file "$FC" --uid $UID_TEST --gid $UID_TEST \
      --chroot-base-dir "$JAIL_BASE" --cgroup-version 2 --parent-cgroup beevm-pf \
      --cgroup pids.max=32 --cgroup memory.max=201326592 --cgroup "cpu.max=100000 100000" \
      -- --version 2>&1); rc=$?
if [ $rc -eq 0 ] && printf '%s' "$out" | grep -q Firecracker; then
  ok "jailer ran firecracker --version inside the jail"
else
  bad "jailer exit $rc"; info "$out"
fi
root="$JAIL_BASE/firecracker/pf1/root"
if [ -c "$root/dev/kvm" ]; then ok "jail has /dev/kvm ($(stat -c '%U:%G' "$root/dev/kvm"))"; else bad "no /dev/kvm in jail"; fi
if [ -c "$root/dev/net/tun" ]; then ok "jail has /dev/net/tun"; else bad "no /dev/net/tun in jail"; fi
if [ -d "$CG/pf1" ]; then ok "jailer made cgroup leaf, pids.max=$(cat $CG/pf1/pids.max)"; else bad "no cgroup leaf $CG/pf1"; fi
# Hard link from images into the jail: same filesystem?
if ln "$BEEVM_HOME/images/vmlinux" "$root/vmlinux" 2>/dev/null; then ok "hard link images -> jail works"; else bad "hard link images -> jail failed"; fi

# 4. Network namespaces, TAP owned by the slot UID, veth pairs.
echo "== 4. netns, tap, veth"
ip netns add beevm-pf-a && ip netns add beevm-pf-b && ok "two named netns" || bad "ip netns add"
if ip -n beevm-pf-a tuntap add tap0 mode tap user $UID_TEST group $UID_TEST 2>&1; then
  ok "tap0 in netns, owner $UID_TEST (tun module: $(lsmod | grep -c '^tun ') loaded)"
else bad "tuntap add"; fi
ip link add bvpfa type veth peer name veth0 netns beevm-pf-a
ip link add bvpfb type veth peer name veth0 netns beevm-pf-b
ip addr add 10.250.0.1/30 dev bvpfa; ip link set bvpfa up
ip addr add 10.250.0.5/30 dev bvpfb; ip link set bvpfb up
ip -n beevm-pf-a addr add 10.250.0.2/30 dev veth0; ip -n beevm-pf-a link set veth0 up; ip -n beevm-pf-a link set lo up
ip -n beevm-pf-b addr add 10.250.0.6/30 dev veth0; ip -n beevm-pf-b link set veth0 up; ip -n beevm-pf-b link set lo up
ip -n beevm-pf-a route add default via 10.250.0.1
ip -n beevm-pf-b route add default via 10.250.0.5

# 5. The key question: VM-to-VM routing with forwarding only on the two veths.
echo "== 5. per-interface forwarding (global ip_forward=$fwd_before)"
if ip netns exec beevm-pf-a ping -c1 -W1 10.250.0.6 >/dev/null 2>&1; then
  info "ping a->b works BEFORE enabling (global forwarding is probably on)"
else
  ok "ping a->b blocked while forwarding is off"
fi
echo 1 > /proc/sys/net/ipv4/conf/bvpfa/forwarding
echo 1 > /proc/sys/net/ipv4/conf/bvpfb/forwarding
if ip netns exec beevm-pf-a ping -c2 -W1 10.250.0.6 >/dev/null 2>&1; then
  ok "ping a->b works with forwarding on bvpfa/bvpfb only"
else
  bad "ping a->b fails with per-interface forwarding (rp_filter all=$(cat /proc/sys/net/ipv4/conf/all/rp_filter))"
fi
fwd_after=$(cat /proc/sys/net/ipv4/ip_forward)
if [ "$fwd_after" = "$fwd_before" ]; then ok "global ip_forward unchanged ($fwd_after)"; else bad "global ip_forward changed to $fwd_after"; fi

# 6. nftables over netlink: we only check the kernel side (no nft binary needed).
echo "== 6. tools"
command -v nft >/dev/null && info "nft: $(nft --version)" || info "nft not installed (fine; code uses netlink)"
info "systemd-networkd: $(systemctl is-active systemd-networkd 2>/dev/null)"

echo
echo "passed $pass, failed $fail"
[ $fail -eq 0 ]
