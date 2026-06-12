#!/usr/bin/env python3
"""
octave_drift.py — inter-speaker + vs-clock drift from an octave-separated recording.

Two speakers play simultaneously in disjoint, non-harmonic, ABOVE-MODAL frequency
bands (via L/R routing): each repeats `tone — gap — sweep — gap`. A single mic records
both; we bandpass-split to separate the speakers, then track each speaker's drift by
the tone's carrier phase (fine) with the sweep arrival as a coarse cross-check.

Carrier phase is tracked by heterodyning each band's tone to baseband and unwrapping
the phase densely WITHIN each tone segment (per-sample change is tiny, so no slips),
stitching across the gaps (sub-half-cycle change). This needs the band ABOVE the room's
modal region (≳1 kHz) — at low frequencies standing waves corrupt the phase.

Outputs TWO graphs:
  <out>_vsclock      — each speaker's drift vs the mic's recording clock (the shared
                       ~ppm clock offset + servo wiggle). "drift against internal clock."
  <out>_interspeaker — high-band minus low-band drift: the common mic-clock drift
                       cancels, leaving the pi01↔pi02 coherence drift.

Usage:
  python octave_drift.py --rec octrun2.wav --lo-tone 1200 --hi-tone 3200 \
      --lo-band 1000,1600 --hi-band 2600,4200 --period 7.5 --discard 6 \
      --out results/octave2
"""
from __future__ import annotations
import argparse, json, os, sys, wave
import numpy as np
from scipy.signal import butter, sosfiltfilt

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import analyze as A

SR = 48000
BG, FG, MUTED, ACCENT, ACCENT2, BORDER = (
    "#11151a", "#e6edf3", "#8b97a7", "#35e3b3", "#5bc8ff", "#2a3340")


def load_mono(path):
    w = wave.open(path, "rb"); n, ch = w.getnframes(), w.getnchannels()
    x = np.frombuffer(w.readframes(n), dtype=np.int16).astype(float) / 32768
    w.close()
    return x.reshape(-1, ch).mean(1)


def bandpass(x, lo, hi):
    return sosfiltfilt(butter(6, [lo, hi], btype="band", fs=SR, output="sos"), x)


def fine_track(sig, tone_hz, anchor, period, tone_win, dt=0.25):
    """Carrier-phase drift (µs) vs a reference oscillator. Sampled every `dt` s but ONLY
    inside each cycle's tone window (anchor + k*period + [tone_win]) — this excludes the
    sweep (which sweeps THROUGH the tone freq and corrupts phase) and the silent gaps.
    Dense within-tone unwrap → robust against cycle slips."""
    t = np.arange(len(sig)) / SR
    bb = sig * np.exp(-2j * np.pi * tone_hz * t)
    sos = butter(4, tone_hz * 0.3, btype="low", fs=SR, output="sos")
    bb = sosfiltfilt(sos, bb.real) + 1j * sosfiltfilt(sos, bb.imag)
    amp = np.abs(bb)
    thresh = 0.3 * np.percentile(amp, 90)
    half = int(dt * SR) // 2
    lo, hi = tone_win
    idx = np.arange(half, len(sig) - half, 2 * half)
    tt, ph = [], []
    for i in idx:
        cyc_local = (t[i] - anchor) % period
        if not (lo <= cyc_local <= hi):
            continue                      # outside the tone window (sweep/gap)
        seg = bb[i - half:i + half]
        if np.abs(seg).mean() < thresh:
            continue
        tt.append(t[i]); ph.append(np.angle(seg.sum()))
    tt = np.array(tt); ph = np.unwrap(np.array(ph))
    drift_us = ph / (2 * np.pi * tone_hz) * 1e6
    return tt, drift_us


def coarse_track(sig, sweep_ref, period, discard):
    """Sweep arrival per cycle (µs drift, relative to best-fit cadence) — coarse check."""
    off = len(sweep_ref) - 1
    mag = np.abs(A.matched_filter(sig, sweep_ref)); mag[:int(discard * SR)] = 0
    step = period * SR; win = int(0.45 * step)
    a0 = int(discard * SR) + off
    anchor = a0 + int(np.argmax(mag[a0:min(len(mag), a0 + int(1.5 * step))]))
    ts, arr = [], []
    k = 0
    while True:
        c = anchor + int(round(k * step))
        if c - win >= len(mag):
            break
        a, b = max(0, c - win), min(len(mag), c + win)
        if b - a < 8:
            break
        loc = a + int(np.argmax(mag[a:b]))
        ts.append((loc - off) / SR); arr.append(loc - off); k += 1
    ts = np.array(ts); arr = np.array(arr, float)
    if len(arr) < 3:
        return ts, arr
    cyc = np.arange(len(arr)); M = np.vstack([cyc, np.ones(len(cyc))]).T
    sl, b = np.linalg.lstsq(M, arr, rcond=None)[0]
    return ts, (arr - (sl * cyc + b)) / SR * 1e6


