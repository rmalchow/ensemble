#!/usr/bin/env bash
# Dev helpers. Usage: scripts/dev.sh <fmt|vet|test|tidy|web|build-web> [args...]
set -euo pipefail

cd "$(dirname "$0")/.."

cmd="${1:-}"; shift || true

case "$cmd" in
  fmt)   gofmt -l -w . ;;
  vet)   go vet ./... ;;
  test)  go test ./... "$@" ;;
  tidy)  go mod tidy ;;
  web)       ( cd web && npm run dev ) ;;
  build-web) ( cd web && npm run build ) ;;
  *)
    echo "usage: scripts/dev.sh <fmt|vet|test|tidy|web|build-web> [args...]" >&2
    exit 2
    ;;
esac
