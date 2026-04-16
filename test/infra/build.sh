#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Building oc-test image..."
docker build -t oc-test:latest "$SCRIPT_DIR"

echo "==> Building oc-ollama-test image..."
docker build -t oc-ollama-test:latest -f "$SCRIPT_DIR/Dockerfile.ollama" "$SCRIPT_DIR"

echo "==> Building oc-mesh-test image..."
docker build -t oc-mesh-test:latest -f "$SCRIPT_DIR/Dockerfile.mesh" "$SCRIPT_DIR"

echo "==> Done. Images ready:"
docker images --format "  {{.Repository}}:{{.Tag}}  {{.Size}}" | grep oc-
