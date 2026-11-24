#!/usr/bin/env bash
# Build Firecracker from source (by default the v1.18.0 backport of drive_overrides and
# pmem_overrides) with its own container-based build tool, and install it as
# /opt/fc-poc/firecracker-main.
set -euo pipefail
DIR=/opt/fc-poc
REPO=${FC_REPO:-https://github.com/rf5138/firecracker.git}
REF=${FC_REF:-v1.18.0-snapshot-overrides}
systemctl start docker
rm -rf "$DIR/firecracker-src"
git clone -q --depth 1 -b "$REF" "$REPO" "$DIR/firecracker-src"
cd "$DIR/firecracker-src"
echo "building firecracker $(git log -1 --format='%h (%cd)')"
tools/devtool -y build --release
cp "build/cargo_target/$(uname -m)-unknown-linux-musl/release/firecracker" "$DIR/firecracker-main"
"$DIR/firecracker-main" --version | head -1
