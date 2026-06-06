#!/usr/bin/env python3
"""Estimate the inter-player audio offset from a recording of the synctest clip.

The clip emits, every 1 s, a 1 ms full-scale CLICK then a 1 kHz tone. With N
players where a synced group lands together and one is late by tau, a single
mixed mic/loopback records each click as a main pulse + a delayed copy. We:
  1. mono-mix, find the per-second primary clicks (the strongest = synced group),
  2. stack a short window around every primary click and average (kills noise,
     reinforces the consistently-delayed late copy),
  3. read the late pulse straight off the averaged envelope, and cross-check with
     the click autocorrelation side-peak (parabolic-interpolated, sub-sample).

Usage: scripts/analyze-sync.py [test.wav]
"""
import sys, wave
import numpy as np

path = sys.argv[1] if len(sys.argv) > 1 else "test.wav"
w = wave.open(path, "rb")
fs, nch, n = w.getframerate(), w.getnchannels(), w.getnframes()
x = np.frombuffer(w.readframes(n), dtype=np.int16).astype(np.float64).reshape(-1, nch)
x = x.mean(axis=1)                       # mono mix (channels are identical)
x /= np.max(np.abs(x)) + 1e-12
dur = len(x) / fs
print(f"{path}: {fs} Hz, {nch}ch, {dur:.2f}s, {len(x)} samples\n")

# --- 1. find primary clicks (transient envelope, ~1 s apart) -----------------
env = np.abs(np.diff(x, prepend=x[0]))   # differentiate -> emphasize transients
thr = 0.12 * env.max()
refractory = int(0.7 * fs)               # ~ the 1 s period (one click each)
peaks, i = [], 0
above = np.where(env > thr)[0]
for idx in above:
    if not peaks or idx - peaks[-1] > refractory:
        # refine to the local max within a few ms
        a, b = idx, min(idx + int(0.005 * fs), len(env))
        peaks.append(a + int(np.argmax(env[a:b])))
peaks = np.array(peaks)
print(f"detected {len(peaks)} click periods (expect ~{int(dur)})")

# --- 2. stack windows around each primary click ------------------------------
pre, post = int(0.002 * fs), int(0.030 * fs)   # -2 ms .. +30 ms
stack = []
for p in peaks:
    if p - pre >= 0 and p + post < len(x):
        stack.append(x[p - pre : p + post])
stack = np.array(stack)
avg = stack.mean(axis=0)
t_ms = (np.arange(len(avg)) - pre) / fs * 1e3
aenv = np.abs(avg)
# smooth the envelope a touch (0.3 ms boxcar) for peak picking
k = max(1, int(0.0003 * fs))
aenv = np.convolve(aenv, np.ones(k) / k, mode="same")

# --- 3a. read pulses off the averaged envelope -------------------------------
def local_maxima(sig, thr_frac, min_gap):
    th = thr_frac * sig.max()
    out = []
    for i in range(1, len(sig) - 1):
        if sig[i] > th and sig[i] >= sig[i - 1] and sig[i] > sig[i + 1]:
            if not out or i - out[-1] > min_gap:
                out.append(i)
    return out

pk = local_maxima(aenv, 0.15, int(0.0005 * fs))
print("\naveraged-click pulse arrivals (relative to primary):")
if pk:
    t0 = t_ms[pk[0]]
    for j, i in enumerate(pk[:6]):
        tag = "primary (synced group)" if j == 0 else f"+{t_ms[i]-t0:.3f} ms  <- late copy"
        print(f"  pulse {j}: t={t_ms[i]:+.3f} ms, level={aenv[i]/aenv.max():.2f}   {tag}")

# --- 3b. autocorrelation side-peak (sub-sample, parabolic) -------------------
c = np.correlate(avg, avg, mode="full")
c = c[len(c) // 2:]                       # lags >= 0
c[0] = 0                                  # ignore the zero-lag spike
guard = int(0.0008 * fs)                  # ignore <0.8 ms (tone/ringing autocorr)
lag = guard + int(np.argmax(np.abs(c[guard:int(0.030 * fs)])))
# parabolic interpolation around the integer peak
if 1 <= lag < len(c) - 1:
    y0, y1, y2 = c[lag - 1], c[lag], c[lag + 1]
    denom = (y0 - 2 * y1 + y2)
    delta = 0.5 * (y0 - y2) / denom if denom != 0 else 0.0
else:
    delta = 0.0
tau_samp = lag + delta
tau_ms = tau_samp / fs * 1e3
print(f"\nautocorrelation side-peak: lag = {tau_samp:.2f} samples = {tau_ms:.3f} ms")
print(f"  => the late player trails the synced group by ~{tau_ms:.3f} ms "
      f"({tau_ms*0.343:.1f} cm of sound travel)")

# tiny ASCII envelope of the averaged click (first 12 ms)
print("\naveraged click envelope (|amp|), 0..12 ms:")
span = int(0.012 * fs)
seg = aenv[pre:pre + span]
seg = seg / (seg.max() + 1e-12)
step = max(1, span // 60)
for i in range(0, span, step):
    bar = "#" * int(seg[i] * 50)
    print(f"  {i/fs*1e3:5.2f}ms |{bar}")
