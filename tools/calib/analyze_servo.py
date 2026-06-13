#!/usr/bin/env python3
"""
analyze_servo.py — does the servo's REALIZED correction match what it COMMANDED,
and does either explain what the MICROPHONE hears?

Three independent views of the same 10-min run:

  1. COMMANDED  — ∫ ratePPM·dt per node. The servo's instantaneous rate order.
  2. REALIZED   — (samplesInjected − samplesDropped)/48 ms per node. The resampler
                  is strictly 1-frame-in/1-frame-out, so the ONLY way it nets
                  samples into/out of the stream over the run is the carry
                  overflow-drop (rate>1) and underflow-inject (rate<1) guards.
                  That count IS the cumulative time the rate correction actually
                  applied to the DAC — ground truth, not the commanded ppm.
  3. ACOUSTIC   — the microphone's inter-speaker arrival offset (R−L), from the
                  gated dual tones (tones.py).

If REALIZED tracks COMMANDED, the counters are honest and the resampler faithfully
executes the servo. The inter-speaker REALIZED *difference* is the net relative
timing the servo pushed between the two speakers; comparing it to the ACOUSTIC
inter-speaker drift shows how much of the acoustic wander the servo is actually
removing vs leaving on the table.

Inputs:
  --wav        the mic recording (raw s16 stereo, 48 kHz) from run_tones.sh
  --stats-log  the 1 Hz poll of /api/playback/statuses (+ a "<epoch> PLAY" marker)
  --pi-low     nodeId routed to L / fL=2300  (pi01)
  --pi-high    nodeId routed to R / fR=2900  (pi02)
  --out        output basename (writes .png/.svg/.json)
"""
from __future__ import annotations
import argparse, json
import numpy as np
import tones  # sibling module: acoustic dual-tone analysis

SR = 48_000
BG, FG, MUTED, ACCENT, ACCENT2, ACCENT3, BORDER = (
    "#11151a", "#e6edf3", "#8b97a7", "#35e3b3", "#5bc8ff", "#ffb454", "#2a3340")


def parse_stats(path, pi_low, pi_high):
    """Returns dict of arrays anchored at the PLAY marker (t in minutes)."""
    t0 = None
    rows = {k: [] for k in ("t", "ppm_lo", "ppm_hi", "inj_lo", "inj_hi",
                            "drop_lo", "drop_hi", "off_lo", "off_hi")}
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
        rows["t"].append((ts - t0) / 60)
        rows["ppm_lo"].append(lo["ratePPM"]);   rows["ppm_hi"].append(hi["ratePPM"])
        rows["inj_lo"].append(lo["samplesInjected"]); rows["inj_hi"].append(hi["samplesInjected"])
        rows["drop_lo"].append(lo["samplesDropped"]);  rows["drop_hi"].append(hi["samplesDropped"])
        rows["off_lo"].append(lo["offsetNs"] / 1e3);    rows["off_hi"].append(hi["offsetNs"] / 1e3)
    return {k: np.array(v, float) for k, v in rows.items()}


def commanded_ms(t_min, ppm):
    """Cumulative ∫ppm·dt expressed as a time shift in ms.
    rate-1 = ppm/1e6; extra samples/s = (rate-1)*SR = ppm*SR/1e6; /SR → seconds of
    shift per second = ppm/1e6. Cumulative ms = cumsum(ppm/1e6 * dt_s) * 1e3."""
    t_s = t_min * 60
    dt = np.diff(t_s, prepend=t_s[0] if len(t_s) else 0.0)
    return np.cumsum(ppm / 1e6 * dt) * 1e3


