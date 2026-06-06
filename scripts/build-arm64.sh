#!/usr/bin/env bash
# Cross-compile the ensemble binary for Raspberry Pi (linux/arm64).
set -euo pipefail

cd "$(dirname "$0")/.."

OUT="${OUT:-bin/ensemble-arm64}"
mkdir -p "$(dirname "$OUT")"

# Build the frontend into internal/web/dist before embedding it in the binary.
# The Vite build is host-arch-independent; only the Go cross-compile differs.
# Guarded so a backend-only checkout (no node) still builds off the committed
# placeholder dist/index.html.
if [ -d web/node_modules ] || [ ! -f internal/web/dist/index.html ]; then
  echo "==> building web (vite)"
  ( cd web && npm ci && npm run build )
fi

echo "==> cross-building $OUT (linux/arm64)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)" -o "$OUT" ./cmd/ensemble
echo "==> done: $OUT"
