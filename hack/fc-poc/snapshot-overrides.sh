#!/usr/bin/env bash
# Test drive_overrides and pmem_overrides (Firecracker main): snapshot a guest with a
# running container, delete everything at the paths the snapshot recorded, and restore
# it from a different directory with every disk, the guest image and the vsock moved.
set -euo pipefail
DIR=/opt/fc-poc
HERE=$(dirname "$0")
export FC_BIN=${FC_BIN:-$DIR/firecracker-main}
SRC=$DIR/run/vm-ovr-src
DST=$DIR/run/vm-ovr-dst
SNAP=$DIR/run/snap-ovr

now_ms() { date +%s%3N; }
api() { curl -s --unix-socket "$1/api.sock" -X "$2" "http://localhost$3" -H 'Content-Type: application/json' -d "$4" -w "  [HTTP %{http_code}]\n"; }
guest() {
  { printf 'CONNECT 1026\n'; sleep 1; printf '%s; exit\n' "$2"; sleep 1.5; } \
    | timeout 6 socat -t2 - UNIX-CONNECT:"$1/kata.hvsock" | grep -v -E '^(OK |exit|.*; exit)' || true
}
COUNTER='echo counter=$(cat /run/kata-containers/ctr0/rootfs/counter)'

"$FC_BIN" --version | head -1
echo "=== boot the source VM from $SRC, with its own copy of the guest image"
mkdir -p "$SRC" && cp --sparse=always "$DIR/rootfs.img" "$SRC/guest.img"
GUEST_IMAGE="$SRC/guest.img" "$HERE/boot-kata-guest.sh" vm-ovr-src "$DIR/alpine.ext4"
"$DIR/fcpoc" -vsock "$SRC/kata.hvsock" -detach \
  -cmd 'i=0; while true; do i=$((i+1)); echo $i > /counter; sleep 0.2; done'
sleep 3
guest "$SRC" "$COUNTER"

echo "=== pause, snapshot, then delete the source directory"
rm -rf "$SNAP" && mkdir -p "$SNAP"
api "$SRC" PATCH /vm '{"state":"Paused"}'
api "$SRC" PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$SNAP/vmstate\",\"mem_file_path\":\"$SNAP/mem\",\"sync_snapshot_files\":false}"
cp --sparse=always "$SRC/ctr0.ext4" "$SNAP/ctr0.ext4"
kill "$(cat "$SRC/fc.pid")"; sleep 0.5
rm -rf "$SRC"
echo "recorded paths gone: $(ls "$SRC" 2>&1 | head -1)"

echo "=== restore from $DST with overrides"
rm -rf "$DST" && mkdir -p "$DST"
cp --sparse=always "$SNAP/ctr0.ext4" "$DST/ctr0.ext4"
cp --sparse=always "$DIR/rootfs.img" "$DST/guest.img"
"$FC_BIN" --api-sock "$DST/api.sock" > "$DST/console.log" 2>&1 &
echo $! > "$DST/fc.pid"
until [ -S "$DST/api.sock" ]; do sleep 0.01; done
t=$(now_ms)
api "$DST" PUT /snapshot/load "{
  \"snapshot_path\": \"$SNAP/vmstate\",
  \"mem_backend\": {\"backend_type\": \"File\", \"backend_path\": \"$SNAP/mem\"},
  \"drive_overrides\": [{\"drive_id\": \"ctr0\", \"path_on_host\": \"$DST/ctr0.ext4\"}],
  \"pmem_overrides\": [{\"id\": \"rootfs\", \"path_on_host\": \"$DST/guest.img\"}],
  \"vsock_override\": {\"uds_path\": \"$DST/kata.hvsock\"},
  \"clock_realtime\": true,
  \"resume_vm\": true
}"
echo "snapshot-load $(( $(now_ms) - t )) ms"

echo "=== checks after restore"
"$DIR/fcpoc" -vsock "$DST/kata.hvsock" -stats || echo "STATS FAILED"
guest "$DST" "$COUNTER"; sleep 2; guest "$DST" "$COUNTER"
echo "open files of the restored firecracker:"
ls -l /proc/"$(cat "$DST/fc.pid")"/fd 2>/dev/null | grep -E "ext4|guest.img|mem" | awk '{print "  " $NF}'
kill "$(cat "$DST/fc.pid")" 2>/dev/null || true