def realized_ms(inj, drop):
    """(injected − dropped) per-channel samples → ms (48 samples = 1 ms).
    Re-zero to the first sample so we measure the shift accrued during the run."""
    net = (inj - drop)
    net = net - (net[0] if len(net) else 0)
    return net / 48.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--wav", required=True)
    ap.add_argument("--stats-log", required=True)
    ap.add_argument("--pi-low", required=True)
    ap.add_argument("--pi-high", required=True)
    ap.add_argument("--out", default="results/servo")
    ap.add_argument("--label", default="realized vs commanded correction + acoustic, 10 min")
    args = ap.parse_args()

    s = parse_stats(args.stats_log, args.pi_low, args.pi_high)
    if len(s["t"]) < 4:
        print("not enough synced polls"); return
    cmd_lo, cmd_hi = commanded_ms(s["t"], s["ppm_lo"]), commanded_ms(s["t"], s["ppm_hi"])
    rea_lo, rea_hi = realized_ms(s["inj_lo"], s["drop_lo"]), realized_ms(s["inj_hi"], s["drop_hi"])

    # Acoustic inter-speaker offset over the run (skip first 5 s of settling).
    off, tm = tones.analyze(tones.read_wav_stereo(args.wav))
    keep = tm > (5 / 60)
    off, tm = off[keep], tm[keep]
    off0 = off - np.median(off)

    # Realized inter-speaker difference (hi − lo), re-centred to compare shape.
    rea_diff = rea_hi - rea_lo
    rea_diff0 = rea_diff - np.median(rea_diff)

    print(f"polls {len(s['t'])} | mic edges {len(off)}")
    print(f"commanded span: lo {cmd_lo.max()-cmd_lo.min():.2f}ms  hi {cmd_hi.max()-cmd_hi.min():.2f}ms")
    print(f"realized  span: lo {rea_lo.max()-rea_lo.min():.2f}ms  hi {rea_hi.max()-rea_hi.min():.2f}ms")
    for nm, cmd, rea in (("pi_low", cmd_lo, rea_lo), ("pi_high", cmd_hi, rea_hi)):
        if np.std(cmd) > 1e-9 and np.std(rea) > 1e-9:
            r = np.corrcoef(cmd, rea)[0, 1]
            print(f"  {nm}: corr(commanded, realized) = {r:+.2f}")
    print(f"final injected/dropped: lo {s['inj_lo'][-1]:.0f}/{s['drop_lo'][-1]:.0f}  "
          f"hi {s['inj_hi'][-1]:.0f}/{s['drop_hi'][-1]:.0f}")
    print(f"acoustic inter-speaker span: {off.max()-off.min():.0f}us  median {np.median(off):+.0f}us")
    # correlation of acoustic vs realized-difference on a common grid
    if len(tm) > 3:
        grid = np.linspace(max(tm.min(), s["t"].min()), min(tm.max(), s["t"].max()), 200)
        mi = np.interp(grid, tm, off0)
        ri = np.interp(grid, s["t"], rea_diff0 * 1e3)  # ms→us
        corr = np.corrcoef(mi, ri)[0, 1]
        print(f"corr(acoustic, realized-difference) = {corr:+.2f}")
    else:
        corr = float("nan")

    import matplotlib; matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    plt.rcParams.update({"figure.facecolor": BG, "axes.facecolor": BG,
        "savefig.facecolor": BG, "text.color": FG, "axes.labelcolor": MUTED,
        "xtick.color": MUTED, "ytick.color": MUTED, "axes.edgecolor": BORDER,
        "axes.grid": True, "grid.color": BORDER, "grid.alpha": 0.5, "font.size": 10})
    fig, (ax1, ax2) = plt.subplots(2, 1, figsize=(11, 8), dpi=160, sharex=True)

    # Top: per-node commanded vs realized.
    ax1.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax1.plot(s["t"], cmd_lo, color=ACCENT, lw=1.3, ls="--", label="pi01 commanded ∫ppm")
    ax1.plot(s["t"], rea_lo, color=ACCENT, lw=1.8, label="pi01 realized (inj−drop)")
    ax1.plot(s["t"], cmd_hi, color=ACCENT2, lw=1.3, ls="--", label="pi02 commanded ∫ppm")
    ax1.plot(s["t"], rea_hi, color=ACCENT2, lw=1.8, label="pi02 realized (inj−drop)")
    ax1.set_ylabel("cumulative correction (ms)")
    ax1.legend(loc="best", frameon=False, ncol=2, fontsize=8)
    ax1.set_title("Servo: commanded (∫ppm) vs realized (resampler inj/drop)", color=FG, fontsize=12)

    # Bottom: acoustic inter-speaker vs realized difference.
    ax2.axhline(0, color=MUTED, lw=0.8, alpha=0.6)
    ax2.plot(tm, off0, color=ACCENT3, lw=1.6, label="microphone (acoustic R−L)")
    ax2.plot(s["t"], rea_diff0 * 1e3, color=FG, lw=1.4, alpha=0.9, label="realized difference (pi02−pi01)")
    ax2.set_xlabel("time (minutes)"); ax2.set_ylabel("inter-speaker (µs, recentred)")
    ax2.legend(loc="best", frameon=False, fontsize=8)
    ax2.set_title(f"Acoustic vs realized inter-speaker  (r = {corr:+.2f})", color=FG, fontsize=12)

    fig.text(0.5, 0.005, args.label, color=MUTED, ha="center", fontsize=9)
    fig.subplots_adjust(top=0.94, bottom=0.08, hspace=0.18)
    fig.savefig(args.out + ".svg"); fig.savefig(args.out + ".png")
    print("wrote", args.out + ".svg/.png")
    json.dump({
        "corr_acoustic_realized": float(corr),
        "t_min": s["t"].tolist(),
        "commanded_ms": {"pi_low": cmd_lo.tolist(), "pi_high": cmd_hi.tolist()},
        "realized_ms": {"pi_low": rea_lo.tolist(), "pi_high": rea_hi.tolist()},
        "acoustic": {"t_min": tm.tolist(), "off_us": off0.tolist()},
        "final_counts": {"inj_lo": float(s["inj_lo"][-1]), "drop_lo": float(s["drop_lo"][-1]),
                         "inj_hi": float(s["inj_hi"][-1]), "drop_hi": float(s["drop_hi"][-1])},
    }, open(args.out + ".json", "w"))


if __name__ == "__main__":
    main()
