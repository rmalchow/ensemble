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
export DEV2_STAGGER="${DEV2_STAGGER:-3}"   # real start gap: catches clock-epoch mixing
source "$ROOT/scripts/dev2.sh"

N1="http://127.0.0.1:18080"
N2="http://127.0.0.1:28080"
N3="http://127.0.0.1:38080"

# Build the protocol-minimal reference client (docs/DUMB-CLIENT.md) for the
# conformance leg (step 11b). It is a standalone, receive-only audio participant.
DUMB="$(mktemp)"
go build -o "$DUMB" "$ROOT/cmd/dumbclient"

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

# ---- 4. follow forms a 2-node group; id = master id (D42) -------------------
# Before forming the group, the two solo groups carry server-DERIVED labels
# (nameDerived=true) — each just the node's own name (n1 / n2).
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$ID1"'").nameDerived' true || die "solo n1 label not derived"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$ID1"'").name' n1 || die "solo n1 derived label != n1"
post "$N2/api/follow" "{\"target\":\"$ID1\"}"
# D42: the group id is the MASTER's node id (was XOR-of-members). With n1 master,
# the group id == ID1. (XOR of the member set is kept only as the override-name
# key, exercised via /api/group/name below.)
GID=$ID1
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'")|.members|length' 2 || die "group not formed (master n1)"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'").id' "$GID" || die "group id != master id"
# Fresh unnamed 2-node group → server-DERIVED label "n1 + n2" (names sorted).
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").nameDerived' true || die "fresh group label not derived"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").name' "n1 + n2" || die "derived label != 'n1 + n2'"
pass "4 follow → 2-node group, master n1, id=master id ($GID); derived label 'n1 + n2'"

# ---- 4b. name override: explicit name beats the derived label ---------------
# The override is keyed by the member-set XOR server-side (an override names a
# COMBINATION of rooms), so it survives the takeover in step 12. nameDerived
# flips to false.
post "$N1/api/group/name" "{\"group\":\"$GID\",\"name\":\"the-lab\"}"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").name' the-lab || die "override name not applied"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").nameDerived' false || die "override still flagged derived"
pass "4b explicit name override 'the-lab' (nameDerived=false)"

# ---- 5. play → BOTH sinks play; n1 source.clients == 2 ----------------------
post_retry "$N1/api/play" '{"uri":"file:tone.wav"}' || die "play on n1 kept failing"
wait_for "$N1/api/status" '.sink.played>0' true 8 || die "n1 sink not playing"
wait_for "$N2/api/status" '.sink.played>0' true 8 || die "n2 sink not playing"
wait_for "$N1/api/status" '.source.clients' 2 8 || die "n1 source.clients != 2"
# Epoch-mixing regression: with a staggered start the member's clock offset is
# seconds-large; playout must STILL hold only ~bufferMs of frames. A buffered
# depth tracking |offset| (≈50/frames per stagger second) means local time
# leaked into the pts/deadline translation (clock.MonoNow contract).
sleep 2
buf2=$(curl -s "$N2/api/status" | jq '.sink.buffered')
[ "$buf2" -lt 25 ] || die "n2 buffered=$buf2 (epoch mixing: playout lags the clock offset)"
pass "5 play → both sinks playing; n1 source.clients=2; buffered sane ($buf2)"

# ---- 5b. late join: a node following into a PLAYING group receives the stream -
# While n1's group is still playing the long tone, start n3 (own port block) and
# follow it into the running group. It must join the live stream: prime burst +
# member-side re-arm to the master's gen (the late-join stale-gen fix). Assert
# n3's sink advances and rises, and n1 now fans out to 3 clients. Then n3 leaves
# + shuts down so the remaining legs keep their 2-node assumptions.
"$BIN" --data "$DATA3" --media "$ROOT/testdata/media" --name n3 --host 127.0.0.1 \
       --http-port 38080 --stream-port 39090 --source-port 39200 --gossip-port 37946 \
       --no-mdns --join 127.0.0.1:17946 >"$LOG3" 2>&1 &  PID3=$!
