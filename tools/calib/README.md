# Coherence measurement prototype (`tools/calib/`)

Piece 1 of the calibration feature (see `docs/calibration.md`): a standalone
Python prototype that proves we can measure the **acoustic arrival time** of a
sine-sweep burst from each player using a **single microphone**, with players
sounding **one at a time** (time-interleaved). The DSP here is the reference
implementation for the eventual Go port (`internal/calib`, Piece 2).

At 48 kHz, **1 sample = 20.83 µs = 7.1 mm** of sound travel. The honest target is
sample-level relative offset, sub-sample with sweep interpolation.

## Files

| File | Role |
|------|------|
| `sweep.py` | Generate the reference exponential sine sweep + Farina inverse filter. |
| `analyze.py` | Estimate per-burst arrival (matched filter **and** deconvolution), sub-sample refine, print the offset table. |
| `selftest.py` | Hardware-free DSP validation against known sub-sample offsets. |
| `requirements.txt` | `numpy`, `scipy`. |

## Setup

```bash
cd tools/calib
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

(`.venv/` and `*.wav` are gitignored — do not commit them.)

## Validate the DSP (no hardware)

```bash
.venv/bin/python selftest.py
```

Synthesizes a recording with three sweeps at **known sub-sample offsets**, plus
two room echoes and white noise at 30 dB SNR, then asserts the estimator
recovers each relative offset within 1 sample (it achieves < 0.02 sample).
Exits non-zero on failure.

## WAV formats

- **Reference sweep** (`sweep.py` output): 48 kHz, **mono, 32-bit float**
  (`WAVE_FORMAT_IEEE_FLOAT`). `--s16` writes mono s16le instead.
- **Recording** (`analyze.py` input): any rate (48 kHz expected), **mono or
  stereo**, PCM s16/s32/u8 or 32-bit float. Stereo is folded to mono by
  averaging channels — matching ensemble's stereo s16le mic capture path
  (`internal/audio/input.go RawCapture`).

## Manual lab procedure

1. **Generate the reference sweep:**
   ```bash
   .venv/bin/python sweep.py --out ref.wav
   ```
   Default: 100 Hz → 12 kHz exponential sweep over 1.0 s, 48 kHz, mono float,
   10 ms raised-cosine fades.

2. **Start one continuous recording** from a single mic at 48 kHz (any tool):
   ```bash
   arecord -f S16_LE -c 2 -r 48000 capture.wav     # or pw-record / interface tool
   ```

3. **Play the sweep on player A, then B** (interleaved, one at a time). For the
   prototype, drive playback by hand — drop `ref.wav` in as a file source, or use
   the existing test paths (`cmd/soundcheck/main.go`, `internal/sink/tone.go`).
   Emit on A, **settle ~1 s**, then emit on B. No new Go code in this piece.

4. **Stop recording**, note roughly which sample range each burst occupies.

5. **Analyze:**
   ```bash
   # auto-detect bursts by energy:
   .venv/bin/python analyze.py capture.wav ref.wav

   # or give explicit windows (more reliable) — start:end sample pairs:
   .venv/bin/python analyze.py capture.wav ref.wav \
       --windows 48000:110000,150000:212000 --labels A,B
   ```
   Prints per-player arrival (matched filter and deconvolution, which should
   agree to a fraction of a sample) and the inter-player offset in samples, µs,
   and ms relative to player A.

   > Prefer explicit `--windows` for precise work; the energy auto-detector is a
   > convenience and assumes a clear (~1 s) settle gap between bursts.

## Validation against the system's own delay control

This proves the acoustic measurement and ensemble's playout-delay control agree:

1. Run the measurement once with both players at default delay; record the A→B
   offset (call it `O0`).
2. In the UI, inject a known delay on player B by setting its **`outputDelayMs`**
   (the sink re-anchors playout via `SetDelayOffset`).
3. Re-run the measurement. The new offset should be `O0 + outputDelayMs`, within
   ~1–2 samples. E.g. injecting 5 ms should move B by `5 ms / 20.83 µs ≈ 240`
   samples.
4. Repeat N times to report the noise floor (mean ± stddev). The selftest shows
   the DSP's intrinsic error is well under a sample; real-world spread comes from
   the room, mic, and clock.

## Notes for the Go port (Piece 2)

- **Matched filter** = cross-correlation = `recording (*) reference[::-1]`
  (`scipy.signal.fftconvolve`, `mode="full"`). Peak index maps to arrival via the
  constant offset `len(ref) - 1`. In Go, an FFT-based linear convolution
  (zero-pad to ≥ `N+M-1`, multiply spectra, inverse FFT) reproduces this exactly.
- **Deconvolution** uses the same alignment offset; the Farina envelope changes
  amplitude shaping, not timing. Both estimators agree to < 0.05 sample in the
  selftest — porting just one (matched filter) is sufficient for timing; keep the
  IR path as a cross-check.
- **Parabolic interpolation** around the peak magnitude:
  `delta = 0.5*(y[-1]-y[+1]) / (y[-1]-2*y[0]+y[+1])`, true peak = `k + delta`.
  Guard the zero denominator (flat top) and the array edges.
- The sweep's large time-bandwidth product gives strong processing gain: the
  selftest still recovers offsets to < 0.02 sample at 10 dB SNR with echoes.
