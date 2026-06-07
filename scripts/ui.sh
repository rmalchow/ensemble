#!/usr/bin/env bash
# Build the Svelte SPA into web/dist (consumed by go:embed at build time).
set -euo pipefail
cd "$(dirname "$0")/../web"

npm install
npm run build
echo "built web/dist"
