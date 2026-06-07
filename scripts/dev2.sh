#!/usr/bin/env bash
# Launch two ensemble nodes on loopback with four disjoint port blocks, tmp data
# dirs, and the null sink. Used by `make dev` and sourced by e2e.sh (DEV2_WAIT=0,
# which lets the caller control lifetime and start N3 for the late-join test).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/ensemble"
go build -o "$BIN" ./cmd/ensemble

# A canonical 48k stereo s16le tone for `play`.
[ -f "$ROOT/testdata/media/tone.wav" ] || "$ROOT/scripts/fixtures.sh"

DATA1="$(mktemp -d)"; DATA2="$(mktemp -d)"; DATA3="$(mktemp -d)"
LOG1="$(mktemp)"; LOG2="$(mktemp)"; LOG3="$(mktemp)"
export ENSEMBLE_OUTPUT=null
export ENSEMBLE_LOG="${ENSEMBLE_LOG:-info}"

# Four ports per node, distinct blocks:
#   N1: http 18080 stream 19090 source 19200 gossip 17946
#   N2: http 28080 stream 29090 source 29200 gossip 27946
#   N3: http 38080 stream 39090 source 39200 gossip 37946  (started later by e2e)
"$BIN" --data "$DATA1" --media "$ROOT/testdata/media" --name n1 --host 127.0.0.1 \
       --http-port 18080 --stream-port 19090 --source-port 19200 --gossip-port 17946 \
       >"$LOG1" 2>&1 &  PID1=$!
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" --name n2 --host 127.0.0.1 \
       --http-port 28080 --stream-port 29090 --source-port 29200 --gossip-port 27946 \
       --join 127.0.0.1:17946 >"$LOG2" 2>&1 &  PID2=$!

trap 'kill $PID1 $PID2 ${PID3:-} 2>/dev/null || true' EXIT INT TERM
echo "N1=http://127.0.0.1:18080  N2=http://127.0.0.1:28080"
echo "DATA1=$DATA1 DATA2=$DATA2 DATA3=$DATA3 LOG1=$LOG1 LOG2=$LOG2 LOG3=$LOG3"
echo "ROOT=$ROOT BIN=$BIN"
if [ "${DEV2_WAIT:-1}" = 1 ]; then
  wait
fi
