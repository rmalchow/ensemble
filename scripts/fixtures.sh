#!/usr/bin/env bash
# Regenerate testdata/media/tone.wav: a 5 s 440 Hz stereo 48 kHz s16le tone.
# Used by the e2e play assertions; long enough that a late join mid-play still
# has frames whose pts+bufferMs deadline is future (burst prime has something).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/testdata/media/tone.wav"
mkdir -p "$(dirname "$OUT")"

python3 - "$OUT" <<'PY'
import struct, sys, math, wave

path = sys.argv[1]
rate, secs, freq, amp = 48000, 5, 440.0, 0.3
n = rate * secs
w = wave.open(path, "wb")
w.setnchannels(2)
w.setsampwidth(2)
w.setframerate(rate)
frames = bytearray()
for i in range(n):
    v = int(amp * 32767 * math.sin(2 * math.pi * freq * i / rate))
    frames += struct.pack("<hh", v, v)
w.writeframes(bytes(frames))
w.close()
print("wrote", path, n, "frames")
PY
