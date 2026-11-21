#!/usr/bin/env bash
# Baseline: install Substrate's microVM mode (Kata on Cloud Hypervisor) in a kind
# cluster on this KVM host, following docs/dev/microvm-local.md (Option A).
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
export PATH=/usr/local/go/bin:/root/go/bin:/usr/local/bin:$PATH
export ATE_INSTALL_KIND=true

dnf install -y -q docker
systemctl enable --now docker
if ! command -v kubectl >/dev/null; then
  v=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
  curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${v}/bin/linux/amd64/kubectl" && chmod +x /usr/local/bin/kubectl
fi
# kind nodes run many watchers; raise inotify and neighbor-table limits.
sysctl -q -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=1048576 \
  net.ipv4.neigh.default.gc_thresh1=4096 net.ipv4.neigh.default.gc_thresh2=16384 net.ipv4.neigh.default.gc_thresh3=32768

cd "$REPO"
if ! docker ps --format '{{.Names}}' | grep -qx kind-control-plane; then
  ./hack/create-kind-cluster.sh
fi
go run ./cmd/ate-setup deploy ate-system --credential-provider='{"name":"k8s.io"}' \
  --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/atenet-client
go run ./cmd/ate-setup deploy demo counter-microvm
go install ./cmd/kubectl-ate

echo "=== nodes advertising KVM"
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.capacity.ate\.dev/kvm}{"\n"}{end}'
echo "=== worker pools and templates"
kubectl get workerpools -A
kubectl ate get actor-templates -a ate-demo-counter-microvm