wait_for "$N3/api/status" '.id|length' 32 30 || die "n3 did not start (late join)"
ID3=$(api "$N3/api/status" | jq -r .id)
wait_for "$N3/api/cluster" '[.nodes[]|select(.alive)]|length' 3 30 || die "n3 did not join cluster"
post_retry "$N3/api/follow" "{\"target\":\"$ID1\"}" || die "n3 follow into playing group failed"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'")|.members|length' 3 || die "n3 not in group"
wait_for "$N3/api/status" '.sink.played>0' true 5 || die "n3 (late join) sink not playing"
rising "$N3/api/status" '.sink.played' 1 || die "n3 (late join) sink not rising"
wait_for "$N1/api/status" '.source.clients' 3 8 || die "n1 source.clients != 3 with late joiner"
pass "5b late join: n3 follows a PLAYING group → its sink plays + rises; n1 clients=3"
# Remove n3: unfollow, then stop it so the rest of the suite stays 2-node.
post_retry "$N3/api/unfollow" '' || die "n3 unfollow failed"
kill "$PID3" 2>/dev/null || true; wait "$PID3" 2>/dev/null || true; PID3=
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'")|.members|length' 2 15 || die "group did not return to 2 after n3 left"

# ---- 6. pause (D39): both sinks STOP advancing; state=="paused" --------------
# Snapshot both played counters, pause, allow the in-flight tail to drain, then
# assert neither counter advances any further and the cluster shows "paused".
post "$N1/api/pause"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").playback.state' paused 8 || die "playback not paused"
sleep 1  # let any in-flight buffered frames drain out of both sinks
P1A=$(api "$N1/api/status" | jq .sink.played)
P2A=$(api "$N2/api/status" | jq .sink.played)
sleep 1
P1B=$(api "$N1/api/status" | jq .sink.played)
P2B=$(api "$N2/api/status" | jq .sink.played)
[ "$P1A" = "$P1B" ] || die "n1 sink kept advancing while paused ($P1A -> $P1B)"
[ "$P2A" = "$P2B" ] || die "n2 sink kept advancing while paused ($P2A -> $P2B)"
# Resume when not paused would be a 409; pause when paused too.
curl -fsS -X POST "$N1/api/pause" >/dev/null 2>&1 && die "second pause should 409" || true
pass "6 pause → both sinks frozen; state=paused"

# ---- 7. resume (D39): both sinks advance again; state=="playing" -------------
post_retry "$N1/api/resume" '' || die "resume kept failing"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").playback.state' playing 8 || die "playback not playing after resume"
rising "$N1/api/status" '.sink.played' 1 || die "n1 sink not advancing after resume"
rising "$N2/api/status" '.sink.played' 1 || die "n2 sink not advancing after resume"
pass "7 resume → both sinks advancing again; state=playing"

# ---- 8. clock synced on both ------------------------------------------------
wait_for "$N1/api/status" '.sink.synced' true 8 || die "n1 not synced"
wait_for "$N2/api/status" '.sink.synced' true 8 || die "n2 not synced"
pass "8 clock synced on both"

# ---- 9. proxy: n1 /api/<n2id>/status returns n2's id ------------------------
PROXIED=$(api "$N1/api/$ID2/status" | jq -r .id)
[ "$PROXIED" = "$ID2" ] || die "proxy returned $PROXIED want $ID2"
pass "9 proxy n1→n2 status returns n2 id"

# ---- 10. volume: PATCH n2 {volume:0.5} → cluster shows 0.5 ------------------
patchj "$N2/api/node" '{"volume":0.5}'
wait_for "$N1/api/cluster" '.nodes[]|select(.id=="'"$ID2"'").volume' 0.5 || die "n2 volume not 0.5"
pass "10 volume PATCH n2=0.5 replicated"

# ---- 11. settings change mid-play → resubscribe, playback continues ---------
# N1 is still master at this point (takeover is step 12), so settings go to N1.
post_retry "$N1/api/group/settings" '{"codec":"pcm","transport":"udp","bufferMs":200}' || die "settings POST failed"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'").settings.bufferMs' 200 || die "bufferMs not 200"
rising "$N1/api/status" '.sink.played' 1 || die "n1 playback stalled after settings change"
rising "$N2/api/status" '.sink.played' 1 || die "n2 playback stalled after settings change"
pass "11 live settings change (bufferMs=200), playback continues on both"

