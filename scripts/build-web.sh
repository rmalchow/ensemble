#!/usr/bin/env bash
# Build the Ensemble web UI (Svelte + Vite) into internal/web/dist, the
# //go:embed target. Extracted from the web portion of build.sh. Guarded so a
# backend-only checkout (no node) builds off the committed placeholder
# dist/index.html: build only when node_modules exists or the placeholder is
# still in place.
set -euo pipefail

cd "$(dirname "$0")/.."

PLACEHOLDER_MARK="Svelte/Vite build replaces this file"

if [ -d web/node_modules ] || grep -q "$PLACEHOLDER_MARK" internal/web/dist/index.html 2>/dev/null; then
  echo "==> building web (vite)"
  ( cd web && npm ci && npm run build )
  echo "==> done: internal/web/dist"
else
  echo "==> skipping web build (no node_modules and dist is already built)"
fi
