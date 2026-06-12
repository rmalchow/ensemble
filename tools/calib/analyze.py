#!/usr/bin/env python3
"""
analyze.py — arrival-time estimation for the coherence experiment.

Given one continuous recording (single mic) containing several time-interleaved
sweep bursts (player A, settle, player B, ...), estimate each burst's acoustic
arrival sample two independent ways and compare:

  (a) matched filter / cross-correlation against the reference sweep, and
  (b) Farina sweep deconvolution -> impulse response, peak picking.

Both are refined to sub-sample precision with parabolic interpolation around the
peak. The result is reported per player as an arrival time plus the inter-player
offset (relative to the first/reference player) in samples, µs, and ms.

This is the reference DSP for the Go port (Piece 2); the correlation and
interpolation math is written out explicitly and commented.

WAV input: any sample rate (48 kHz expected), mono or stereo. Stereo is folded
to mono by averaging channels (see read_wav). Integer PCM is scaled to float
[-1, 1]; float WAV is read as-is.
"""

from __future__ import annotations

import argparse
import struct
import wave
from dataclasses import dataclass

import numpy as np
from scipy.signal import fftconvolve

import sweep as sweepmod

SAMPLE_RATE = 48_000


# ----------------------------------------------------------------------------
# WAV reading (handles s16/s32 PCM and IEEE float, mono or stereo).
# ----------------------------------------------------------------------------
def read_wav(path: str) -> tuple[np.ndarray, int]:
    """
    Read a WAV into a mono float64 array in roughly [-1, 1], plus its rate.

    Stereo (or N-channel) input is summed/averaged to one channel — the mic
    capture path in ensemble produces stereo s16le, and a single physical mic
    duplicated across channels (or a true stereo capture) is fine to fold: we
    only need one acoustic arrival.

    Supports PCM s16le and s32le via stdlib `wave`, and 32-bit IEEE float by
    parsing the container directly (stdlib `wave` rejects format tag 3).
    """
    # First, sniff the format tag from the fmt chunk.
    fmt_tag, nch, rate, bits = _probe_fmt(path)

    if fmt_tag == 3:  # IEEE float
        data, rate = _read_float_wav(path)
    else:  # PCM integer
        with wave.open(path, "rb") as w:
            nch = w.getnchannels()
            rate = w.getframerate()
            sw = w.getsampwidth()
            frames = w.readframes(w.getnframes())
        if sw == 2:
            arr = np.frombuffer(frames, dtype="<i2").astype(np.float64) / 32768.0
        elif sw == 4:
            arr = np.frombuffer(frames, dtype="<i4").astype(np.float64) / 2147483648.0
        elif sw == 1:
            # unsigned 8-bit
            arr = (np.frombuffer(frames, dtype="<u1").astype(np.float64) - 128.0) / 128.0
        else:
            raise ValueError(f"unsupported PCM sample width: {sw} bytes")
        data = arr.reshape(-1, nch) if nch > 1 else arr

    if data.ndim == 2:
        data = data.mean(axis=1)  # fold to mono
    return data.astype(np.float64), rate


def _probe_fmt(path: str) -> tuple[int, int, int, int]:
    """Return (format_tag, channels, sample_rate, bits_per_sample) from fmt chunk."""
    with open(path, "rb") as fh:
        riff = fh.read(12)
        if riff[:4] != b"RIFF" or riff[8:12] != b"WAVE":
            raise ValueError("not a RIFF/WAVE file")
        while True:
            hdr = fh.read(8)
            if len(hdr) < 8:
                raise ValueError("no fmt chunk found")
            cid, csize = struct.unpack("<4sI", hdr)
            body = fh.read(csize)
            if cid == b"fmt ":
                tag, nch, rate, _br, _ba, bits = struct.unpack("<HHIIHH", body[:16])
                return tag, nch, rate, bits
            if csize % 2 == 1:
                fh.read(1)  # chunks are word-aligned


