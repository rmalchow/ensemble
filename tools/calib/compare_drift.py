#!/usr/bin/env python3
"""
compare_drift.py — overlay what the MICROPHONE hears against what the SERVO reports.

Inputs:
  --mic-json   the inter-speaker drift JSON from lr_drift.py (mic-measured pi02−pi01)
  --stats-log  the 1 Hz poll of GET /api/playback/statuses captured during the same
               run (each line: "<epoch> <json-array>", plus a "<epoch> PLAY" marker)

The microphone measures the *acoustic* inter-speaker offset (after the servo, plus
hardware). The players self-report each one's clock offset to the master. Their
difference (pi02 − pi01) is what the servo is working from. Overlaying the two shows
whether the acoustic reality tracks the servo's own books — and how much the servo
compensates vs leaves on the table.

Usage:
  python compare_drift.py --mic-json results/wideband.json --stats-log /tmp/stats_log.jsonl \
      --pi-low 781fd37e69de64d4f09ec6c501932f7a --pi-high 01eb7719afd542f404d3a6a733bedd82 \
      --out results/compare
"""
from __future__ import annotations
import argparse, json
import numpy as np

BG, FG, MUTED, ACCENT, ACCENT2, BORDER = (
    "#11151a", "#e6edf3", "#8b97a7", "#35e3b3", "#5bc8ff", "#2a3340")


def parse_stats(path, pi_low, pi_high):
    t0 = None
    t, off_diff, phase_diff, dev_diff, ppm_lo, ppm_hi = [], [], [], [], [], []
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        sp = line.split(" ", 1)
        if len(sp) < 2:
            continue
        ts = float(sp[0]); rest = sp[1]
        if rest == "PLAY":
            t0 = ts; continue
        if t0 is None:
            continue
        try:
            arr = json.loads(rest)
        except Exception:
            continue
        d = {n["nodeId"]: n for n in arr}
        if pi_low not in d or pi_high not in d:
            continue
        lo, hi = d[pi_low], d[pi_high]
        if not (lo.get("synced") and hi.get("synced")):
            continue
        t.append(ts - t0)
        off_diff.append((hi["offsetNs"] - lo["offsetNs"]) / 1e3)     # µs, pi2−pi1
        phase_diff.append((hi["phaseErrNs"] - lo["phaseErrNs"]) / 1e3)
        dev_diff.append((hi["deviceDelayNs"] - lo["deviceDelayNs"]) / 1e3)  # µs, static hw
        ppm_lo.append(lo["ratePPM"]); ppm_hi.append(hi["ratePPM"])
    return (np.array(t) / 60, np.array(off_diff), np.array(phase_diff),
            np.array(dev_diff), np.array(ppm_lo), np.array(ppm_hi))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mic-json", required=True)
    ap.add_argument("--stats-log", required=True)
    ap.add_argument("--pi-low", required=True, help="nodeId routed to L (pi01)")
    ap.add_argument("--pi-high", required=True, help="nodeId routed to R (pi02)")
    ap.add_argument("--out", default="results/compare")
    ap.add_argument("--label", default="microphone vs servo telemetry, 12 min")
    ap.add_argument("--bare", action="store_true", help="omit baked-in titles")
    args = ap.parse_args()

    mic = json.load(open(args.mic_json))["interspeaker"]
    tm = np.array(mic["t_min"]); dm = np.array(mic["drift_us"])
    tr, off, phase, dev, ppm_lo, ppm_hi = parse_stats(args.stats_log, args.pi_low, args.pi_high)
    print(f"mic: {len(tm)} pts; reported: {len(tr)} polls (synced)")
    if len(tr) < 4:
        print("not enough synced reported samples"); return

    # detrend each to compare SHAPE (remove constant offset + linear so we compare wander)
    def detr(t, y):
        s = np.polyfit(t, y, 1); return y - np.polyval(s, t)
    dm0 = dm - np.median(dm)
    off0 = off - np.median(off)
    # correlation of mic vs reported-offset-diff on a common grid
    grid = np.linspace(max(tm.min(), tr.min()), min(tm.max(), tr.max()), 200)
    mi = np.interp(grid, tm, dm0); ri = np.interp(grid, tr, off0)
    corr = np.corrcoef(mi, ri)[0, 1]
    print(f"offset-diff drift: {off.max()-off.min():.0f}µs span; mic inter-speaker {dm.max()-dm.min():.0f}µs span")
    print(f"correlation(mic, reported offset-diff) = {corr:+.2f}")
    print(f"reported phaseErr diff span: {phase.max()-phase.min():.0f}µs  (0 ⇒ pis don't populate phaseErr)")
    print(f"reported deviceDelay diff (static hw): mean {dev.mean():+.0f}µs  → explains the baseline acoustic offset")
    print(f"mic baseline (median acoustic pi02−pi01): {np.median(dm):+.0f}µs")
    print(f"servo ppm: pi_low {ppm_lo.mean():+.1f}  pi_high {ppm_hi.mean():+.1f}")

    import matplotlib; matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    plt.rcParams.update({"figure.facecolor": BG, "axes.facecolor": BG,
        "savefig.facecolor": BG, "text.color": FG, "axes.labelcolor": MUTED,
        "xtick.color": MUTED, "ytick.color": MUTED, "axes.edgecolor": BORDER,
        "axes.grid": True, "grid.color": BORDER, "grid.alpha": 0.5, "font.size": 11})
    fig, ax = plt.subplots(figsize=(11, 5), dpi=160)
    ax.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax.plot(tm, dm0, color=ACCENT, lw=1.6, label="microphone (acoustic pi02−pi01)")
    ax.plot(tr, off0, color=ACCENT2, lw=1.4, alpha=0.9, label="servo reported (clock offset pi02−pi01)")
    ax.set_xlabel("time (minutes)"); ax.set_ylabel("inter-speaker drift (µs, recentred)")
    ax.legend(loc="best", frameon=False)
    if not args.bare:
        fig.text(0.09, 0.95, "Microphone vs the servo's own clock telemetry", color=FG, fontsize=16, fontweight="bold")
        fig.text(0.063, 0.955, "•", color=ACCENT, fontsize=17)
        fig.text(0.975, 0.95, f"r = {corr:+.2f}", color=ACCENT, fontsize=18, fontweight="bold", ha="right")
        fig.text(0.5, 0.005, args.label, color=MUTED, ha="center", fontsize=10)
    fig.subplots_adjust(top=0.97 if args.bare else 0.87, bottom=0.12)
    fig.savefig(args.out + ".svg"); fig.savefig(args.out + ".png")
    print("wrote", args.out + ".svg/.png")
    json.dump({"correlation": float(corr),
               "mic": {"t_min": tm.tolist(), "drift_us": dm0.tolist()},
               "reported_offset": {"t_min": tr.tolist(), "drift_us": off0.tolist()}},
              open(args.out + ".json", "w"))


if __name__ == "__main__":
    main()