# ---- 11t. TCP transport leg: switch the live session to tcp audio -----------
# First e2e coverage of TCP audio end-to-end. n1 is master, mid-play (pcm from
# step 11). Switch transport to tcp (keep codec/buffer); subscribers resubscribe
# over the length-prefixed TCP framing (D13) and both sinks must KEEP rising.
# Then switch back to udp so the remaining legs run on the default transport.
post_retry "$N1/api/group/settings" '{"codec":"pcm","transport":"tcp","bufferMs":200}' || die "tcp settings POST failed"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'").settings.transport' tcp || die "transport not tcp"
rising "$N1/api/status" '.sink.played' 1 || die "n1 playback stalled on tcp transport"
rising "$N2/api/status" '.sink.played' 1 || die "n2 playback stalled on tcp transport"
post_retry "$N1/api/group/settings" '{"codec":"pcm","transport":"udp","bufferMs":200}' || die "revert to udp POST failed"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'").settings.transport' udp || die "transport not back to udp"
rising "$N1/api/status" '.sink.played' 1 || die "n1 playback stalled after udp revert"
rising "$N2/api/status" '.sink.played' 1 || die "n2 playback stalled after udp revert"
pass "11t TCP transport: live switch to tcp audio (both rising), reverted to udp"

# ---- 11b. dumb-client conformance: a protocol-minimal receiver plays --------
# The group is mid-play, master=n1, codec=pcm (step 11) — a clean PCM window.
# Launch the standalone reference client (cmd/dumbclient, no internal/ imports):
# it discovers via GET /api/cluster on n1, subscribes to the master's source,
# follows the master clock, and schedules PCM playout. It IS a source
# subscriber (so source.clients rises to 3) but is NOT a cluster gossip member —
# it never appears in groups[].members and never affects codec negotiation.
# Assert its own 1 Hz stats line shows played>0 and strictly rising, then kill it.
# Re-issue play on the master so a fresh, full-length PCM session is guaranteed
# to be in flight for the leg (the original tone may have finished by now).
post_retry "$N1/api/play" '{"uri":"file:tone.wav"}' || die "11b replay on n1 failed"
wait_for "$N1/api/status" '.sink.played>0' true 8 || die "11b n1 not playing before dumb leg"
DCLOG="$(mktemp)"
"$DUMB" --node 127.0.0.1:18080 --group "$ID1" --out null >"$DCLOG" 2>&1 &  DCPID=$!
# Wait for the first non-zero played count in its stats line.
dc_played() { grep -oE 'played=[0-9]+' "$DCLOG" | tail -n1 | cut -d= -f2; }
ok=
for _ in $(seq 24); do
  p=$(dc_played 2>/dev/null || true)
  [ -n "${p:-}" ] && [ "${p:-0}" -gt 0 ] && { ok=1; break; }
  sleep 0.25
done
[ -n "$ok" ] || { echo "----- dumbclient log -----"; cat "$DCLOG"; kill "$DCPID" 2>/dev/null || true; die "dumbclient never played (played>0)"; }
A=$(dc_played); sleep 1; B=$(dc_played)
[ "${B:-0}" -gt "${A:-0}" ] || { echo "----- dumbclient log -----"; cat "$DCLOG"; kill "$DCPID" 2>/dev/null || true; die "dumbclient played not rising ($A -> $B)"; }
# Invisible to gossip: it never joins the group's member set (still 2 members).
M=$(api "$N1/api/cluster" | jq '[.groups[]|select(.master=="'"$ID1"'").members[]]|length')
[ "$M" = 2 ] || { kill "$DCPID" 2>/dev/null || true; die "dumbclient leaked into group members ($M != 2)"; }
kill "$DCPID" 2>/dev/null || true; wait "$DCPID" 2>/dev/null || true
pass "11b dumb-client conformance: protocol-minimal receiver plays (played $A -> $B rising); not a cluster member"

# ---- 12. takeover: make n2 master; group id MOVES to n2 (D42) ---------------
# D42: the group id is the master's node id, so takeover changes it from ID1 to
# ID2. The membership (hence the XOR override key) is unchanged, so the explicit
# name "the-lab" CARRIES OVER (nameDerived stays false), and group SETTINGS carry
# over too (one extra SetGroupSettings during the handoff). Playback does NOT.
GID2=$ID2
# Post the takeover at the FOLLOWER (n2), not the master: §5.2/D17 — the API
# forwards it to the current master (one hop). This is exactly what the UI's
# play-from-node flow does; locks the forwarding path.
post "$N2/api/group/master" "{\"node\":\"$ID2\"}"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID2"'").master' "$ID2" 15 || die "takeover: master not n2"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID2"'")|.members|length' 2 || die "takeover: members != 2"
# Name override survives the master change (XOR-keyed), still non-derived.
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID2"'").name' the-lab 15 || die "takeover: name override not carried"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID2"'").nameDerived' false || die "takeover: name flagged derived"
# Settings carried over to the new master's key (bufferMs=200 from step 11).
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID2"'").settings.bufferMs' 200 15 || die "takeover: settings not carried"
pass "12 takeover → master n2, group id moves to n2; name + settings carry over"

