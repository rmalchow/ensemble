#!/usr/bin/env python3
"""
graph_servo_ppm.py — the servo's COMMANDED ppm vs the acoustically MEASURED
inter-speaker offset and its RATE.

The point is dimensional: ppm is a RATE (1 ppm = 1 µs/s). So the servo's commanded
ppm is directly comparable to the time-derivative of the measured inter-speaker
offset — NOT to the offset itself (a position). Three panels:

  1. Commanded ppm per node (pi01, pi02) + their difference (pi02−pi01). The
     servo's rate order; the difference is the RELATIVE rate it pushes between
     the two speakers.
  2. Measured acoustic inter-speaker offset (R−L), robustly de-spiked, with its
     linear trend. A position (µs).
  3. The offset RATE — d(offset)/dt in µs/s ≡ ppm — overlaid on the commanded
     ppm difference. If the servos fully cancel each DAC's drift, the residual
     measured rate sits near 0 even while each node commands tens of ppm; if a
     speaker's drift is going uncorrected, the measured rate tracks the commanded
     difference instead.

Inputs mirror analyze_servo.py (--wav, --stats-log, --pi-low, --pi-high, --out).
"""
from __future__ import annotations
import argparse, json
import numpy as np
import tones

SR = 48_000
BG, FG, MUTED, ACCENT, ACCENT2, ACCENT3, BORDER = (
    "#11151a", "#e6edf3", "#8b97a7", "#35e3b3", "#5bc8ff", "#ffb454", "#2a3340")


def parse_stats(path, pi_low, pi_high):
    t0 = None
    t, ppm_lo, ppm_hi = [], [], []
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        sp = line.split(" ", 1)
        if len(sp) < 2:
            continue
        ts, rest = float(sp[0]), sp[1]
        if rest == "PLAY":
            t0 = ts
            continue
        if t0 is None:
            continue
        try:
            arr = json.loads(rest)
        except Exception:
            continue
        d = {n["nodeId"]: n for n in arr}
        lo, hi = d.get(pi_low), d.get(pi_high)
        if not lo or not hi or not (lo.get("synced") and hi.get("synced")):
            continue
        t.append((ts - t0) / 60)
        ppm_lo.append(lo["ratePPM"]); ppm_hi.append(hi["ratePPM"])
    return np.array(t), np.array(ppm_lo), np.array(ppm_hi)


def despike(off, tm, k=5.0):
    """Keep edges within k*MAD of the median — drops the ±0.2 s mispaired edges."""
    med = np.median(off)
    mad = np.median(np.abs(off - med)) * 1.4826
    keep = np.abs(off - med) < k * mad
    return off[keep], tm[keep]


