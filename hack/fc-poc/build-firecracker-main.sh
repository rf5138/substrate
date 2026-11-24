#!/usr/bin/env bash
# Build Firecracker from main (which has drive_overrides and pmem_overrides) with its
# own container-based build tool, and install it as /opt/fc-poc/firecracker-main.
set -euo pipefail
DIR=/opt/fc-poc
REF=${FC_REF:-main}
systemctl start docker
rm -rf "$DIR/firecracker-src"
git clone -q --depth 1 -b "$REF" https://github.com/firecracker-microvm/firecracker.git "$DIR/firecracker-src"
cd "$DIR/firecracker-src"
echo "building firecracker $(git log -1 --format='%h (%cd)')"
tools/devtool -y build --release
cp "build/cargo_target/$(uname -m)-unknown-linux-musl/release/firecracker" "$DIR/firecracker-main"
"$DIR/firecracker-main" --version | head -1