# The group id is now ID2 for the rest of the flow.
GID=$GID2

# n2 is master now; restart playback on the new master so both sinks subscribe.
post_retry "$N2/api/play" '{"uri":"file:tone.wav"}' || die "play on n2 (new master) kept failing"
wait_for "$N1/api/status" '.sink.played>0' true 8 || die "n1 not playing after takeover"
wait_for "$N2/api/status" '.sink.played>0' true 8 || die "n2 not playing after takeover"

# ---- 13. opus leg (only when both nodes report opus) ------------------------
o2=$(api "$N2/api/cluster" | jq -r --arg id "$ID2" '[.nodes[]|select(.id==$id).capabilities.codecs[]]|index("opus")!=null')
if [ "$o1" = true ] && [ "$o2" = true ]; then
  P1=$(api "$N1/api/status" | jq .sink.played)
  P2=$(api "$N2/api/status" | jq .sink.played)
  post "$N2/api/group/settings" '{"codec":"opus","transport":"udp","bufferMs":200}'
  wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID2"'").settings.codec' opus || die "codec not opus"
  sleep 1
  api "$N1/api/status" | jq -e '.sink.played > '"$P1" >/dev/null || die "n1 opus playback stalled"
  api "$N2/api/status" | jq -e '.sink.played > '"$P2" >/dev/null || die "n2 opus playback stalled"

  # ---- 13b. mid-session renegotiation: a member disables opus → live downgrade -
  # With the opus session PLAYING, disable opus on the member (n1). The master
  # (n2) detects the running codec is no longer supported by all members and
  # renegotiates the live session to pcm in place (bump gen, drop encoder,
  # RECONFIG). Both sinks must keep advancing and the playback record flips to
  # codec=pcm — no stop, no operator action. (D33 mid-session renegotiation.)
  patchj "$N1/api/node" '{"disabled":["opus"]}'
  wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$GID"'").playback.codec' pcm 8 \
    || die "session did not renegotiate to pcm after member disabled opus"
  rising "$N1/api/status" '.sink.played' 1 || die "n1 stalled after renegotiation"
  rising "$N2/api/status" '.sink.played' 1 || die "n2 stalled after renegotiation"
  pass "13b renegotiation: member disabled opus → live session downgraded to pcm, both rising"
  # Re-enable opus on n1 (no auto-upgrade mid-session; restores caps for teardown).
  patchj "$N1/api/node" '{"disabled":[]}'

  post "$N2/api/group/settings" '{"codec":"pcm","transport":"udp","bufferMs":200}'
  wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID2"'").settings.codec' pcm || die "codec not reset to pcm"
  pass "13 opus leg: both sinks play through opus; reset to pcm"
else
  pass "13 opus leg skipped (codecs opus not present on both: n1=$o1 n2=$o2)"
fi

# ---- 14. stop → playback idle on both ---------------------------------------
post "$N2/api/stop"
wait_for "$N1/api/cluster" '.groups[]|select(.id=="'"$GID"'").playback.state' idle || die "playback not idle after stop"
# Once the session ends the source goes idle: /api/status omits .source entirely
# (it is present only while a source is actively running, D19).
wait_for "$N2/api/status" '.source == null' true 8 || die "n2 source still active after stop"
pass "14 stop → playback idle on both; n2 source goes idle"

