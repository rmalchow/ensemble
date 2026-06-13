#!/usr/bin/env python3
"""
tones.py — dual-tone gated inter-speaker timing (frequency-separated, edge-timed).

An alternative to the time-interleaved sweeps (lr_drift.py). Instead of playing
the speakers one-at-a-time, both play CONTINUOUSLY and SIMULTANEOUSLY but at
different frequencies (pi01/L = fL, pi02/R = fR), gated on/off together. A single
mic records the sum; a sharp bandpass isolates each speaker by frequency, and the
RISING edges of the gated bursts give each speaker's timing. The L↔R edge offset
is the inter-speaker sync error, tracked over the whole run — so a re-sync /
sawtooth shows up directly as a jump in the offset.

Why this beats the sweep method here:
  - both speakers measured continuously (no interleave, no midpoint trick),
  - amplitude-envelope EDGES are robust to phase/reflections (a reflection smears
    the tail but the initial rise is the direct arrival),
  - no matched-filter peak-picking (which locked onto reflections at low SNR).

Frequency choice — keep them CLOSE so the two tones see comparable speaker /
room / mic response (symmetric edge SNR), while harmonics + intermod miss both
passbands:
  fL=2300, fR=2900 Hz, passband ±250 Hz (2050–2550 / 2650–3150), 100 Hz guard.
  For close mid-band tones the 2f/3f harmonics (4600, 5800, 6900 …) sit far
  above the passbands automatically — the only real risk is intermodulation:
  fR−fL=600, fR+fL=5200, 2fL−fR=1700, 2fR−fL=3500, 3fL−fR=4000 — none land in
  either passband. ±250 Hz still rejects the other tone hard (it is 600 Hz =
  2.4 passband-widths away at a 6th-order roll-off).

Use the RISING edge only: gate-ON is led by the direct arrival; gate-OFF is
smeared by the room's reverb decay.

Pure numpy + scipy; validates in software (`python tones.py`) with a synthetic
recording at a known L↔R offset, echoes and noise.
"""
from __future__ import annotations
import argparse, sys, wave
import numpy as np
from scipy.signal import butter, sosfiltfilt, hilbert

SR = 48_000
FL, FR = 2300.0, 2900.0   # close together -> comparable energy; intermod misses both bands
BW = 250.0          # passband half-width (Hz); 100 Hz guard between the two bands
ON_S, OFF_S = 0.40, 0.40
RAMP_S = 0.006      # raised-cosine gate edge (limits spectral splatter to ~1/ramp)


def _gate(n_on, n_off, n_ramp):
    """One on+off gate cycle as a 0..1 envelope with raised-cosine edges."""
    g = np.zeros(n_on + n_off)
    g[:n_on] = 1.0
    if n_ramp > 0:
        r = 0.5 * (1 - np.cos(np.pi * np.arange(n_ramp) / n_ramp))  # 0->1
        g[:n_ramp] *= r
        g[n_on - n_ramp:n_on] *= r[::-1]
    return g


def generate(minutes=3.0, fL=FL, fR=FR, on_s=ON_S, off_s=OFF_S, ramp_s=RAMP_S,
             amp=0.5, lead_s=2.0):
    """Stereo float: L=gated fL, R=gated fR (same gate schedule). Returns array."""
    n_on, n_off, n_ramp = int(on_s * SR), int(off_s * SR), int(ramp_s * SR)
    cyc = _gate(n_on, n_off, n_ramp)
    ncyc = int(np.ceil(minutes * 60 * SR / len(cyc)))
    gate = np.tile(cyc, ncyc)
    lead = np.zeros(int(lead_s * SR))
    gate = np.concatenate([lead, gate])
    t = np.arange(len(gate)) / SR
    L = amp * gate * np.sin(2 * np.pi * fL * t)
    R = amp * gate * np.sin(2 * np.pi * fR * t)
    return np.stack([L, R], 1)


def _bandpass(x, f0, bw=BW, order=6):
    sos = butter(order, [(f0 - bw) / (SR / 2), (f0 + bw) / (SR / 2)], "bandpass", output="sos")
    return sosfiltfilt(sos, x)


def _envelope(x):
    return np.abs(hilbert(x))


