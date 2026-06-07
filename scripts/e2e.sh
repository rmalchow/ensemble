#!/usr/bin/env bash
# End-to-end smoke test: builds the binary, launches two null-sink nodes on
# loopback (via dev2.sh), and asserts the full system through the REST API only.
# Prints PASS/FAIL per step; exits non-zero on any FAIL. No fixed sleeps where
# polling works; mDNS is not relied upon (explicit --join seed).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# ---- launch two nodes (sourced; caller controls lifetime) -------------------
DEV2_WAIT=0
# shellcheck disable=SC1091
source "$ROOT/scripts/dev2.sh"

N1="http://127.0.0.1:18080"
N2="http://127.0.0.1:28080"
N3="http://127.0.0.1:38080"

PASS=0; FAIL=0
pass() { echo "PASS: $*"; PASS=$((PASS+1)); }
die()  { echo "FAIL: $*" >&2; FAIL=$((FAIL+1)); cleanup_fail; exit 1; }

cleanup_fail() {
  echo "----- N1 log -----"; tail -n 40 "$LOG1" 2>/dev/null || true
  echo "----- N2 log -----"; tail -n 40 "$LOG2" 2>/dev/null || true
  echo "----- N3 log -----"; tail -n 40 "$LOG3" 2>/dev/null || true
}
trap 'kill $PID1 $PID2 ${PID3:-} 2>/dev/null || true' EXIT INT TERM

api()  { curl -fsS -H 'Accept: application/json' "$@"; }
post() { curl -fsS -X POST -H 'Content-Type: application/json' -d "${2:-}" "$1"; }
patchj(){ curl -fsS -X PATCH -H 'Content-Type: application/json' -d "$2" "$1"; }

# post_retry url body [timeout_s] — POST until it returns 2xx (tolerates the
# transient not-synced/not-master window right after a group change).
post_retry() {
  local url=$1 body=$2 t=${3:-15}
  for _ in $(seq $((t*2))); do
    if curl -fsS -X POST -H 'Content-Type: application/json' -d "$body" "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "POST kept failing: $url $body" >&2
  return 1
}

# wait_for url jqfilter expected [timeout_s]
wait_for() {
  local url=$1 f=$2 want=$3 t=${4:-20} got
  for _ in $(seq $((t*4))); do
    got=$(api "$url" 2>/dev/null | jq -r "$f" 2>/dev/null || true)
    [ "$got" = "$want" ] && return 0
    sleep 0.25
  done
  echo "TIMEOUT: $url  $f != $want  (last=$got)" >&2
  return 1
}

xor16() { python3 - "$1" "$2" <<'PY'
import sys
a=bytes.fromhex(sys.argv[1]); b=bytes.fromhex(sys.argv[2])
print(bytes(x^y for x,y in zip(a,b)).hex())
PY
}

# rising url jqfilter [settle_s] — true if the value strictly increases over settle.
rising() {
  local url=$1 f=$2 s=${3:-1} a b
  a=$(api "$url" | jq -r "$f")
  sleep "$s"
  b=$(api "$url" | jq -r "$f")
  awk -v a="$a" -v b="$b" 'BEGIN{exit !(b>a)}'
}

# ---- 1. both up; ports.source nonzero ---------------------------------------
wait_for "$N1/api/status" '.id|length' 32 30 || die "N1 not up"
wait_for "$N2/api/status" '.id|length' 32 30 || die "N2 not up"
ID1=$(api "$N1/api/status" | jq -r .id)
ID2=$(api "$N2/api/status" | jq -r .id)
wait_for "$N1/api/status" '.ports.source>0' true || die "N1 ports.source not >0"
wait_for "$N2/api/status" '.ports.source>0' true || die "N2 ports.source not >0"
pass "1 both up; ports.source nonzero (ID1=$ID1 ID2=$ID2)"

# ---- 2. cluster convergence: both nodes alive on each ------------------------
wait_for "$N1/api/cluster" '[.nodes[]|select(.alive)]|length' 2 30 || die "N1 cluster not converged"
wait_for "$N2/api/cluster" '[.nodes[]|select(.alive)]|length' 2 30 || die "N2 cluster not converged"
wait_for "$N1/api/cluster" '[.nodes[]|select(.sourcePort>0)]|length' 2 || die "sourcePort not advertised by both"
pass "2 cluster converged; both nodes alive + advertise sourcePort"

# ---- 3. capabilities reflect host: opus + alsa on n1 ------------------------
# NB: avoid `ldconfig | grep -q` — grep -q closes the pipe early, SIGPIPEs
# ldconfig, and trips pipefail. Capture first, then match.
LDCACHE=$(ldconfig -p 2>/dev/null || true)
has_lib() { case "$LDCACHE" in *"$1"*) echo true;; *) echo false;; esac; }
HAS_OPUS=$(has_lib "libopus.so")
HAS_ALSA=$(has_lib "libasound.so")
o1=$(api "$N1/api/cluster" | jq -r --arg id "$ID1" '[.nodes[]|select(.id==$id).capabilities.codecs[]]|index("opus")!=null')
b1=$(api "$N1/api/cluster" | jq -r --arg id "$ID1" '[.nodes[]|select(.id==$id).capabilities.backends[]]|index("alsa")!=null')
[ "$o1" = "$HAS_OPUS" ] || die "n1 codecs/opus=$o1 want $HAS_OPUS"
[ "$b1" = "$HAS_ALSA" ] || die "n1 backends/alsa=$b1 want $HAS_ALSA"
pass "3 capabilities reflect host (opus=$o1 alsa=$b1)"

