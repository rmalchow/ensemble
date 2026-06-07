#!/usr/bin/env bash
# Quality gate: vet + tests + gofmt. Same checks CI runs.
set -euo pipefail
cd "$(dirname "$0")/.."

go vet ./...
go test ./...

out="$(gofmt -l . 2>/dev/null | grep -v node_modules || true)"
if [[ -n "$out" ]]; then
  echo "gofmt needs to run on:" >&2
  echo "$out" >&2
  exit 1
fi
echo "check OK"