def rising_edges(env, frac=0.5, min_gap_s=0.5):
    """
    Sub-sample times (s) where the envelope crosses `frac` of its high level on
    the way UP. High level = 90th percentile of the envelope (the ON plateau).
    """
    hi = np.percentile(env, 90)
    thr = frac * hi
    above = env > thr
    # rising crossings: False->True
    idx = np.where((~above[:-1]) & (above[1:]))[0]
    out = []
    last = -1e9
    for i in idx:
        # sub-sample: linear interp of env across thr between i and i+1
        e0, e1 = env[i], env[i + 1]
        frac_s = (thr - e0) / (e1 - e0) if e1 != e0 else 0.0
        ts = (i + frac_s) / SR
        if ts - last >= min_gap_s:      # one edge per gate cycle
            out.append(ts); last = ts
    return np.array(out)


def analyze(stereo, fL=FL, fR=FR):
    """Returns (offset_us[], t_min[]) — per-cycle R-rise minus L-rise."""
    m = stereo.mean(1) if stereo.ndim > 1 else stereo
    eL = _envelope(_bandpass(m, fL))
    eR = _envelope(_bandpass(m, fR))
    tL = rising_edges(eL)
    tR = rising_edges(eR)
    # pair each L edge with the nearest R edge within ±0.2 s
    off, tm = [], []
    for a in tL:
        j = np.argmin(np.abs(tR - a)) if len(tR) else None
        if j is not None and abs(tR[j] - a) < 0.2:
            off.append((tR[j] - a) * 1e6); tm.append(a / 60)
    return np.array(off), np.array(tm)


def write_wav_s16(path, stereo):
    pcm = (np.clip(stereo, -1, 1) * 32767).round().astype("<i2")
    with wave.open(path, "wb") as w:
        w.setnchannels(2); w.setsampwidth(2); w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def read_wav_stereo(path):
    raw = open(path, "rb").read()
    return np.frombuffer(raw[44:], dtype="<i2").astype(float).reshape(-1, 2) / 32768


# ---------------------------------------------------------------------------
def _selftest():
    rng = np.random.default_rng(3)
    st = generate(minutes=0.5)          # 30 s
    m = st.mean(1)
    DELAY_US = 2500.0                    # inject a known R-vs-L offset
    dly = int(DELAY_US * 1e-6 * SR)
    # rebuild the recording: L on time, R shifted by `dly`
    L = st[:, 0].copy()
    R = np.zeros_like(st[:, 1]); R[dly:] = st[:-dly, 1]
    rec = L + R
    for d_ms, g in ((9.0, 0.4), (23.0, 0.25)):   # room echoes
        d = int(d_ms * 1e-3 * SR); rec[d:] += g * (L + R)[:-d]
    rec += rng.normal(0, 0.02, len(rec))         # noise
    off, tm = analyze(np.stack([rec, rec], 1))
    med = np.median(off)
    print(f"injected R-L offset {DELAY_US:.0f}us | recovered median {med:.0f}us "
          f"(n={len(off)}, jitter {np.std(off):.0f}us)")
    ok = abs(med - DELAY_US) < 200 and np.std(off) < 200
    print("PASS" if ok else "FAIL")
    return 0 if ok else 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--gen", help="write a gated dual-tone stereo WAV to this path")
    ap.add_argument("--minutes", type=float, default=3.0)
    ap.add_argument("--analyze", help="analyze a recording WAV (raw s16 stereo from pw-record)")
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()
    if args.selftest or (not args.gen and not args.analyze):
        sys.exit(_selftest())
    if args.gen:
        write_wav_s16(args.gen, generate(minutes=args.minutes))
        print(f"wrote {args.gen} ({args.minutes} min, L={FL:.0f}Hz R={FR:.0f}Hz)")
    if args.analyze:
        off, tm = analyze(read_wav_stereo(args.analyze))
        if len(off):
            print(f"edges {len(off)} | offset median {np.median(off):+.0f}us "
                  f"const(mean) {np.mean(off):+.0f}us | variation std {np.std(off):.0f}us "
                  f"MAD {np.median(np.abs(off-np.median(off)))*1.4826:.0f}us")
        else:
            print("no edges detected")


if __name__ == "__main__":
    main()