# ---- 4. follow forms a 2-node group with XOR id -----------------------------
post "$N2/api/follow" "{\"target\":\"$ID1\"}"
GID=$(xor16 "$ID1" "$ID2")
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'")|.members|length' 2 || die "group not formed (master n1)"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'").id' "$GID" || die "group id != XOR"
pass "4 follow → 2-node group, master n1, id=XOR ($GID)"

# ---- 5. play → BOTH sinks play; n1 source.clients == 2 ----------------------
post_retry "$N1/api/play" '{"uri":"file:tone.wav"}' || die "play on n1 kept failing"
wait_for "$N1/api/status" '.sink.played>0' true 8 || die "n1 sink not playing"
wait_for "$N2/api/status" '.sink.played>0' true 8 || die "n2 sink not playing"
wait_for "$N1/api/status" '.source.clients' 2 8 || die "n1 source.clients != 2"
pass "5 play → both sinks playing; n1 source.clients=2"

# ---- 6. clock synced on both ------------------------------------------------
wait_for "$N1/api/status" '.sink.synced' true 8 || die "n1 not synced"
wait_for "$N2/api/status" '.sink.synced' true 8 || die "n2 not synced"
pass "6 clock synced on both"

# ---- 7. proxy: n1 /api/<n2id>/status returns n2's id ------------------------
PROXIED=$(api "$N1/api/$ID2/status" | jq -r .id)
[ "$PROXIED" = "$ID2" ] || die "proxy returned $PROXIED want $ID2"
pass "7 proxy n1→n2 status returns n2 id"

# ---- 8. volume: PATCH n2 {volume:0.5} → cluster shows 0.5 -------------------
patchj "$N2/api/node" '{"volume":0.5}'
wait_for "$N1/api/cluster" '.nodes[]|select(.id=="'"$ID2"'").volume' 0.5 || die "n2 volume not 0.5"
pass "8 volume PATCH n2=0.5 replicated"

# ---- 9. settings change mid-play → resubscribe, playback continues ----------
# N1 is still master at this point (takeover is step 10), so settings go to N1.
post_retry "$N1/api/group/settings" '{"codec":"pcm","transport":"udp","bufferMs":200}' || die "settings POST failed"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'").settings.bufferMs' 200 || die "bufferMs not 200"
rising "$N1/api/status" '.sink.played' 1 || die "n1 playback stalled after settings change"
rising "$N2/api/status" '.sink.played' 1 || die "n2 playback stalled after settings change"
pass "9 live settings change (bufferMs=200), playback continues on both"

# ---- 10. takeover: make n2 master; group id unchanged -----------------------
post "$N1/api/group/master" "{\"node\":\"$ID2\"}"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").master' "$ID2" 15 || die "takeover: master not n2"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'")|.members|length' 2 || die "takeover: members != 2"
pass "10 takeover → master n2, group id unchanged"

# n2 is master now; restart playback on the new master so both sinks subscribe.
post_retry "$N2/api/play" '{"uri":"file:tone.wav"}' || die "play on n2 (new master) kept failing"
wait_for "$N1/api/status" '.sink.played>0' true 8 || die "n1 not playing after takeover"
wait_for "$N2/api/status" '.sink.played>0' true 8 || die "n2 not playing after takeover"

# ---- 11. opus leg (only when both nodes report opus) ------------------------
o2=$(api "$N2/api/cluster" | jq -r --arg id "$ID2" '[.nodes[]|select(.id==$id).capabilities.codecs[]]|index("opus")!=null')
if [ "$o1" = true ] && [ "$o2" = true ]; then
  P1=$(api "$N1/api/status" | jq .sink.played)
  P2=$(api "$N2/api/status" | jq .sink.played)
  post "$N2/api/group/settings" '{"codec":"opus","transport":"udp","bufferMs":200}'
  wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID2"'").settings.codec' opus || die "codec not opus"
  sleep 1
  api "$N1/api/status" | jq -e '.sink.played > '"$P1" >/dev/null || die "n1 opus playback stalled"
  api "$N2/api/status" | jq -e '.sink.played > '"$P2" >/dev/null || die "n2 opus playback stalled"
  post "$N2/api/group/settings" '{"codec":"pcm","transport":"udp","bufferMs":200}'
  wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID2"'").settings.codec' pcm || die "codec not reset to pcm"
  pass "11 opus leg: both sinks play through opus; reset to pcm"
else
  pass "11 opus leg skipped (codecs opus not present on both: n1=$o1 n2=$o2)"
fi

# ---- 12. stop → playback idle on both ---------------------------------------
post "$N2/api/stop"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").playback.state' idle || die "playback not idle after stop"
# Once the session ends the source goes idle: /api/status omits .source entirely
# (it is present only while a source is actively running, D19).
wait_for "$N2/api/status" '.source == null' true 8 || die "n2 source still active after stop"
pass "12 stop → playback idle on both; n2 source goes idle"

# ---- 13. kill the master (n2) → n1 reverts to solo within ~15s --------------
kill "$PID2" 2>/dev/null || true
wait_for "$N1/api/status" '.role' solo 20 || die "n1 did not revert to solo after master death"
pass "13 master (n2) killed → n1 self-heals to solo"

echo
echo "e2e OK ($PASS passed, $FAIL failed)"
