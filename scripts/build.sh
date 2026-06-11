#!/usr/bin/env bash
# Build ensemble for linux/amd64 + arm64 + armv6 into bin/, plus a host-arch
# ./ensemble for local runs (dev2.sh / e2e.sh). Pure Go (CGO_ENABLED=0), so
# cross-compiling needs no toolchain. The committed web/dist placeholder makes
# go:embed compile without node; pass --ui to (re)build and embed the SPA.
#   ./scripts/build.sh          -> bin/ensemble-linux-{amd64,arm64,armv6} + ./ensemble
#   ./scripts/build.sh --ui     -> SPA build first, then the same
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "${1:-}" == "--ui" ]]; then
  ./scripts/ui.sh
fi

VER="${VERSION:-$(git describe --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=$VER"

mkdir -p bin
# arch matrix: "<name>:<GOARCH>:<GOARM>:<interp>" (GOARM/interp blank when N/A). The
# armv6* targets the Pi Zero W / Pi 1 (ARM1176, ARMv6+VFPv2) — GOARM=6 is mandatory
# there; a GOARM=7 binary SIGILLs. Pure Go + purego (>=0.10 supports linux/arm), so
# no C toolchain.
#
# purego forces a *dynamically linked* binary on linux/arm even with CGO_ENABLED=0
# (amd64/arm64 stay static). Go's linker then stamps the soft-float armel loader
# /lib/ld-linux.so.3 as the ELF interpreter. That works on armel systems but NOT on
# hard-float armhf userlands like Raspberry Pi OS, which only ship
# /lib/ld-linux-armhf.so.3 — there the armel binary dies at exec with "cannot
# execute: required file not found". So we ship BOTH 32-bit builds from the same
# GOARM=6 code, differing only in the ELF interpreter (-ldflags -I):
#   armv6   → /lib/ld-linux.so.3        soft-float armel
#   armv6hf → /lib/ld-linux-armhf.so.3  hard-float armhf (Raspberry Pi OS, most distros)
for spec in "amd64:amd64::" "arm64:arm64::" "armv6:arm:6:" "armv6hf:arm:6:/lib/ld-linux-armhf.so.3"; do
  IFS=: read -r name goarch goarm interp <<<"$spec"
  ldflags="$LDFLAGS"
  [ -n "$interp" ] && ldflags="$ldflags -I $interp"
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" GOARM="$goarm" \
    go build -trimpath -ldflags "$ldflags" -o "bin/ensemble-linux-$name" ./cmd/ensemble
  echo "built bin/ensemble-linux-$name"
done

# Host-arch convenience binary at the repo root.
case "$(uname -m)" in
  x86_64)  cp "bin/ensemble-linux-amd64" ensemble ;;
  aarch64) cp "bin/ensemble-linux-arm64" ensemble ;;
  armv6l | armv7l | arm)
           # match the interpreter to this host's float ABI (armhf if present, else armel)
           hi="$LDFLAGS"; [ -e /lib/ld-linux-armhf.so.3 ] && hi="$LDFLAGS -I /lib/ld-linux-armhf.so.3"
           CGO_ENABLED=0 go build -trimpath -ldflags "$hi" -o ensemble ./cmd/ensemble ;;
  *)       CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o ensemble ./cmd/ensemble ;;
esac
echo "built ./ensemble ($VER)"