def _read_float_wav(path: str) -> tuple[np.ndarray, int]:
    """Read a 32-bit IEEE float WAV (mono or interleaved) -> (array, rate)."""
    with open(path, "rb") as fh:
        fh.read(12)
        nch = rate = None
        data = None
        while True:
            hdr = fh.read(8)
            if len(hdr) < 8:
                break
            cid, csize = struct.unpack("<4sI", hdr)
            body = fh.read(csize)
            if cid == b"fmt ":
                _tag, nch, rate, _br, _ba, _bits = struct.unpack("<HHIIHH", body[:16])
            elif cid == b"data":
                data = np.frombuffer(body, dtype="<f4").astype(np.float64)
            if csize % 2 == 1:
                fh.read(1)
    if data is None or rate is None:
        raise ValueError("missing fmt/data chunk")
    if nch and nch > 1:
        data = data.reshape(-1, nch)
    return data, rate


# ----------------------------------------------------------------------------
# Core DSP.
# ----------------------------------------------------------------------------
def parabolic_interp(y_m1: float, y_0: float, y_p1: float) -> float:
    """
    Parabolic (quadratic) peak interpolation.

    Given three equally-spaced samples straddling a discrete peak — y[-1], y[0],
    y[+1] with y[0] the largest — fit a parabola and return the sub-sample offset
    delta of the true peak relative to the centre sample (in [-0.5, +0.5]):

        delta = 0.5 * (y[-1] - y[+1]) / (y[-1] - 2*y[0] + y[+1])

    The true peak index is then (peak_index + delta). This is the standard
    3-point vertex formula; the denominator is the discrete second derivative
    (negative at a maximum), guarded against zero (flat top).
    """
    denom = (y_m1 - 2.0 * y_0 + y_p1)
    if denom == 0:
        return 0.0
    return 0.5 * (y_m1 - y_p1) / denom


def _peak_subsample(sig: np.ndarray, search_lo: int = 0, search_hi: int | None = None) -> float:
    """Find the max-magnitude sample in [search_lo, search_hi) and refine it."""
    hi = len(sig) if search_hi is None else min(search_hi, len(sig))
    lo = max(0, search_lo)
    seg = np.abs(sig[lo:hi])
    if len(seg) == 0:
        return float("nan")
    k = int(np.argmax(seg)) + lo
    # Parabolic interp needs both neighbours; clamp at edges.
    if k <= 0 or k >= len(sig) - 1:
        return float(k)
    delta = parabolic_interp(abs(sig[k - 1]), abs(sig[k]), abs(sig[k + 1]))
    return float(k) + delta


def matched_filter(recording: np.ndarray, reference: np.ndarray) -> np.ndarray:
    """
    Cross-correlation of recording with the reference sweep (matched filter).

    Cross-correlation r[lag] = sum_n recording[n] * reference[n - lag] is
    computed efficiently as the convolution of `recording` with the
    time-reversed `reference`:
        xcorr(recording, reference) = recording (*) reference[::-1]
    Implemented with fftconvolve (mode="full"). The output index where the peak
    occurs corresponds to the lag at which the reference best aligns with the
    recording — i.e. the arrival sample of the sweep's *start*, after correcting
    for the convolution's offset.

    With mode="full" of len(rec)+len(ref)-1, lag 0 (reference aligned to start of
    recording) lands at output index len(ref)-1. So:
        arrival_start_sample = peak_index - (len(ref) - 1)
    We return the full correlation; callers map peak index to arrival via that
    constant offset (see estimate_arrival).
    """
    return fftconvolve(recording, reference[::-1], mode="full")


