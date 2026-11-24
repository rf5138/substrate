#!/usr/bin/env bash
# Switch the kind cluster's counter-microvm demo to the Firecracker worker:
# build the worker base image, point ko at it, redeploy the control plane (for the
# worker's SYS_CHROOT capability) and the demo (fresh golden snapshot on Firecracker).
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
export PATH=/usr/local/go/bin:/root/go/bin:/usr/local/bin:$PATH
export ATE_INSTALL_KIND=true
BASE=localhost:5001/fc-worker-base:latest
cd "$REPO"

ctx=$(mktemp -d)
cp hack/fc-poc/Dockerfile.worker-base "$ctx/Dockerfile"
# The 1.18 backport of snapshot overrides (hack/fc-poc/build-firecracker-main.sh).
cp "${FC_BIN:-/opt/fc-poc/firecracker-main}" "$ctx/firecracker"
docker build -q -t "$BASE" "$ctx" && docker push -q "$BASE"
rm -rf "$ctx"

if ! grep -q 'cmd/ateworker-microvm: localhost:5001/fc-worker-base' .ko.yaml; then
  sed -i "/^baseImageOverrides:/a\\  go.ate.dev/substrate/cmd/ateworker-microvm: $BASE" .ko.yaml
fi

# The old golden snapshot is in Cloud Hypervisor's format.
go run ./cmd/ate-setup delete demo counter-microvm || true
go run ./cmd/ate-setup deploy ate-system --credential-provider='{"name":"k8s.io"}' \
  --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/atenet-client
go run ./cmd/ate-setup deploy demo counter-microvm || echo "!!! deploy demo counter-microvm failed; worker logs below"
echo "=== worker pools and templates"
kubectl get workerpools -A
kubectl ate get actor-templates -a ate-demo-counter-microvm
echo "=== Firecracker phases from the workers"
for pod in $(kubectl get pods -n ate-demo-counter-microvm -o name); do
  kubectl logs -n ate-demo-counter-microvm "$pod" --all-containers 2>/dev/null \
    | grep -E "Firecracker boot phases|Checkpoint timing breakdown|level\":\"ERROR" | tail -n 4 || true
done