def styled():
    import matplotlib; matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    plt.rcParams.update({"figure.facecolor": BG, "axes.facecolor": BG,
        "savefig.facecolor": BG, "text.color": FG, "axes.labelcolor": MUTED,
        "xtick.color": MUTED, "ytick.color": MUTED, "axes.edgecolor": BORDER,
        "axes.grid": True, "grid.color": BORDER, "grid.alpha": 0.5, "font.size": 11})
    return plt


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rec", required=True)
    ap.add_argument("--lo-sweep", default="/tmp/oct2_loS.npy")
    ap.add_argument("--hi-sweep", default="/tmp/oct2_hiS.npy")
    ap.add_argument("--lo-band", default="1000,1600")
    ap.add_argument("--hi-band", default="2600,4200")
    ap.add_argument("--lo-tone", type=float, default=1200.0)
    ap.add_argument("--hi-tone", type=float, default=3200.0)
    ap.add_argument("--period", type=float, default=7.5)
    ap.add_argument("--discard", type=float, default=6.0)
    ap.add_argument("--out", default="results/octave2")
    ap.add_argument("--label", default="octave-separated, 12 min")
    args = ap.parse_args()

    m = load_mono(args.rec); print(f"recording {len(m)/SR:.0f}s")
    lob = tuple(int(x) for x in args.lo_band.split(","))
    hib = tuple(int(x) for x in args.hi_band.split(","))
    lo = bandpass(m, *lob); hi = bandpass(m, *hib)

    # cycle grid from the sweep arrivals; sweep starts at cycle-local 5.0s in the unit
    # (tone[0,4] gap[4,5] sweep[5,6.5] gap[6.5,7.5]). tone window = [0.3, 3.7].
    SWEEP_LOCAL = 5.0; TONE_WIN = (0.3, 3.7)
    loS = np.load(args.lo_sweep).astype(float); hiS = np.load(args.hi_sweep).astype(float)
    ts_l, _ = coarse_track(lo, loS, args.period, args.discard)
    ts_h, _ = coarse_track(hi, hiS, args.period, args.discard)
    anchor_l = (ts_l[0] - SWEEP_LOCAL) if len(ts_l) else args.discard
    anchor_h = (ts_h[0] - SWEEP_LOCAL) if len(ts_h) else args.discard

    tl, dl = fine_track(lo, args.lo_tone, anchor_l, args.period, TONE_WIN)
    th, dh = fine_track(hi, args.hi_tone, anchor_h, args.period, TONE_WIN)
    dl -= dl[0]; dh -= dh[0]
    # fit ppm vs recording clock
    def ppm(t, d):
        s = np.polyfit(t, d, 1)[0]; return s  # µs per s == ppm
    print(f"  low  band: {len(tl)} pts, {ppm(tl,dl):+.1f} ppm vs mic, wiggle rms {np.std(dl-np.polyval(np.polyfit(tl,dl,1),tl)):.1f}µs")
    print(f"  high band: {len(th)} pts, {ppm(th,dh):+.1f} ppm vs mic, wiggle rms {np.std(dh-np.polyval(np.polyfit(th,dh,1),th)):.1f}µs")

    # inter-speaker: interp high onto low grid, subtract (common mic-clock drift cancels)
    dh_on_l = np.interp(tl, th, dh)
    rel = dh_on_l - dl; rel -= np.median(rel)
    print(f"  inter-speaker drift: rms {np.std(rel):.1f}µs, span {rel.max()-rel.min():.1f}µs over {tl[-1]/60:.1f}min")

    plt = styled()
    # ---- graph 1: vs recording clock ----
    fig, ax = plt.subplots(figsize=(11, 4.6), dpi=160)
    ax.plot(tl/60, dl/1000, color=ACCENT, lw=1.3, label=f"pi01 (low) {ppm(tl,dl):+.0f} ppm")
    ax.plot(th/60, dh/1000, color=ACCENT2, lw=1.3, label=f"pi02 (high) {ppm(th,dh):+.0f} ppm")
    ax.set_xlabel("time (minutes)"); ax.set_ylabel("drift vs recording clock (ms)")
    ax.legend(loc="best", frameon=False)
    fig.text(0.09, 0.95, "Playback drift vs the recording clock", color=FG, fontsize=17, fontweight="bold")
    fig.text(0.063, 0.955, "•", color=ACCENT, fontsize=17)
    fig.text(0.5, 0.005, args.label, color=MUTED, ha="center", fontsize=10)
    fig.subplots_adjust(top=0.86, bottom=0.13)
    fig.savefig(args.out + "_vsclock.svg"); fig.savefig(args.out + "_vsclock.png")

    # ---- graph 2: inter-speaker ----
    fig2, ax2 = plt.subplots(figsize=(11, 4.6), dpi=160)
    rms = np.std(rel)
    ax2.axhspan(-rms, rms, color=ACCENT, alpha=0.10, lw=0)
    ax2.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax2.plot(tl/60, rel, color=ACCENT, lw=1.3)
    ax2.set_xlabel("time (minutes)"); ax2.set_ylabel("inter-speaker drift (µs)")
    fig2.text(0.09, 0.95, "Inter-speaker coherence drift  (pi02 − pi01)", color=FG, fontsize=17, fontweight="bold")
    fig2.text(0.063, 0.955, "•", color=ACCENT, fontsize=17)
    fig2.text(0.975, 0.95, f"±{rms:.0f} µs", color=ACCENT, fontsize=22, fontweight="bold", ha="right")
    fig2.text(0.5, 0.005, args.label, color=MUTED, ha="center", fontsize=10)
    fig2.subplots_adjust(top=0.86, bottom=0.13)
    fig2.savefig(args.out + "_interspeaker.svg"); fig2.savefig(args.out + "_interspeaker.png")
    print("wrote", args.out + "_vsclock.* and _interspeaker.*")

    json.dump({"label": args.label,
        "vsclock": {"low": {"t_min": (tl/60).tolist(), "drift_us": dl.tolist(), "ppm": ppm(tl,dl)},
                    "high": {"t_min": (th/60).tolist(), "drift_us": dh.tolist(), "ppm": ppm(th,dh)}},
        "interspeaker": {"t_min": (tl/60).tolist(), "drift_us": rel.tolist(), "rms_us": float(rms)}},
        open(args.out + ".json", "w"))


if __name__ == "__main__":
    main()
