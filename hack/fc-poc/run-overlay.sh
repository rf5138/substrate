#!/usr/bin/env bash
# Step 1d: two VMs share one read-only image; each writes to its own disk-backed upper.
# Afterwards the host reads each upper back, as a ROOTFS checkpoint would.
set -euo pipefail
DIR=/opt/fc-poc
HERE=$(dirname "$0")
LOWER=$DIR/alpine.ext4
SUM_BEFORE=$(md5sum "$LOWER" | cut -d' ' -f1)

for vm in vm-a vm-b; do
  LOWER_IMAGE=$LOWER "$HERE/boot-kata-guest.sh" "$vm" &
done
wait

rc=0
for vm in vm-a vm-b; do
  echo "=== $vm"
  "$DIR/fcpoc" -vsock "$DIR/run/$vm/kata.hvsock" -dev /dev/vda -upper-dev /dev/vdb \
    -cmd "mount | grep ' / '; echo $vm > /probe; cat /probe; rm /etc/motd; ls /etc/motd 2>&1; echo lower-is-shared: \$(head -1 /etc/alpine-release)" \
    || rc=1
done

for vm in vm-a vm-b; do kill "$(cat "$DIR/run/$vm/fc.pid")" 2>/dev/null || true; done
sleep 1

echo "=== host view of each upper (what a ROOTFS checkpoint would read)"
for vm in vm-a vm-b; do
  m=$(mktemp -d)
  # noload: the guest was killed without unmounting, so skip journal replay on this read-only look.
  mount -o loop,ro,noload "$DIR/run/$vm/upper.ext4" "$m"
  echo "--- $vm upper: $(du -h --apparent-size "$DIR/run/$vm/upper.ext4" | cut -f1) apparent, $(du -h "$DIR/run/$vm/upper.ext4" | cut -f1) on disk"
  (cd "$m/upper" && find . -mindepth 1 -printf '%M %y %p\n' | sort)
  echo "probe=$(cat "$m/upper/probe")"
  stat -c 'etc/motd whiteout: %F major=%t minor=%T' "$m/upper/etc/motd"
  umount "$m" && rmdir "$m"
done

SUM_AFTER=$(md5sum "$LOWER" | cut -d' ' -f1)
[ "$SUM_BEFORE" = "$SUM_AFTER" ] && echo "shared lower unchanged ($SUM_AFTER)" || { echo "LOWER CHANGED"; rc=1; }
exit $rc
