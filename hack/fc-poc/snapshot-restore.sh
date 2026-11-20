#!/usr/bin/env bash
# Step 1c: snapshot a guest with a running container, kill it, restore it into a new
# Firecracker process at different paths, and check the agent, the container and the clock.
set -euo pipefail
DIR=/opt/fc-poc
HERE=$(dirname "$0")
SRC=$DIR/run/vm-snap
DST=$DIR/run/vm-restore
SNAP=$DIR/run/snap
GAP=${GAP:-5}

now_ms() { date +%s%3N; }
api() { curl -s --unix-socket "$1/api.sock" -X "$2" "http://localhost$3" -H 'Content-Type: application/json' -d "$4" -w "  [HTTP %{http_code}]\n"; }
guest() {
  { printf 'CONNECT 1026\n'; sleep 1; printf '%s; exit\n' "$2"; sleep 1.5; } \
    | timeout 6 socat -t2 - UNIX-CONNECT:"$1/kata.hvsock" | grep -v -E '^(OK |exit|.*; exit)' || true
}
COUNTER='echo counter=$(cat /run/kata-containers/ctr0/rootfs/counter) guest_epoch=$(date +%s)'

echo "=== boot source VM and start a long-running container"
"$HERE/boot-kata-guest.sh" vm-snap "$DIR/alpine.ext4"
"$DIR/fcpoc" -vsock "$SRC/kata.hvsock" -detach \
  -cmd 'i=0; while true; do i=$((i+1)); echo $i > /counter; sleep 0.2; done'
sleep 3
guest "$SRC" "$COUNTER"; echo "host_epoch=$(date +%s)"

echo "=== pause and take a full snapshot"
rm -rf "$SNAP" && mkdir -p "$SNAP"
t=$(now_ms); api "$SRC" PATCH /vm '{"state":"Paused"}'; echo "pause $(( $(now_ms) - t )) ms"
t=$(now_ms)
api "$SRC" PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$SNAP/vmstate\",\"mem_file_path\":\"$SNAP/mem\"}"
echo "snapshot-create $(( $(now_ms) - t )) ms"
ls -ls "$SNAP"
cp --sparse=always "$SRC/ctr0.ext4" "$SNAP/ctr0.ext4"
kill "$(cat "$SRC/fc.pid")"; echo "source VM killed; waiting ${GAP}s before restore"
sleep "$GAP"

echo "=== restore into a new Firecracker process at new paths"
rm -rf "$DST" && mkdir -p "$DST"
cp --sparse=always "$SNAP/ctr0.ext4" "$DST/ctr0.ext4"
T0=$(now_ms)
"$DIR/firecracker" --api-sock "$DST/api.sock" > "$DST/console.log" 2>&1 &
echo $! > "$DST/fc.pid"
until [ -S "$DST/api.sock" ]; do sleep 0.01; done
t=$(now_ms)
api "$DST" PUT /snapshot/load "{
  \"snapshot_path\": \"$SNAP/vmstate\",
  \"mem_backend\": {\"backend_type\": \"File\", \"backend_path\": \"$SNAP/mem\"},
  \"drive_overrides\": [{\"drive_id\": \"ctr0\", \"path_on_host\": \"$DST/ctr0.ext4\"}],
  \"vsock_override\": {\"uds_path\": \"$DST/kata.hvsock\"},
  \"resume_vm\": true
}"
echo "snapshot-load $(( $(now_ms) - t )) ms"
for i in $(seq 1 200); do
  reply=$(printf 'CONNECT 1024\n' | timeout 1 socat -t1 - UNIX-CONNECT:"$DST/kata.hvsock" 2>/dev/null | head -1 || true)
  [[ "$reply" == OK* ]] && { echo "kata-agent reachable $(( $(now_ms) - T0 )) ms after Firecracker start"; break; }
  sleep 0.02
done

echo "=== checks after restore"
"$DIR/fcpoc" -vsock "$DST/kata.hvsock" -stats || echo "STATS FAILED"
guest "$DST" "$COUNTER"; echo "host_epoch=$(date +%s)"
sleep 2
guest "$DST" "$COUNTER"; echo "host_epoch=$(date +%s)"
echo "restored firecracker RSS: $(ps -o rss= -p "$(cat "$DST/fc.pid")") KiB"
echo "--- firecracker console/log tail"; tail -n 5 "$DST/console.log"
kill "$(cat "$DST/fc.pid")" 2>/dev/null || true
