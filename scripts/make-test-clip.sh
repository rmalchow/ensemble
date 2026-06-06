#!/usr/bin/env bash
# Generate a phase-calibration clip: an audio-only structured signal for
# estimating the inter-node audio phase offset of a synchronized pair (D14 —
# Ensemble is audio-only, no video).
#
# The audio is a 1 s repeating pattern, IDENTICAL on L and R (so a stereo pair —
# node A=L, node B=R — emits the same reference on both nodes; record both and
# cross-correlate to get the offset), matching A.10b exactly:
#
#   0.000–0.001 s : full-scale CLICK  → sharp transient, unambiguous COARSE delay
#   0.100–0.400 s : 1 kHz sine burst  → phase-continuous; 1 ms period resolves the
#                                       residual sub-ms phase (FINE)
#   else          : silence           → isolates the click + shows the noise floor
#
# Output is lossless FLAC (or WAV) at the canonical 48 kHz / stereo, so the
# node's on-demand transcode to canonical FLAC is exact (no resample, no phase
# smear). ffmpeg is a DEV-host tool only — never required at build or runtime.
#
# Usage: scripts/make-test-clip.sh [OUTPUT.flac|.wav] [DURATION_S] [TONE_HZ]
set -euo pipefail
cd "$(dirname "$0")/.."

OUT="${1:-test.flac}"
DUR="${2:-30}"
HZ="${3:-1000}"

# Lossless codec by container: .wav -> pcm_s16le, anything else -> flac.
case "$OUT" in
  *.wav) CODEC=pcm_s16le ;;
  *)     CODEC=flac ;;
esac

# Per-channel expression (escaped for the lavfi filtergraph: , -> \, ).
EXPR="0.8*lt(mod(t\,1)\,0.001)+0.35*sin(2*PI*${HZ}*t)*between(mod(t\,1)\,0.10\,0.40)"

ffmpeg -y -hide_banner \
  -f lavfi -i "aevalsrc=exprs='${EXPR}|${EXPR}':s=48000:c=stereo:d=${DUR}" \
  -c:a "${CODEC}" \
  -shortest "${OUT}"

echo "wrote ${OUT}  (${DUR}s, ${HZ} Hz tone, 48 kHz stereo, click+tone every 1 s, audio-only)"
