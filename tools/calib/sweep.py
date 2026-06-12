#!/usr/bin/env python3
"""
sweep.py — reference signal generation for acoustic coherence measurement.

Generates a logarithmic (exponential) sine sweep ("chirp") and the matching
Farina inverse filter used for sweep deconvolution. This is the *reference
implementation* of the DSP: it is intentionally explicit and heavily commented
because Piece 2 will port this math to Go (internal/calib).

WAV format written by the CLI: 48 kHz, mono, **32-bit float** (PCM_FLOAT).
We choose float over s16le for the *reference* sweep so the windowed sweep and
its fades are represented exactly with no quantisation. The microphone capture
path (Piece 2) is stereo s16le; analyze.py handles that on the read side.

References:
  A. Farina, "Simultaneous measurement of impulse response and distortion with
  a swept-sine technique", AES 108th Convention, 2000.
"""

from __future__ import annotations

import argparse
import wave

import numpy as np

# ----------------------------------------------------------------------------
# Canonical constants. 48 kHz everywhere, matching ensemble's canonical audio.
# ----------------------------------------------------------------------------
SAMPLE_RATE = 48_000


def generate_sweep(
    f0: float = 100.0,
    f1: float = 12_000.0,
    duration: float = 1.0,
    sample_rate: int = SAMPLE_RATE,
    fade: float = 0.01,
    amplitude: float = 0.5,
) -> np.ndarray:
    """
    Generate an exponential (logarithmic) sine sweep from f0 to f1.

    The instantaneous frequency rises geometrically:
        f(t) = f0 * (f1/f0)**(t/T)

    The phase is the integral of 2*pi*f(t). For an exponential sweep the closed
    form is:
        phi(t) = 2*pi*f0 * T/ln(f1/f0) * ((f1/f0)**(t/T) - 1)
    and the sweep is x(t) = sin(phi(t)).

    A short raised-cosine (Hann) fade is applied at both ends to avoid the click
    that a hard start/stop discontinuity would produce (clicks smear the impulse
    response and add broadband energy that hurts the correlation peak).

    Returns a float64 mono array in [-amplitude, amplitude].
    """
    if f0 <= 0 or f1 <= 0:
        raise ValueError("frequencies must be positive")
    if f1 <= f0:
        raise ValueError("f1 must be greater than f0 for an upward sweep")

    n = int(round(duration * sample_rate))
    # Sample instants. endpoint=False keeps the sample grid uniform (t = k/fs)
    # so that the Go port can use the same integer-sample indexing.
    t = np.arange(n, dtype=np.float64) / sample_rate
    T = n / sample_rate  # exact duration in seconds for the integer sample count

    K = (2.0 * np.pi * f0 * T) / np.log(f1 / f0)
    L = np.log(f1 / f0) / T
    phase = K * (np.exp(L * t) - 1.0)
    sweep = np.sin(phase)

    # Raised-cosine fades. fade is in seconds; clamp to at most half the sweep.
    nf = int(round(fade * sample_rate))
    nf = max(0, min(nf, n // 2))
    if nf > 0:
        # Hann ramp 0->1 over nf samples.
        ramp = 0.5 * (1.0 - np.cos(np.pi * np.arange(nf) / nf))
        sweep[:nf] *= ramp
        sweep[-nf:] *= ramp[::-1]

    return (amplitude * sweep).astype(np.float64)


def inverse_filter(
    sweep: np.ndarray,
    f0: float = 100.0,
    f1: float = 12_000.0,
    sample_rate: int = SAMPLE_RATE,
) -> np.ndarray:
    """
    Build the Farina inverse filter for an exponential sweep.

    Deconvolution by the inverse filter recovers the system impulse response:
        h(t) = recording(t) (*) inv(t)              [linear convolution]
    where inv(t) is the time-reversed sweep with a +6 dB/octave amplitude
    envelope correction.

    Why the envelope: an exponential sweep spends *more time per octave* at low
    frequencies (it sweeps slowly low, fast high), so low frequencies carry more
    energy. The reversed sweep alone is only an approximate inverse; multiplying
    it by an amplitude ramp that rises +6 dB/oct (i.e. proportional to the
    instantaneous frequency) flattens the spectrum so that convolving sweep with
    its inverse yields a clean, near-ideal Dirac impulse (a "perfect" delta plus
    band-limiting at f0/f1).

    Construction:
      1. inv = reversed sweep.
      2. The instantaneous frequency at original time t is f(t)=f0*(f1/f0)**(t/T).
         After reversal, sample i of inv corresponds to original time (T - i/fs),
         so we apply a gain proportional to that instant's frequency.
         Equivalently the envelope is a geometric ramp from 1 (at the inverse's
         start, = sweep's end = high freq) down to f0/f1 (at the inverse's end).
      3. Normalise so that sweep (*) inv peaks at ~1.0 — convenient for reading
         the IR peak amplitude, not required for *timing*.

    Returns a float64 array, same length as `sweep`.
    """
    n = len(sweep)
    T = n / sample_rate

    inv = sweep[::-1].copy()

    # Amplitude envelope: +6 dB/octave == gain proportional to frequency.
    # Original instantaneous frequency over time:
    t = np.arange(n, dtype=np.float64) / sample_rate
    f_of_t = f0 * (f1 / f0) ** (t / T)
    # Reverse it to align with the reversed sweep, normalise to 1.0 at the high
    # end so the envelope is a pure ramp (overall scale handled below).
    env = (f_of_t / f1)[::-1]
    inv *= env

    # Normalise so the deconvolution peak is ~1.0. The peak of sweep (*) inv
    # equals the dot product of sweep with its (enveloped, reversed) self at the
    # alignment lag; scaling inv by 1/that makes the IR peak unity.
    peak = np.dot(sweep, inv[::-1])  # == full-overlap convolution centre value
    if peak != 0:
        inv = inv / peak

    return inv.astype(np.float64)


# ----------------------------------------------------------------------------
# WAV I/O helpers (stdlib `wave` only; float WAV written manually).
# ----------------------------------------------------------------------------
def write_wav_float32(path: str, data: np.ndarray, sample_rate: int = SAMPLE_RATE) -> None:
    """
    Write a mono 32-bit float WAV (IEEE float, format tag 3).

    Python's stdlib `wave` writes only PCM integer WAVs and hardcodes format
    tag 1, so we cannot use it for float. We emit a minimal WAVE container with a
    WAVE_FORMAT_IEEE_FLOAT (0x0003) fmt chunk by hand. soundfile/scipy read this
    fine; so does analyze.py's reader.
    """
    import struct

    audio = np.ascontiguousarray(data, dtype="<f4")
    nch = 1
    bits = 32
    byte_rate = sample_rate * nch * bits // 8
    block_align = nch * bits // 8
    raw = audio.tobytes()

    with open(path, "wb") as fh:
        fh.write(b"RIFF")
        fh.write(struct.pack("<I", 36 + len(raw)))
        fh.write(b"WAVE")
        fh.write(b"fmt ")
        fh.write(struct.pack("<I", 16))            # fmt chunk size
        fh.write(struct.pack("<H", 3))             # WAVE_FORMAT_IEEE_FLOAT
        fh.write(struct.pack("<H", nch))
        fh.write(struct.pack("<I", sample_rate))
        fh.write(struct.pack("<I", byte_rate))
        fh.write(struct.pack("<H", block_align))
        fh.write(struct.pack("<H", bits))
        fh.write(b"data")
        fh.write(struct.pack("<I", len(raw)))
        fh.write(raw)


def write_wav_s16(path: str, data: np.ndarray, sample_rate: int = SAMPLE_RATE) -> None:
    """Write a mono s16le WAV using stdlib `wave` (clips to [-1, 1])."""
    clipped = np.clip(data, -1.0, 1.0)
    pcm = (clipped * 32767.0).round().astype("<i2")
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(pcm.tobytes())


def main() -> None:
    ap = argparse.ArgumentParser(description="Generate reference sine sweep + inverse filter.")
    ap.add_argument("--out", default="ref.wav", help="output WAV path for the reference sweep")
    ap.add_argument("--inv-out", default=None,
                    help="optional output WAV path for the inverse filter")
    ap.add_argument("--f0", type=float, default=100.0, help="start frequency (Hz)")
    ap.add_argument("--f1", type=float, default=12_000.0, help="end frequency (Hz)")
    ap.add_argument("--dur", type=float, default=1.0, help="sweep duration (s)")
    ap.add_argument("--rate", type=int, default=SAMPLE_RATE, help="sample rate (Hz)")
    ap.add_argument("--fade", type=float, default=0.01, help="fade in/out length (s)")
    ap.add_argument("--amp", type=float, default=0.5, help="peak amplitude (0..1)")
    ap.add_argument("--s16", action="store_true",
                    help="write s16le instead of float32 (default float32)")
    args = ap.parse_args()

    sweep = generate_sweep(args.f0, args.f1, args.dur, args.rate, args.fade, args.amp)
    if args.s16:
        write_wav_s16(args.out, sweep, args.rate)
        fmt = "s16le"
    else:
        write_wav_float32(args.out, sweep, args.rate)
        fmt = "float32"
    print(f"wrote sweep: {args.out} ({len(sweep)} samples, {args.rate} Hz, mono, {fmt})")

    if args.inv_out:
        inv = inverse_filter(sweep, args.f0, args.f1, args.rate)
        # Inverse filter has values outside [-1,1] after normalisation; float only.
        write_wav_float32(args.inv_out, inv / (np.max(np.abs(inv)) or 1.0), args.rate)
        print(f"wrote inverse filter: {args.inv_out} (normalised for storage)")


if __name__ == "__main__":
    main()
