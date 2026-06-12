#!/usr/bin/env python3
"""
plot_coherence.py — turn a long sweep recording into a coherence-stability graph.

Given a continuous microphone recording of an ensemble group playing a periodic
sine-sweep file (one sweep every `--cadence` seconds), this:

  1. matched-filters each sweep to a sub-sample acoustic arrival time,
  2. fits the best constant-rate clock (arrival = rate*index + offset),
  3. reports the residual as the per-sweep timing error in microseconds,
  4. renders a brand-styled graph (SVG + PNG) for the marketing site,
  5. dumps the raw points as JSON so the site can render them natively.

The residual-from-best-fit-rate IS the sync jitter: how far each speaker strays
from a perfectly steady clock. The fitted rate is the (constant) crystal offset
between the ensemble master clock and the measuring mic's ADC — not a defect.

Usage:
  python plot_coherence.py --rec run5min.wav --cadence 2.5 --discard 4 \
      --out results/coherence_5min --title "Acoustic sync stability"
"""
from __future__ import annotations
import argparse, json, os, sys, wave
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import analyze as A
import sweep as sweepmod

SR = 48_000

# ---- ensemble brand tokens (design/DESIGN.md, control app) ----
BG     = "#11151a"
PANEL  = "#1a212b"
FG     = "#e6edf3"
MUTED  = "#8b97a7"
ACCENT = "#35e3b3"
BORDER = "#2a3340"
DANGER = "#ff6b6b"


def load_mono(path: str) -> np.ndarray:
    w = wave.open(path, "rb")
    n, ch, sw = w.getnframes(), w.getnchannels(), w.getsampwidth()
    raw = w.readframes(n)
    w.close()
    dt = {1: np.uint8, 2: np.int16, 4: np.int32}[sw]
    x = np.frombuffer(raw, dtype=dt).astype(np.float64)
    if sw == 1:
        x = (x - 128) / 128.0
    else:
        x = x / float(np.iinfo(dt).max + 1)
    x = x.reshape(-1, ch)
    return x.mean(axis=1)


def parabolic(c: np.ndarray, k: int) -> float:
    if k <= 0 or k >= len(c) - 1:
        return float(k)
    ym, y0, yp = c[k - 1], c[k], c[k + 1]
    den = ym - 2 * y0 + yp
    return k + (0.5 * (ym - yp) / den if den else 0.0)


def find_arrivals(mag: np.ndarray, off: int, cadence_s: float, discard_s: float):
    """Locate each periodic sweep by stepping at the nominal cadence and taking
    the local matched-filter peak in a ±0.45*cadence window. Returns (idx, arr_samp,
    mag) arrays, dropping windows whose peak is too weak (missed sweep)."""
    cad = cadence_s * SR
    win = int(0.45 * cad)
    lo0 = int(discard_s * SR) + off
    # anchor: strongest peak in the first two cadences after discard
    a_lo, a_hi = lo0, min(len(mag), lo0 + int(2 * cad))
    anchor = a_lo + int(np.argmax(mag[a_lo:a_hi]))
    ks, arrs, mags = [], [], []
    k = 0
    while True:
        center = anchor + int(round(k * cad))
        if center - win >= len(mag):
            break
        a, b = max(0, center - win), min(len(mag), center + win)
        if b - a < 8:
            break
        local = a + int(np.argmax(mag[a:b]))
        sub = parabolic(mag, local)
        ks.append(k); arrs.append(sub - off); mags.append(mag[local])
        k += 1
    ks = np.array(ks); arrs = np.array(arrs); mags = np.array(mags)
    # drop weak windows (no sweep / dropout): mag < 0.4 * median
    if len(mags):
        keep = mags >= 0.4 * np.median(mags)
        ks, arrs, mags = ks[keep], arrs[keep], mags[keep]
    return ks, arrs, mags


