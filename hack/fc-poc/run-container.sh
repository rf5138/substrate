#!/usr/bin/env bash
# Step 1b: boot a guest with the Alpine image on virtio-blk and run one container from it.
set -euo pipefail
DIR=/opt/fc-poc
NAME=${1:-vm-blk}
"$(dirname "$0")/boot-kata-guest.sh" "$NAME" "$DIR/alpine.ext4"
RUN=$DIR/run/$NAME
set +e
"$DIR/fcpoc" -vsock "$RUN/kata.hvsock" -dev /dev/vda -fstype ext4
rc=$?
set -e
if [ $rc -ne 0 ]; then
  echo "--- kata-agent errors"; grep -E '"level":"(ERRO|WARN)"' "$RUN/console.log" | tail -n 15
fi
echo "--- guest block devices (debug console)"
{ printf 'CONNECT 1026\n'; sleep 1; printf 'ls -l /dev/vd*; mount | grep -E "vda|kata-containers/shared"; exit\n'; sleep 2; } \
  | timeout 6 socat -t3 - UNIX-CONNECT:"$RUN/kata.hvsock" | tail -n +2 || true
kill "$(cat "$RUN/fc.pid")" 2>/dev/null || true
exit $rc
