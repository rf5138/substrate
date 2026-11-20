#!/usr/bin/env bash
# Install Firecracker, the Kata guest assets (the Kata version Substrate pins), Go, and build fcpoc.
set -euo pipefail
FC_VER=v1.18.0
KATA_VER=4.3.1
GO_VER=1.27.4
ALPINE_TAR=https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/alpine-minirootfs-3.24.4-x86_64.tar.gz
DIR=/opt/fc-poc
REPO=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$DIR" && cd "$DIR"

dnf install -y -q zstd jq tar socat iproute e2fsprogs bc git

if [ ! -x "$DIR/firecracker" ]; then
  curl -fsSL -o fc.tgz "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VER}/firecracker-${FC_VER}-x86_64.tgz"
  tar -xzf fc.tgz && cp "release-${FC_VER}-x86_64/firecracker-${FC_VER}-x86_64" "$DIR/firecracker" && chmod +x "$DIR/firecracker"
fi

if [ ! -f "$DIR/vmlinux" ]; then
  curl -fsSL -o kata.tar.zst "https://github.com/kata-containers/kata-containers/releases/download/${KATA_VER}/kata-static-${KATA_VER}-amd64.tar.zst"
  mkdir -p kata && tar --zstd -xf kata.tar.zst -C kata
  cp -L kata/opt/kata/share/kata-containers/vmlinux.container "$DIR/vmlinux"
  cp -L kata/opt/kata/share/kata-containers/kata-containers.img "$DIR/rootfs.img"
  rm -f kata.tar.zst
fi

# Container image as an ext4 block image (stands in for an image-cache entry).
if [ ! -f "$DIR/alpine.ext4" ]; then
  rm -rf alpine-root && mkdir alpine-root
  curl -fsSL "$ALPINE_TAR" | tar -xz -C alpine-root
  mkdir -p alpine-root/proc alpine-root/sys alpine-root/dev alpine-root/tmp
  truncate -s 256M alpine.ext4 && mkfs.ext4 -q -F -d alpine-root alpine.ext4
fi

if [ ! -x /usr/local/go/bin/go ] || ! /usr/local/go/bin/go version | grep -q "go${GO_VER}"; then
  rm -rf /usr/local/go && curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" | tar -xz -C /usr/local
fi
(cd "$REPO" && GOFLAGS=-mod=vendor CGO_ENABLED=0 /usr/local/go/bin/go build -o "$DIR/fcpoc" ./cmd/ateworker-microvm/fcpoc)

ls -l /dev/kvm
"$DIR/firecracker" --version | head -1
ls -l "$DIR"
