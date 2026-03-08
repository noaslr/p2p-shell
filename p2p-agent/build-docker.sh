#!/bin/bash
# Build p2p-agent inside a Docker container (host-independent fallback).
# Outputs to ../p2p-web/b/p2p-agent/ so the binaries are served by the web UI.
# Override the output directory with: OUT_DIR=/path/to/dir ./build-docker.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/../p2p-web/b/p2p-agent}"
mkdir -p "$OUT_DIR"

echo "[*] Building p2p-agent in Docker (golang:1.21-alpine)..."
docker build \
    -f "$SCRIPT_DIR/Dockerfile.build" \
    --output "type=local,dest=$OUT_DIR" \
    "$SCRIPT_DIR"

echo ""
echo "[+] Build complete. Binaries:"
ls -lh "$OUT_DIR"