def deconvolve_ir(recording: np.ndarray, inv: np.ndarray) -> np.ndarray:
    """
    Farina deconvolution: convolve the recording with the inverse filter to get
    the impulse response. The IR peak marks the arrival of the sweep.

    h = recording (*) inv   (linear convolution, mode="full")

    The inverse filter `inv` is the enveloped time-reversed sweep from
    sweep.inverse_filter(). Because inv is essentially reference[::-1] with a
    spectral-flattening envelope, the IR-peak index maps to the arrival sample
    with the *same* constant offset (len(ref)-1) as the matched filter — the
    envelope changes amplitude shaping, not the alignment lag. estimate_arrival
    applies that offset uniformly.
    """
    return fftconvolve(recording, inv, mode="full")


@dataclass
class BurstResult:
    label: str
    window: tuple[int, int]
    arrival_xcorr: float       # sub-sample arrival via matched filter
    arrival_ir: float          # sub-sample arrival via deconvolution
    peak_xcorr: float          # correlation peak magnitude (quality indicator)
    peak_ir: float


def estimate_arrival(
    recording: np.ndarray,
    reference: np.ndarray,
    window: tuple[int, int] | None = None,
    inv: np.ndarray | None = None,
    method: str = "xcorr",
) -> float:
    """
    Estimate the sub-sample arrival index of the reference sweep within
    `recording`, optionally restricted to a [start, end) sample window.

    method = "xcorr" -> matched filter (default)
    method = "ir"    -> Farina deconvolution (requires `inv`, else built here)

    Returns the sub-sample arrival of the sweep's *first sample* in the
    recording's own sample index. This is the importable single-value entry
    point the spec asks for.
    """
    ref_len = len(reference)
    offset = ref_len - 1  # full-convolution alignment constant (see matched_filter)

    if method == "ir":
        if inv is None:
            inv = sweepmod.inverse_filter(reference)
        corr = deconvolve_ir(recording, inv)
    else:
        corr = matched_filter(recording, reference)

    # Restrict the peak search to the requested window (mapped into corr index
    # space by adding the alignment offset). This keeps interleaved bursts from
    # cross-triggering on each other.
    if window is not None:
        lo = window[0] + offset
        hi = window[1] + offset
    else:
        lo, hi = 0, len(corr)

    peak = _peak_subsample(corr, lo, hi)
    return peak - offset


