#!/usr/bin/env python3
"""
make_playout.py — render the interleaved L/R sweep playout for a coherence run.

Produces the STEREO file ensemble plays to the pi01+pi02 group. pi01 is routed
to L, pi02 to R (physical routing), so putting the wideband sweep on the LEFT at
even cadence slots and on the RIGHT at odd slots makes the two speakers sound
one-at-a-time, time-interleaved — exactly what lr_drift.py expects
(--period 2.4 --gap 1.2).

Also dumps the matched-filter reference (the single sweep) as .npy for analysis.

  L (pi01):  sweep at t = 0, period, 2*period, ...
  R (pi02):  sweep at t = gap, gap+period, ...

Example:
  python make_playout.py --minutes 30 --out /path/to/media/lrrun.wav --ref /tmp/ref_wb.npy
"""
from __future__ import annotations
import argparse, os, sys
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from sweep import generate_sweep, write_wav_s16  # reuse the reference DSP
import codec  # self-identifying-sweep counter frames (optional)

SR = 48_000


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--minutes", type=float, default=30.0, help="playout length")
    ap.add_argument("--period", type=float, default=2.4, help="per-channel sweep period (s)")
    ap.add_argument("--gap", type=float, default=1.2, help="L->R interleave offset (s)")
    ap.add_argument("--f0", type=float, default=100.0)
    ap.add_argument("--f1", type=float, default=12_000.0)
    ap.add_argument("--dur", type=float, default=1.0, help="sweep duration (s)")
    ap.add_argument("--amp", type=float, default=0.5, help="sweep peak amplitude")
    ap.add_argument("--lead", type=float, default=3.0, help="silent lead-in (s)")
    ap.add_argument("--coded", action="store_true",
                    help="append a codec.py counter frame after each sweep (slot index: even=L, odd=R)")
    ap.add_argument("--ndig", type=int, default=codec.COUNTER_NDIG, help="counter digits (base-16)")
    ap.add_argument("--out", required=True, help="output stereo WAV (into the media dir)")
    ap.add_argument("--ref", default="/tmp/ref_wb.npy", help="matched-filter reference (.npy)")
    args = ap.parse_args()

    sweep = generate_sweep(args.f0, args.f1, args.dur, SR, amplitude=args.amp)
    np.save(args.ref, sweep.astype(np.float64))
    # The per-sweep signal: bare sweep, or sweep||gap||counter-frame. The frame
    # offset (sweep arrival -> SYNC) is fixed and handed to the decoder.
    frame_off = None
    if args.coded:
        _b, frame_off = codec.build_coded_burst(sweep, 0, args.ndig)
        burst_len = len(_b)
        print(f"coded: frame_offset={frame_off} samples ({frame_off/SR*1000:.0f} ms), "
              f"burst_len={burst_len/SR*1000:.0f} ms (must fit gap {args.gap*1000:.0f} ms)")

    n_total = int(round((args.minutes * 60 + args.lead) * SR))
    L = np.zeros(n_total, dtype=np.float64)
    R = np.zeros(n_total, dtype=np.float64)
    lead = int(round(args.lead * SR))
    per = int(round(args.period * SR))
    gap = int(round(args.gap * SR))
    sl = len(sweep)

    def slot_signal(counter: int) -> np.ndarray:
        if not args.coded:
            return sweep
        b, _ = codec.build_coded_burst(sweep, counter, args.ndig)
        return b

    nL = nR = 0
    k = 0
    while True:
        lpos = lead + k * per
        rpos = lead + gap + k * per
        # Monotonic slot counter in TIME order: L(k)=2k, R(k)=2k+1.
        sigL, sigR = slot_signal(2 * k), slot_signal(2 * k + 1)
        placed = False
        if lpos + len(sigL) <= n_total:
            L[lpos:lpos + len(sigL)] += sigL; nL += 1; placed = True
        if rpos + len(sigR) <= n_total:
            R[rpos:rpos + len(sigR)] += sigR; nR += 1; placed = True
        if not placed:
            break
        k += 1

    stereo = np.stack([L, R], axis=1).reshape(-1)  # interleaved L,R,L,R for write
    # write_wav_s16 is mono; emit stereo s16 by hand (same clip+scale).
    import wave
    clipped = np.clip(stereo, -1.0, 1.0)
    pcm = (clipped * 32767.0).round().astype("<i2")
    with wave.open(args.out, "wb") as w:
        w.setnchannels(2); w.setsampwidth(2); w.setframerate(SR)
        w.writeframes(pcm.tobytes())

    dur_min = n_total / SR / 60
    print(f"wrote {args.out}: {dur_min:.1f} min stereo s16 @ {SR} Hz, "
          f"L sweeps={nL} R sweeps={nR}, period={args.period}s gap={args.gap}s")
    print(f"wrote reference: {args.ref} ({sl} samples)")


if __name__ == "__main__":
    main()
