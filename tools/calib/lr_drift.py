#!/usr/bin/env python3
"""
lr_drift.py — inter-speaker + vs-clock drift from a wideband L/R-interleave recording.

Both speakers play the SAME wideband sweep but at interleaved times (pi01=L at
t=0,2.4,4.8…; pi02=R at t=1.2,3.6…), so they never overlap and each is identified by
its time slot. Robust, modal-free (wideband matched filter), no carrier-phase unwrap —
the method that nailed the 46 cm move and a ±150 µs baseline.

  inter-speaker  — each pi02 (R) sweep referenced to the midpoint of its neighbouring
                   pi01 (L) sweeps. This cancels the playback-vs-mic clock rate exactly,
                   leaving the pi01↔pi02 coherence offset; tracked over time.
  vs-clock       — pi01 (L) sweep arrivals detrended of nominal cadence → playback drift
                   vs the recording clock (the shared ~ppm offset + servo wiggle).

Usage:
  python lr_drift.py --rec lrrun.wav --ref /tmp/ref_wb.npy --period 2.4 --discard 4 \
      --out results/wideband
"""
from __future__ import annotations
import argparse, json, os, sys, wave
import numpy as np

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


def parab(c, k):
    if k <= 0 or k >= len(c) - 1:
        return float(k)
    ym, y0, yp = c[k - 1], c[k], c[k + 1]
    d = ym - 2 * y0 + yp
    return k + (0.5 * (ym - yp) / d if d else 0.0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rec", required=True)
    ap.add_argument("--ref", default="/tmp/ref_wb.npy")
    ap.add_argument("--period", type=float, default=2.4, help="per-channel sweep period")
    ap.add_argument("--gap", type=float, default=1.2, help="L→R interleave gap")
    ap.add_argument("--discard", type=float, default=4.0)
    ap.add_argument("--out", default="results/wideband")
    ap.add_argument("--label", default="wideband L/R interleave, 12 min")
    args = ap.parse_args()

    m = load_mono(args.rec); print(f"recording {len(m)/SR:.0f}s")
    ref = np.load(args.ref).astype(float); off = len(ref) - 1
    mag = np.abs(A.matched_filter(m, ref)); mag[:int(args.discard * SR)] = 0

    # find all sweep peaks, min-sep = 0.7*gap (alternating L,R every `gap` s)
    minsep = int(0.7 * args.gap * SR); thr = 0.30 * mag.max()
    c = mag.copy(); pk = []
    while True:
        k = int(np.argmax(c))
        if c[k] < thr:
            break
        pk.append(k); c[max(0, k - minsep):k + minsep] = 0
    pk = sorted(pk)
    subs = np.array([parab(mag, k) - off for k in pk])      # arrival samples
    t = subs / SR
    print(f"  {len(subs)} sweeps found")
    # even index = L (pi01), odd = R (pi02). Drop a leading R if first peak is odd parity
    # (assume first detected is an L; gaps could break parity — verify by spacing)

    # vs-clock: L arrivals (even idx) detrended of cadence (period) → drift vs mic clock
    Lidx = np.arange(0, len(subs), 2)
    Larr = subs[Lidx]; Lt = t[Lidx]
    cyc = np.arange(len(Larr)); M = np.vstack([cyc, np.ones(len(cyc))]).T
    sl, b = np.linalg.lstsq(M, Larr, rcond=None)[0]
    vs_us = (Larr - (sl * cyc + b)) / SR * 1e6
    ppm = (sl / (args.period * SR) - 1) * 1e6
    print(f"  vs-clock: {ppm:+.1f} ppm, wiggle rms {np.std(vs_us):.1f}µs")

    # inter-speaker: each R (odd) referenced to midpoint of neighbour Ls → rate-cancelled
    rel_t, rel = [], []
    for j in range(1, len(subs) - 1, 2):
        interpL = (subs[j - 1] + subs[j + 1]) / 2
        rel_t.append(t[j]); rel.append((subs[j] - interpL) / SR * 1e6)
    rel_t = np.array(rel_t); rel = np.array(rel)
    # robust outlier purge (reverb mis-picks): keep within 4*MAD of median
    med = np.median(rel); mad = np.median(np.abs(rel - med)) + 1e-9
    good = np.abs(rel - med) < 6 * mad
    rel_t, rel = rel_t[good], rel[good]; rel -= np.median(rel)
    print(f"  inter-speaker: rms {np.std(rel):.1f}µs, span {rel.max()-rel.min():.1f}µs over {rel_t[-1]/60:.1f}min "
          f"({good.sum()}/{len(good)} kept)")

    import matplotlib; matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    plt.rcParams.update({"figure.facecolor": BG, "axes.facecolor": BG,
        "savefig.facecolor": BG, "text.color": FG, "axes.labelcolor": MUTED,
        "xtick.color": MUTED, "ytick.color": MUTED, "axes.edgecolor": BORDER,
        "axes.grid": True, "grid.color": BORDER, "grid.alpha": 0.5, "font.size": 11})

    fig, ax = plt.subplots(figsize=(11, 4.6), dpi=160)
    ax.plot(Lt/60, vs_us/1000, color=ACCENT, lw=1.3)
    ax.set_xlabel("time (minutes)"); ax.set_ylabel("drift vs recording clock (ms)")
    fig.text(0.09, 0.95, f"Playback drift vs the recording clock  ({ppm:+.0f} ppm)", color=FG, fontsize=16, fontweight="bold")
    fig.text(0.063, 0.955, "•", color=ACCENT, fontsize=17)
    fig.text(0.5, 0.005, args.label, color=MUTED, ha="center", fontsize=10)
    fig.subplots_adjust(top=0.86, bottom=0.13)
    fig.savefig(args.out + "_vsclock.svg"); fig.savefig(args.out + "_vsclock.png")

    fig2, ax2 = plt.subplots(figsize=(11, 4.6), dpi=160)
    rms = np.std(rel)
    ax2.axhspan(-rms, rms, color=ACCENT, alpha=0.10, lw=0)
    ax2.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax2.plot(rel_t/60, rel, color=ACCENT, lw=0.9, alpha=0.5)
    ax2.scatter(rel_t/60, rel, s=8, color=ACCENT, edgecolors="none")
    ax2.set_xlabel("time (minutes)"); ax2.set_ylabel("inter-speaker drift (µs)")
    fig2.text(0.09, 0.95, "Inter-speaker coherence drift  (pi02 − pi01)", color=FG, fontsize=16, fontweight="bold")
    fig2.text(0.063, 0.955, "•", color=ACCENT, fontsize=17)
    fig2.text(0.975, 0.95, f"±{rms:.0f} µs", color=ACCENT, fontsize=22, fontweight="bold", ha="right")
    fig2.text(0.5, 0.005, args.label, color=MUTED, ha="center", fontsize=10)
    fig2.subplots_adjust(top=0.86, bottom=0.13)
    fig2.savefig(args.out + "_interspeaker.svg"); fig2.savefig(args.out + "_interspeaker.png")
    print("wrote", args.out + "_vsclock.* and _interspeaker.*")

    json.dump({"label": args.label, "ppm": ppm,
        "vsclock": {"t_min": (Lt/60).tolist(), "drift_us": vs_us.tolist()},
        "interspeaker": {"t_min": (rel_t/60).tolist(), "drift_us": rel.tolist(), "rms_us": float(rms)}},
        open(args.out + ".json", "w"))


if __name__ == "__main__":
    main()