def bin_median(tm, off, nbins):
    """Robust offset(t): median within each of nbins equal-time bins."""
    edges = np.linspace(tm.min(), tm.max(), nbins + 1)
    ctr, val = [], []
    for i in range(nbins):
        m = (tm >= edges[i]) & (tm < edges[i + 1] if i < nbins - 1 else tm <= edges[i + 1])
        if m.sum() >= 3:
            ctr.append(0.5 * (edges[i] + edges[i + 1]))
            val.append(np.median(off[m]))
    return np.array(ctr), np.array(val)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--wav", required=True)
    ap.add_argument("--stats-log", required=True)
    ap.add_argument("--pi-low", required=True)
    ap.add_argument("--pi-high", required=True)
    ap.add_argument("--out", default="results/servo_ppm")
    ap.add_argument("--label", default="commanded ppm vs measured inter-speaker offset & rate, 10 min")
    args = ap.parse_args()

    t, ppm_lo, ppm_hi = parse_stats(args.stats_log, args.pi_low, args.pi_high)
    ppm_diff = ppm_hi - ppm_lo

    off_raw, tm_raw = tones.analyze(tones.read_wav_stereo(args.wav))
    off, tm = despike(off_raw, tm_raw)
    # robust offset(t) on a 20 s grid, then its rate (µs/s = ppm)
    nb = max(6, int((tm.max() - tm.min()) * 60 / 20))
    tb, ob = bin_median(tm, off, nb)
    tb_s = tb * 60
    rate = np.gradient(ob, tb_s)  # µs/s ≡ ppm
    # headline linear drift over the run
    slope_us_per_min = np.polyfit(tm, off, 1)[0]
    drift_ppm = slope_us_per_min / 60.0  # µs/min → µs/s = ppm

    print(f"edges kept {len(off)}/{len(off_raw)} | polls {len(t)}")
    print(f"offset baseline (median): {np.median(off):+.0f} µs   spread (std): {np.std(off):.0f} µs")
    print(f"linear inter-speaker drift: {slope_us_per_min:+.1f} µs/min = {drift_ppm:+.2f} ppm")
    print(f"commanded ppm: pi01 {ppm_lo.mean():+.1f} (σ{ppm_lo.std():.1f})  "
          f"pi02 {ppm_hi.mean():+.1f} (σ{ppm_hi.std():.1f})  diff {ppm_diff.mean():+.1f}")
    print(f"measured offset-rate: mean {np.mean(rate):+.2f} ppm  |median commanded diff| {np.median(np.abs(ppm_diff)):.1f} ppm")

    import matplotlib; matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    plt.rcParams.update({"figure.facecolor": BG, "axes.facecolor": BG,
        "savefig.facecolor": BG, "text.color": FG, "axes.labelcolor": MUTED,
        "xtick.color": MUTED, "ytick.color": MUTED, "axes.edgecolor": BORDER,
        "axes.grid": True, "grid.color": BORDER, "grid.alpha": 0.5, "font.size": 10})
    fig, (ax1, ax2, ax3) = plt.subplots(3, 1, figsize=(11, 10), dpi=160, sharex=True)

    ax1.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax1.plot(t, ppm_lo, color=ACCENT, lw=1.5, label="pi01 commanded ppm")
    ax1.plot(t, ppm_hi, color=ACCENT2, lw=1.5, label="pi02 commanded ppm")
    ax1.plot(t, ppm_diff, color=FG, lw=1.2, alpha=0.8, label="difference (pi02−pi01)")
    ax1.set_ylabel("commanded rate (ppm)")
    ax1.legend(loc="best", frameon=False, ncol=3, fontsize=8)
    ax1.set_title("Servo commanded ppm (the rate order)", color=FG, fontsize=12)

    ax2.scatter(tm, off, s=4, color=ACCENT3, alpha=0.25, label="per-burst (de-spiked)")
    ax2.plot(tb, ob, color=ACCENT3, lw=1.8, label="20 s median")
    ax2.plot(tm, np.polyval(np.polyfit(tm, off, 1), tm), color=FG, lw=1.2, ls="--",
             label=f"trend {slope_us_per_min:+.1f} µs/min ({drift_ppm:+.2f} ppm)")
    ax2.set_ylabel("acoustic offset R−L (µs)")
    ax2.legend(loc="best", frameon=False, fontsize=8)
    ax2.set_title("Measured inter-speaker offset (a position)", color=FG, fontsize=12)

    ax3.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax3.plot(tb, rate, color=ACCENT3, lw=1.8, label="measured offset rate  d(offset)/dt  (µs/s ≡ ppm)")
    ax3.plot(t, ppm_diff, color=FG, lw=1.2, alpha=0.8, label="commanded ppm difference (pi02−pi01)")
    ax3.axhline(drift_ppm, color=ACCENT, lw=1.0, ls=":", label=f"net drift {drift_ppm:+.2f} ppm")
    ax3.set_xlabel("time (minutes)"); ax3.set_ylabel("rate (ppm = µs/s)")
    ax3.legend(loc="best", frameon=False, fontsize=8)
    ax3.set_title("Rate vs rate: measured drift rate vs commanded ppm (same units)", color=FG, fontsize=12)

    fig.text(0.5, 0.004, args.label, color=MUTED, ha="center", fontsize=9)
    fig.subplots_adjust(top=0.95, bottom=0.06, hspace=0.22)
    fig.savefig(args.out + ".svg"); fig.savefig(args.out + ".png")
    print("wrote", args.out + ".svg/.png")
    json.dump({
        "drift_ppm": float(drift_ppm), "slope_us_per_min": float(slope_us_per_min),
        "offset_baseline_us": float(np.median(off)), "offset_std_us": float(np.std(off)),
        "commanded_ppm": {"pi01_mean": float(ppm_lo.mean()), "pi02_mean": float(ppm_hi.mean()),
                          "diff_mean": float(ppm_diff.mean())},
        "t_min": t.tolist(), "ppm_lo": ppm_lo.tolist(), "ppm_hi": ppm_hi.tolist(),
        "offset": {"t_min": tb.tolist(), "us": ob.tolist(), "rate_ppm": rate.tolist()},
    }, open(args.out + ".json", "w"))


if __name__ == "__main__":
    main()
