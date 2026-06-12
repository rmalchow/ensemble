#!/usr/bin/env python3
"""
selftest.py — hardware-free validation of the arrival-time DSP.

Synthesises a fake single-mic recording: it places several copies of the
reference sweep at KNOWN sub-sample offsets into a long, noise-padded buffer
(simulating time-interleaved bursts), adds a couple of attenuated echoes
(room reflections) and white noise at a realistic SNR, then runs analyze.py's
estimator and asserts it recovers the known offsets.

Pass criteria (per the spec's honest target):
  - matched-filter and IR arrival estimates recover each *relative* offset
    within ~1 sample, ideally < 0.2 sample with parabolic interpolation.

Run:  python selftest.py
Exits non-zero on failure.
"""

from __future__ import annotations

import sys

import numpy as np

import sweep as sweepmod
from analyze import analyze, SAMPLE_RATE


def fractional_delay(x: np.ndarray, delay: float) -> np.ndarray:
    """
    Delay a signal by a (possibly fractional) number of samples using a
    windowed-sinc fractional delay filter.

    We need sub-sample truth to test sub-sample recovery: integer shifts can't
    challenge the parabolic interpolator. A windowed sinc kernel centred at the
    fractional delay reconstructs the band-limited shifted copy faithfully.
    """
    n = len(x)
    int_d = int(np.floor(delay))
    frac = delay - int_d

    # 32-tap windowed sinc for the fractional part.
    half = 16
    taps = np.arange(-half, half + 1)
    sinc = np.sinc(taps - frac)
    window = np.hamming(len(taps))
    kernel = sinc * window
    kernel /= kernel.sum()

    filtered = np.convolve(x, kernel, mode="full")
    # The kernel centre is at index `half`; account for it plus the integer delay.
    out = np.zeros(n + int_d + len(x), dtype=np.float64)
    start = int_d + half
    seg = filtered[: n]
    out[start:start + len(seg)] = seg
    return out[:n + int_d + len(x)]


def synth_recording(
    reference: np.ndarray,
    placements: list[tuple[float, float]],
    total_len: int,
    snr_db: float = 30.0,
    echoes: list[tuple[int, float]] | None = None,
    seed: int = 1234,
) -> np.ndarray:
    """
    Build a synthetic recording.

    placements: list of (start_sample_float, amplitude) — each places a copy of
                the sweep (with fractional sub-sample delay) at that position.
    total_len:  output length in samples.
    snr_db:     white-noise SNR relative to the sweep RMS.
    echoes:     list of (delay_samples, gain) reflections added per placement.
    """
    rng = np.random.default_rng(seed)
    if echoes is None:
        # Two attenuated room reflections at non-integer-friendly delays.
        echoes = [(411, 0.35), (1303, 0.18)]

    buf = np.zeros(total_len, dtype=np.float64)

    for (start, amp) in placements:
        # Fractional-delay a fresh copy of the sweep, then add it (+ echoes) at start.
        delayed = fractional_delay(reference, start - np.floor(start))
        base = int(np.floor(start))

        def _add(sig, at, gain):
            seg = sig * gain
            lo = at
            hi = min(total_len, at + len(seg))
            if lo >= total_len or hi <= 0:
                return
            buf[max(0, lo):hi] += seg[max(0, -lo):hi - lo]

        _add(delayed, base, amp)
        for (ed, eg) in echoes:
            _add(delayed, base + ed, amp * eg)

    # Add white noise at the requested SNR (relative to sweep RMS).
    sig_rms = np.sqrt(np.mean(reference**2))
    noise_rms = sig_rms / (10.0 ** (snr_db / 20.0))
    buf += rng.normal(0.0, noise_rms, size=total_len)
    return buf


def main() -> int:
    fs = SAMPLE_RATE
    ref = sweepmod.generate_sweep(sample_rate=fs)
    ref_len = len(ref)

    # KNOWN truth placements (sub-sample). Player A is the reference; the rest are
    # offset from A by deliberately non-integer amounts to exercise interpolation.
    a_start = 24_000.0           # 0.5 s of leading silence/noise
    truth = {
        "A": a_start,
        "B": a_start + ref_len + 12_000 + 37.6,     # ~+37.6 samples vs ideal grid
        "C": a_start + 2 * (ref_len + 12_000) - 8.25,
    }
    amps = {"A": 0.7, "B": 0.55, "C": 0.62}

    placements = [(truth[k], amps[k]) for k in ("A", "B", "C")]
    total = int(truth["C"]) + ref_len + 24_000

    rec = synth_recording(ref, placements, total, snr_db=30.0)

    # Build search windows around each known burst (the operator would log these
    # roughly; here we derive generous windows from the truth ± slack).
    slack = 6_000
    windows = []
    labels = list(truth.keys())
    for k in labels:
        s = int(truth[k]) - slack
        e = int(truth[k]) + ref_len + slack
        windows.append((max(0, s), min(total, e)))

    results = analyze(rec, ref, windows, labels, sample_rate=fs)

    # True relative offsets vs A.
    true_off = {k: truth[k] - truth["A"] for k in labels}
    spp_us = 1e6 / fs

    print("\nselftest: synthetic interleaved sweeps (SNR 30 dB, 2 echoes)\n")
    print(f"  1 sample = {spp_us:.3f} µs;  reference player = A\n")
    header = (f"{'player':>6} | {'true off':>10} | {'meas(xcorr)':>11} | "
              f"{'err(xc)':>9} | {'meas(IR)':>10} | {'err(IR)':>9}")
    print(header)
    print("-" * len(header))

    ref0_xc = results[0].arrival_xcorr
    ref0_ir = results[0].arrival_ir

    max_err_xc = 0.0
    max_err_ir = 0.0
    failures = []
    TOL = 1.0          # hard pass: within 1 sample
    GOOD = 0.2         # informational: sub-0.2 sample

    for r in results:
        off_xc = r.arrival_xcorr - ref0_xc
        off_ir = r.arrival_ir - ref0_ir
        t = true_off[r.label]
        err_xc = off_xc - t
        err_ir = off_ir - t
        max_err_xc = max(max_err_xc, abs(err_xc))
        max_err_ir = max(max_err_ir, abs(err_ir))
        print(f"{r.label:>6} | {t:10.3f} | {off_xc:11.3f} | {err_xc:+9.4f} | "
              f"{off_ir:10.3f} | {err_ir:+9.4f}")
        # Player A is the reference; its self-offset is 0 by construction.
        if r.label == "A":
            continue
        if abs(err_xc) > TOL:
            failures.append(f"{r.label}: xcorr error {err_xc:+.4f} samp > {TOL}")
        if abs(err_ir) > TOL:
            failures.append(f"{r.label}: IR error {err_ir:+.4f} samp > {TOL}")

    print()
    print(f"  max |error| xcorr = {max_err_xc:.4f} samp ({max_err_xc * spp_us:.3f} µs)")
    print(f"  max |error| IR    = {max_err_ir:.4f} samp ({max_err_ir * spp_us:.3f} µs)")
    print()

    if failures:
        print("FAIL:")
        for f in failures:
            print(f"  - {f}")
        return 1

    note = ""
    if max_err_xc < GOOD and max_err_ir < GOOD:
        note = "  (both methods sub-0.2 sample — sub-sample interpolation verified)"
    print(f"PASS: all relative offsets recovered within {TOL} sample.{note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
