#!/bin/bash
# Orchestrate one dual-tone capture: record mic + play file + poll playback stats
# at 1 Hz (with a PLAY marker so compare_drift.py can anchor t0). Run in background.
set -u
cd /home/rm/Git/gitlab.rand0m.me/share/ensemble
MASTER=http://192.168.71.63:8080
WAV=tools/calib/results/tones_run.wav
LOG=tools/calib/results/tones_stats.jsonl
DUR=615        # record seconds (10-min WAV + margin)
POLLS=610      # ~1 Hz polls

: > "$LOG"
# Recorder first so the onset is captured (WAV has 2 s lead silence for margin).
arecord -d "$DUR" -f S16_LE -c 2 -r 48000 "$WAV" 2>/tmp/arecord.err &
AREC=$!

# Trigger playback, then immediately drop the PLAY marker (t0 for analysis).
curl -s -X POST "$MASTER/api/play" -H 'Content-Type: application/json' \
     -d '{"uri":"file:calib_tones.wav"}' >/dev/null
echo "$(date +%s.%N) PLAY" >> "$LOG"

for i in $(seq 1 "$POLLS"); do
  echo "$(date +%s.%N) $(curl -s "$MASTER/api/playback/statuses")" >> "$LOG"
  sleep 1
done

wait "$AREC"
curl -s -X POST "$MASTER/api/stop" >/dev/null
echo "DONE wav=$WAV log=$LOG polls=$POLLS"