def fit_rate(ks: np.ndarray, arrs: np.ndarray):
    """Robust linear fit arrivals = slope*k + b with one 4-sigma residual purge."""
    keep = np.ones(len(ks), bool)
    for _ in range(2):
        M = np.vstack([ks[keep], np.ones(keep.sum())]).T
        slope, b = np.linalg.lstsq(M, arrs[keep], rcond=None)[0]
        resid = arrs - (slope * ks + b)
        s = resid[keep].std()
        if s == 0:
            break
        nk = np.abs(resid) <= 4 * s
        if nk.sum() == keep.sum():
            keep = nk; break
        keep = nk
    return slope, b, resid, keep


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rec", required=True, help="recording WAV")
    ap.add_argument("--ref", default="/tmp/ref_sweep.npy",
                    help="reference sweep .npy (else regenerated at --sweep-dur)")
    ap.add_argument("--sweep-dur", type=float, default=0.8)
    ap.add_argument("--cadence", type=float, required=True, help="seconds between sweeps")
    ap.add_argument("--discard", type=float, default=3.0, help="seconds to skip at start")
    ap.add_argument("--out", default="results/coherence", help="output path prefix")
    ap.add_argument("--title", default="Acoustic sync stability")
    ap.add_argument("--subtitle", default="measured with a microphone")
    args = ap.parse_args()

    if os.path.exists(args.ref):
        ref = np.load(args.ref).astype(float)
    else:
        ref = sweepmod.generate_sweep(duration=args.sweep_dur, sample_rate=SR, amplitude=0.7)
    off = len(ref) - 1

    m = load_mono(args.rec)
    dur = len(m) / SR
    m[: int(args.discard * SR)] = 0.0
    mag = np.abs(A.matched_filter(m, ref))

    ks, arrs, mags = find_arrivals(mag, off, args.cadence, args.discard)
    if len(ks) < 4:
        print("ERROR: too few sweeps detected (%d)" % len(ks)); sys.exit(1)
    slope, b, resid, keep = fit_rate(ks, arrs)

    err_us = resid / SR * 1e6
    t_min = (arrs / SR) / 60.0
    rate_ppm = (slope / (args.cadence * SR) - 1.0) * 1e6
    kept = keep
    rms = err_us[kept].std()
    pk = np.abs(err_us[kept]).max()
    p95 = np.percentile(np.abs(err_us[kept]), 95)
    n = int(kept.sum())
    span_min = (t_min[kept].max() - t_min[kept].min())

    print(f"sweeps used: {n} (of {len(ks)} found) over {span_min:.1f} min")
    print(f"rate offset (ensemble vs mic clock): {rate_ppm:+.1f} ppm")
    print(f"RMS jitter: {rms:.2f} us | p95: {p95:.2f} us | peak: {pk:.2f} us")

    # ---------------- plot ----------------
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib import font_manager  # noqa

    plt.rcParams.update({
        "figure.facecolor": BG, "axes.facecolor": BG, "savefig.facecolor": BG,
        "text.color": FG, "axes.labelcolor": MUTED, "xtick.color": MUTED,
        "ytick.color": MUTED, "axes.edgecolor": BORDER, "font.size": 12,
        "axes.grid": True, "grid.color": BORDER, "grid.alpha": 0.5,
        "grid.linewidth": 0.6,
    })
    fig = plt.figure(figsize=(11, 5.2), dpi=160)
    gs = fig.add_gridspec(1, 2, width_ratios=[5.2, 1], wspace=0.04,
                          left=0.085, right=0.975, top=0.80, bottom=0.13)
    ax = fig.add_subplot(gs[0, 0]); hx = fig.add_subplot(gs[0, 1], sharey=ax)

    tk = t_min[kept]; ek = err_us[kept]
    # RMS band
    ax.axhspan(-rms, rms, color=ACCENT, alpha=0.10, lw=0, zorder=0)
    ax.axhline(0, color=MUTED, lw=0.8, alpha=0.7, zorder=1)
    ax.plot(tk, ek, color=ACCENT, lw=0.8, alpha=0.35, zorder=2)
    ax.scatter(tk, ek, s=14, color=ACCENT, edgecolors="none", alpha=0.9, zorder=3)
    ax.set_xlabel("time (minutes)")
    ax.set_ylabel("speaker timing error  (µs)")
    ylim = max(pk * 1.25, rms * 3, 5)
    ax.set_ylim(-ylim, ylim)
    ax.set_xlim(tk.min() - 0.05, tk.max() + 0.05)
    for sp in ("top", "right"):
        ax.spines[sp].set_visible(False)

    # marginal histogram
    hx.hist(ek, bins=30, orientation="horizontal", color=ACCENT, alpha=0.85)
    hx.axhline(0, color=MUTED, lw=0.8, alpha=0.7)
    hx.set_xticks([]); hx.tick_params(axis="y", labelleft=False)
    for sp in ("top", "right", "bottom"):
        hx.spines[sp].set_visible(False)
    hx.spines["left"].set_color(BORDER)

    # titles + headline stat
    fig.text(0.085, 0.93, args.title, color=FG, fontsize=20, fontweight="bold")
    fig.text(0.085, 0.875, args.subtitle, color=MUTED, fontsize=12)
    fig.text(0.975, 0.93, f"±{rms:.1f} µs", color=ACCENT, fontsize=26,
             fontweight="bold", ha="right")
    fig.text(0.975, 0.875,
             f"RMS over {n} sweeps · {span_min:.0f} min · {rate_ppm:+.0f} ppm",
             color=MUTED, fontsize=11, ha="right")
    # accent dot (brand mark nod)
    fig.text(0.063, 0.935, "•", color=ACCENT, fontsize=20)

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    fig.savefig(args.out + ".svg")
    fig.savefig(args.out + ".png")
    print("wrote", args.out + ".svg", "and .png")

    data = {
        "title": args.title, "subtitle": args.subtitle,
        "recording": os.path.basename(args.rec),
        "sample_rate": SR, "cadence_s": args.cadence,
        "sweeps_used": n, "span_minutes": round(span_min, 3),
        "rate_offset_ppm": round(rate_ppm, 2),
        "rms_jitter_us": round(rms, 3), "p95_us": round(p95, 3),
        "peak_us": round(pk, 3),
        "points": [{"t_min": round(float(t), 4), "err_us": round(float(e), 3)}
                   for t, e in zip(tk, ek)],
    }
    with open(args.out + ".json", "w") as f:
        json.dump(data, f, indent=1)
    print("wrote", args.out + ".json")


if __name__ == "__main__":
    main()