# ---- 15s. settings persistence (D47): custom settings survive a master restart
# n2 is master (group id == ID2, D42). Set a distinctive bufferMs on its group,
# confirm it lands on its OWN settings record (key == ID2) and is written to
# cluster.json, then kill + relaunch n2 against the SAME data dir. At boot n2
# loads its own settings record BEFORE gossip and re-forms its (solo) group, so
# /api/cluster must still show bufferMs 250 — loaded from disk, not the default.
post_retry "$N2/api/group/settings" '{"codec":"pcm","transport":"udp","bufferMs":250}' || die "settings POST (persist leg) failed"
wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$ID2"'").settings.bufferMs' 250 || die "bufferMs not 250 before restart"
sleep 3
[ -f "$DATA2/cluster.json" ] || die "n2 cluster.json not written (D47)"
grep -q 250 "$DATA2/cluster.json" || die "n2 cluster.json missing the settings bufferMs (D47)"
kill "$PID2" 2>/dev/null || true; wait "$PID2" 2>/dev/null || true
# n1 self-heals to solo while n2 is down.
wait_for "$N1/api/status" '.role' solo 20 || die "n1 did not go solo while n2 down (persist leg)"
# Relaunch n2 SAME data dir; it boots solo (master n1 absent / re-forms later).
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" --name n2 --host 127.0.0.1 \
       --http-port 28080 --stream-port 29090 --source-port 29200 --gossip-port 27946 \
       --no-mdns --join 127.0.0.1:17946 >"$LOG2" 2>&1 &  PID2=$!
wait_for "$N2/api/status" '.id|length' 32 30 || die "n2 did not restart (persist leg)"
# n2's solo group is keyed by its own id (ID2); its persisted settings record
# (key == ID2) loaded from cluster.json, so bufferMs is still 250 (not 150).
wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$ID2"'").settings.bufferMs' 250 20 \
  || die "n2 lost its group settings across restart (D47 persistence)"
wait_for "$N2/api/cluster" '[.nodes[]|select(.alive)]|length' 2 30 || die "n2 did not rejoin (persist leg)"
pass "15s settings persistence: bufferMs=250 survives n2 master restart (D47)"

# Re-form the n1+n2 group (n1 follows n2 master) so the next leg's GID group
# exists again — 15s left n1 self-healed to solo.
post_retry "$N1/api/follow" "{\"target\":\"$ID2\"}" || die "re-follow after persist leg failed"
wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$GID"'")|.members|length' 2 20 || die "group did not re-form after persist leg"

# ---- 15. group-name persistence (D41): name a group, restart n2, name known -
# Rename the group (the override is keyed by the n1+n2 member-set XOR), confirm
# n2 sees it, then kill + relaunch n2 against the SAME data dir. n2 reloads
# cluster.json (the override-NAMES map ONLY, D42) BEFORE rejoining gossip, so
# after rejoin + group re-form the name is still attached. (Pure persistence-vs-
# gossip isolation is covered by the cluster unit tests; here we assert the
# end-to-end "name survives a restart" guarantee.)
post "$N2/api/group/name" "{\"group\":\"$GID\",\"name\":\"persisted-room\"}"
wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$GID"'").name' persisted-room || die "n2 did not see group name"
# Confirm cluster.json was actually written on n2's data dir, then stop n2
# cleanly (SIGTERM → graceful Close force-saves too).
sleep 3
[ -f "$DATA2/cluster.json" ] || die "n2 cluster.json not written (D41)"
grep -q persisted-room "$DATA2/cluster.json" || die "n2 cluster.json missing the name (D41)"
kill "$PID2" 2>/dev/null || true
wait "$PID2" 2>/dev/null || true
# n1 self-heals to solo while n2 is down; the GID combination dissolves.
wait_for "$N1/api/status" '.role' solo 20 || die "n1 did not go solo while n2 down"
# Relaunch n2 against the SAME DATA2 dir + ports; rejoin via n1 seed.
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" --name n2 --host 127.0.0.1 \
       --http-port 28080 --stream-port 29090 --source-port 29200 --gossip-port 27946 \
       --no-mdns --join 127.0.0.1:17946 >"$LOG2" 2>&1 &  PID2=$!
wait_for "$N2/api/status" '.id|length' 32 30 || die "n2 did not restart"
wait_for "$N2/api/cluster" '[.nodes[]|select(.alive)]|length' 2 30 || die "n2 did not rejoin"
# Re-form the SAME group (n1 follows n2) so the GID combination reappears; the
# persisted name must attach to it.
post_retry "$N1/api/follow" "{\"target\":\"$ID2\"}" || die "re-follow failed"
wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$GID"'")|.members|length' 2 20 || die "group did not re-form"
wait_for "$N2/api/cluster" '.groups[]|select(.id=="'"$GID"'").name' persisted-room 20 \
  || die "n2 lost the group name across restart (D41 persistence)"
