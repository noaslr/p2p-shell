#!/bin/bash
# Build p2p-agent for all target platforms.
# Outputs to ../p2p-web/b/p2p-agent/ so the binaries are served by the web UI.
# Override the output directory with: OUT_DIR=/path/to/dir ./build.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/../p2p-web/b/p2p-agent}"
mkdir -p "$OUT_DIR"

cd "$SCRIPT_DIR"

# Skip the checksum-DB round-trips (go.sum is already committed and verified).
export GONOSUMDB='*'
export GONOSUMCHECK='*'

echo "[*] Downloading dependencies..."
go mod download

echo "[*] Building p2p-agent for all platforms..."

build() {
    local goos=$1 goarch=$2 suffix=${3:-}
    local out="$OUT_DIR/${goos}-${goarch}${suffix}"
    printf "    %-30s → %s\n" "${goos}/${goarch}" "$(basename "$out")"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
        -ldflags="-s -w" \
        -trimpath \
        -o "$out" .
}

build linux   amd64
build linux   arm64
build linux   arm
build linux   386
build darwin  amd64
build darwin  arm64
build windows amd64 .exe
build windows arm64 .exe

echo ""
echo "[+] Build complete. Binaries:"
ls -lh "$OUT_DIR"
