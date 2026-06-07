#!/usr/bin/env bash
# Build ensemble for linux/amd64 + linux/arm64 into bin/, plus a host-arch
# ./ensemble for local runs (dev2.sh / e2e.sh). Pure Go (CGO_ENABLED=0), so
# cross-compiling needs no toolchain. The committed web/dist placeholder makes
# go:embed compile without node; pass --ui to (re)build and embed the SPA.
#   ./scripts/build.sh          -> bin/ensemble-linux-{amd64,arm64} + ./ensemble
#   ./scripts/build.sh --ui     -> SPA build first, then the same
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "${1:-}" == "--ui" ]]; then
  ./scripts/ui.sh
fi

VER="${VERSION:-$(git describe --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=$VER"

mkdir -p bin
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "bin/ensemble-linux-$arch" ./cmd/ensemble
  echo "built bin/ensemble-linux-$arch"
done

# Host-arch convenience binary at the repo root.
case "$(uname -m)" in
  x86_64)  cp "bin/ensemble-linux-amd64" ensemble ;;
  aarch64) cp "bin/ensemble-linux-arm64" ensemble ;;
  *)       CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o ensemble ./cmd/ensemble ;;
esac
echo "built ./ensemble ($VER)"