pass "15 group-name persistence: name survives n2 restart (D41)"

# ---- 15b. follow persistence: rejoin previous group on return (D45) ---------
# Re-form a clean group with n2 FOLLOWING n1 (master), then kill n2 and relaunch
# it against the SAME data dir. n2's node.json persisted following=ID1 at the
# follow, and InitialFollowing seeds its own record at boot, so the EXISTING
# group machinery re-forms n1's group within ~10s — no new rejoin logic.
post_retry "$N1/api/unfollow" '' || die "n1 unfollow failed"
wait_for "$N1/api/status" '.role' solo 15 || die "n1 not solo before rejoin setup"
post_retry "$N2/api/follow" "{\"target\":\"$ID1\"}" || die "n2 follow n1 failed"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'")|.members|length' 2 20 || die "n2 did not join n1 group"
# n2 persisted following=ID1 to its node.json (D45).
sleep 1
[ -f "$DATA2/node.json" ] || die "n2 node.json missing"
F2=$(jq -r '.following' "$DATA2/node.json")
[ "$F2" = "$ID1" ] || die "n2 node.json following=$F2 want $ID1 (D45 persist)"
# Kill n2 and relaunch SAME data dir; previous following restores it to n1's group.
kill "$PID2" 2>/dev/null || true
wait "$PID2" 2>/dev/null || true
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" --name n2 --host 127.0.0.1 \
       --http-port 28080 --stream-port 29090 --source-port 29200 --gossip-port 27946 \
       --no-mdns --join 127.0.0.1:17946 >"$LOG2" 2>&1 &  PID2=$!
wait_for "$N2/api/status" '.id|length' 32 30 || die "n2 did not restart (rejoin)"
wait_for "$N2/api/cluster" '.nodes[]|select(.id=="'"$ID2"'").following' "$ID1" 15 || die "n2 following not restored at boot"
wait_for "$N1/api/cluster" '.groups[]|select(.master=="'"$ID1"'")|.members|length' 2 15 || die "n2 did not rejoin n1 group on return"
wait_for "$N2/api/status" '.role' follower 15 || die "n2 not follower after rejoin"
pass "15b follow persistence: n2 rejoins n1's group on return (D45)"

# ---- 15c. solo fallback: master absent on return → self-heal clears (D45) ----
# Kill BOTH; relaunch ONLY n2 (master n1 absent). n2 boots seeded with
# following=ID1, but n1 never appears, so the §5 self-heal grace fires and resets
# n2 to solo — and clears its persisted following back to "".
kill "$PID1" 2>/dev/null || true; wait "$PID1" 2>/dev/null || true
kill "$PID2" 2>/dev/null || true; wait "$PID2" 2>/dev/null || true
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" --name n2 --host 127.0.0.1 \
       --http-port 28080 --stream-port 29090 --source-port 29200 --gossip-port 27946 \
       --no-mdns >"$LOG2" 2>&1 &  PID2=$!
wait_for "$N2/api/status" '.id|length' 32 30 || die "n2 did not restart (solo fallback)"
# Master n1 is gone; within the ~15s grace n2 self-heals to solo.
wait_for "$N2/api/status" '.role' solo 20 || die "n2 did not self-heal to solo (master absent)"
# Self-heal cleared its replicated following back to Zero (renders as 32 hex
# zeros in /api/cluster, id.ID marshaling) — i.e. no longer ID1.
ZERO=$(printf '0%.0s' $(seq 32))
wait_for "$N2/api/cluster" '.nodes[]|select(.id=="'"$ID2"'").following' "$ZERO" 20 || die "n2 following not cleared in cluster"
# And cleared the persisted following back to "" in node.json (D45).
sleep 1
F2=$(jq -r '.following' "$DATA2/node.json")
[ "$F2" = "" ] || die "n2 node.json following=$F2 want empty (self-heal clears, D45)"
pass "15c solo fallback: master absent → n2 self-heals to solo, following cleared (D45)"

# ---- 16. final: n2 (now solo) is the only node left ; n1 already gone --------
# n1 was killed in 15c; n2 self-healed to solo. Confirm n2 stands alone.
wait_for "$N2/api/status" '.role' solo 5 || die "n2 not solo at teardown"
pass "16 teardown: n2 solo after master (n1) departure"

echo
echo "e2e OK ($PASS passed, $FAIL failed)"
