#!/usr/bin/env bash
# Boot the Kata guest under Firecracker on KVM and wait for kata-agent on vsock 1024.
# Usage: boot-kata-guest.sh [vm-name] [container-rootfs-image]
# Guest image is read-only virtio-pmem with DAX, as in Substrate's Cloud Hypervisor setup.
# A container rootfs image, if given, is copied per VM and attached as virtio-blk (/dev/vda).
set -euo pipefail
DIR=/opt/fc-poc
NAME=${1:-vm1}
CTR_IMAGE=${2:-}
RUN=$DIR/run/$NAME
if [ -f "$RUN/fc.pid" ]; then kill "$(cat "$RUN/fc.pid")" 2>/dev/null || true; sleep 0.5; fi
rm -rf "$RUN" && mkdir -p "$RUN"

DRIVES="[]"
if [ -n "$CTR_IMAGE" ]; then
  cp --sparse=always "$CTR_IMAGE" "$RUN/ctr0.ext4"
  DRIVES="[ { \"drive_id\": \"ctr0\", \"path_on_host\": \"$RUN/ctr0.ext4\", \"is_root_device\": false, \"is_read_only\": false } ]"
fi

BOOT_ARGS="console=ttyS0 reboot=k panic=1 no_timer_check noreplace-smp clocksource=kvm-clock \
root=/dev/pmem0p1 rootflags=dax,data=ordered,errors=remount-ro ro rootfstype=ext4 \
cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1 \
systemd.unit=kata-containers.target systemd.mask=systemd-networkd.service systemd.mask=systemd-networkd.socket \
agent.log=debug agent.debug_console agent.debug_console_vport=1026"

cat > "$RUN/config.json" <<EOF
{
  "boot-source": { "kernel_image_path": "$DIR/vmlinux", "boot_args": "$BOOT_ARGS" },
  "machine-config": { "vcpu_count": 2, "mem_size_mib": 2048 },
  "drives": $DRIVES,
  "pmem": [ { "id": "rootfs", "path_on_host": "$DIR/rootfs.img", "root_device": false, "read_only": true } ],
  "vsock": { "guest_cid": 3, "uds_path": "$RUN/kata.hvsock" },
  "entropy": {},
  "logger": { "log_path": "$RUN/fc.log", "level": "Info" }
}
EOF
touch "$RUN/fc.log"

T0=$(date +%s.%N)
"$DIR/firecracker" --api-sock "$RUN/api.sock" --config-file "$RUN/config.json" > "$RUN/console.log" 2>&1 &
echo $! > "$RUN/fc.pid"

for i in $(seq 1 300); do
  if [ -S "$RUN/kata.hvsock" ]; then
    reply=$(printf 'CONNECT 1024\n' | timeout 1 socat -t1 - UNIX-CONNECT:"$RUN/kata.hvsock" 2>/dev/null | head -1 || true)
    if [[ "$reply" == OK* ]]; then
      echo "kata-agent reachable on vsock 1024 after $(echo "$(date +%s.%N) - $T0" | bc) s"
      exit 0
    fi
  fi
  kill -0 "$(cat "$RUN/fc.pid")" 2>/dev/null || { echo "firecracker exited"; tail -n 20 "$RUN/console.log"; exit 1; }
  sleep 0.1
done
echo "kata-agent not reachable after 30 s"; tail -n 20 "$RUN/console.log"; exit 1
