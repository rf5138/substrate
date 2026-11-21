#!/usr/bin/env bash
# Baseline numbers for Substrate's microVM mode on Cloud Hypervisor: create an actor
# from the counter-microvm template, then suspend and resume it a few times.
set -euo pipefail
export PATH=/usr/local/go/bin:/root/go/bin:/usr/local/bin:$PATH
AS=ate-demo-counter-microvm
ACTOR=${ACTOR:-bench-$(date +%H%M%S)}
ROUNDS=${ROUNDS:-5}
PORT=8000
ms() { date +%s%3N; }
hit() { curl -s -o /tmp/ch-body -w '%{http_code} %{time_total}s' -X POST "http://127.0.0.1:$PORT/"; echo " | $(cat /tmp/ch-body)"; }

t=$(ms); kubectl ate create actor "$ACTOR" -a "$AS" --template counter-microvm >/dev/null
echo "create actor: $(( $(ms) - t )) ms"

kubectl ate proxy actor "$ACTOR" -a "$AS" >/tmp/ch-proxy.log 2>&1 &
PROXY=$!
trap 'kill $PROXY 2>/dev/null || true' EXIT
t=$(ms)
until curl -s -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null; do sleep 0.2; done
echo "proxy up and actor answering (includes the first resume from the golden snapshot): $(( $(ms) - t )) ms"
echo "first POST: $(hit)"

for i in $(seq 1 "$ROUNDS"); do
  t=$(ms); kubectl ate suspend actor "$ACTOR" -a "$AS" >/dev/null; s=$(( $(ms) - t ))
  t=$(ms); kubectl ate resume actor "$ACTOR" -a "$AS" >/dev/null; r=$(( $(ms) - t ))
  echo "round $i: suspend ${s} ms, resume ${r} ms, request after resume: $(hit)"
done

echo "=== worker phase timings (last checkpoint and restore)"
for pod in $(kubectl get pods -n "$AS" -o name); do
  kubectl logs -n "$AS" "$pod" --all-containers 2>/dev/null | grep -E "Checkpoint timing breakdown|Restore timing breakdown" | tail -n 2
done
kubectl ate get actor "$ACTOR" -a "$AS"