def autodetect_bursts(
    recording: np.ndarray,
    ref_len: int,
    threshold_db: float = -25.0,
    min_gap: int | None = None,
) -> list[tuple[int, int]]:
    """
    Crude energy-based burst detector for when explicit windows aren't supplied.

    Computes a short-time energy envelope, thresholds it relative to the peak,
    and groups contiguous above-threshold regions into windows. Each window is
    padded to at least one reference length. Returns [(start, end), ...].

    This is a convenience for the manual lab flow; for precise work, pass
    explicit windows (the operator knows roughly when each burst played).
    """
    if min_gap is None:
        # Bridge only short intra-burst dropouts; a real inter-burst settle gap
        # (spec: ~1 s) is far larger than this and will correctly split bursts.
        min_gap = ref_len // 10

    win = max(1, ref_len // 50)  # ~20 ms envelope smoothing for a 1 s sweep
    env = np.sqrt(np.convolve(recording**2, np.ones(win) / win, mode="same"))
    peak = env.max() if env.size else 0.0
    if peak <= 0:
        return []
    thr = peak * (10.0 ** (threshold_db / 20.0))
    active = env > thr

    bursts: list[tuple[int, int]] = []
    i = 0
    n = len(active)
    while i < n:
        if active[i]:
            j = i
            last = i
            # Extend while still active, OR while inside a short gap measured from
            # the *last active* sample (so a long silence between interleaved
            # bursts ends the current window instead of bridging into the next).
            while j < n and (active[j] or (j - last < min_gap)):
                if active[j]:
                    last = j
                j += 1
            # `i..last` already spans the sweep's active energy; pad only by the
            # smoothing window plus a small margin, not another full ref_len
            # (which would overrun into the next interleaved burst).
            start = max(0, i - win)
            end = min(n, last + 2 * win)
            bursts.append((start, end))
            i = max(end, last + 1)
        else:
            i += 1
    return bursts


def analyze(
    recording: np.ndarray,
    reference: np.ndarray,
    windows: list[tuple[int, int]] | None,
    labels: list[str] | None = None,
    sample_rate: int = SAMPLE_RATE,
) -> list[BurstResult]:
    """Run both estimators for every burst window and return per-burst results."""
    inv = sweepmod.inverse_filter(reference)
    ref_len = len(reference)

    if windows is None:
        windows = autodetect_bursts(recording, ref_len)
    if labels is None:
        labels = [chr(ord("A") + i) for i in range(len(windows))]

    offset = ref_len - 1
    xc = matched_filter(recording, reference)
    ir = deconvolve_ir(recording, inv)

    results: list[BurstResult] = []
    for (w, label) in zip(windows, labels):
        lo, hi = w[0] + offset, w[1] + offset
        ax = _peak_subsample(xc, lo, hi)
        ai = _peak_subsample(ir, lo, hi)
        # peak magnitudes for a quality read-out
        px = float(np.max(np.abs(xc[max(0, lo):min(len(xc), hi)])))
        pi = float(np.max(np.abs(ir[max(0, lo):min(len(ir), hi)])))
        results.append(BurstResult(
            label=label,
            window=w,
            arrival_xcorr=ax - offset,
            arrival_ir=ai - offset,
            peak_xcorr=px,
            peak_ir=pi,
        ))
    return results


def print_report(results: list[BurstResult], sample_rate: int = SAMPLE_RATE) -> None:
    """Print a clean per-player table with inter-player offsets vs the first."""
    if not results:
        print("no bursts found.")
        return

    spp_us = 1e6 / sample_rate  # microseconds per sample
    ref0 = results[0].arrival_xcorr

    print()
    print(f"  sample rate: {sample_rate} Hz   (1 sample = {spp_us:.3f} µs)")
    print()
    hdr = (f"{'player':>6} | {'arrival(xcorr)':>15} | {'arrival(IR)':>13} | "
           f"{'xc-IR Δ':>8} | {'offset vs A':>26}")
    print(hdr)
    print("-" * len(hdr))
    for r in results:
        d_methods = r.arrival_xcorr - r.arrival_ir
        off_samp = r.arrival_xcorr - ref0
        off_us = off_samp * spp_us
        off_ms = off_us / 1000.0
        off_str = f"{off_samp:+9.3f} smp {off_us:+9.1f} µs {off_ms:+7.3f} ms"
        print(f"{r.label:>6} | {r.arrival_xcorr:15.3f} | {r.arrival_ir:13.3f} | "
              f"{d_methods:+8.3f} | {off_str:>26}")
    print()


def _parse_windows(spec: str | None) -> list[tuple[int, int]] | None:
    """Parse '--windows 1000:50000,120000:170000' into [(1000,50000),...]."""
    if not spec:
        return None
    out = []
    for part in spec.split(","):
        a, b = part.split(":")
        out.append((int(a), int(b)))
    return out


def main() -> None:
    ap = argparse.ArgumentParser(description="Estimate sweep arrival times in a recording.")
    ap.add_argument("recording", help="recording WAV (mono or stereo)")
    ap.add_argument("reference", help="reference sweep WAV (from sweep.py)")
    ap.add_argument("--windows", default=None,
                    help="comma list of start:end sample windows, e.g. 1000:50000,120000:170000")
    ap.add_argument("--labels", default=None,
                    help="comma list of player labels matching the windows, e.g. A,B")
    args = ap.parse_args()

    rec, rate = read_wav(args.recording)
    ref, rref = read_wav(args.reference)
    if rref != rate:
        print(f"WARNING: recording rate {rate} != reference rate {rref}; "
              f"results assume {rate} Hz")

    windows = _parse_windows(args.windows)
    labels = args.labels.split(",") if args.labels else None

    results = analyze(rec, ref, windows, labels, sample_rate=rate)
    print_report(results, sample_rate=rate)


if __name__ == "__main__":
    main()
