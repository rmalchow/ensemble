#!/usr/bin/env python3
"""
codec.py — in-band sweep labelling + microphone-ADC clock anchoring (prototype).

This is the "self-identifying sweep" layer for the coherence measurement (see
docs/calibration.md). The timing layer is unchanged: every burst is the SAME
wideband sweep (sweep.py), matched-filtered to a sub-sample *position*. This
module adds a *label* layer — a short, low-rate tone frame appended after each
sweep — that carries:

  - a per-sweep rolling COUNTER (cheap, every sweep), and
  - every Nth sweep, an absolute master-time EPOCH (master-clock ms).

Why: the microphone's ADC samples at its own free-running crystal rate, which
drifts (thermally) over a long run. The midpoint trick cancels that rate for the
*inter-speaker* (differential) measurement, but a *per-speaker absolute* number
has no common-mode cancellation, so the full ADC rate+drift leaks in (~1 ppm over
11 min ≈ 0.66 ms — comparable to the signal). The labels give us anchors

    (recorded_sample_position, master_time)

with master_time originating on a node clock (NOT the ADC), so a fit of
master_time vs sample_position removes the ADC rate *and its slow curvature*.

KEY DECOUPLING: precision comes from the sweep peak; identity/time comes from the
tone frame. So the frame only has to be *readable*, not sample-accurate — which
is why a slow, redundant, reverb-tolerant tone code is fine.

WHERE master_time COMES FROM (and the one caveat): the anchors must be emitted by
a REFERENCE node's audio path (ideally the master itself, in-room), because the
recorded position of an anchor = emit_PTS + that_node's_DAC_latency + propagation.
For a single reference emitter those are CONSTANTS — absorbed by the fit's offset
term, leaving the *rate* clean. Anchoring off the speakers under test would fold
their DAC thermal drift into the reference; don't.

Everything here is pure numpy and validates in software (`python codec.py`) the
same way selftest.py does — no hardware, no scipy. The eventual Go emit side
(internal/calib) mirrors the frame layout; this is the reference implementation.
"""
from __future__ import annotations

import numpy as np

SR = 48_000

# --- Frame layout ----------------------------------------------------------
# A frame is a sequence of fixed-length tone "slots":
#     [SYNC] [digit_0] [digit_1] ... [digit_{ndig-1}] [checksum]
# Each slot is one pure tone with short raised-cosine fades, separated by a
# guard so room reverb from one slot decays before the next. The payload is a
# non-negative integer rendered as `ndig` base-RADIX digits, most-significant
# first; the checksum slot carries sum(digits) % RADIX.
SYNC_FREQ = 1_000.0          # below the data band; marks frame start + presence
DATA_F0 = 1_500.0            # first data tone
DATA_DF = 150.0              # tone spacing
RADIX = 16                   # 16 tones -> 4 bits/slot (1500..3750 Hz)
SYM_S = 0.040                # tone duration (s): 25 Hz bin res << 150 Hz spacing
GUARD_S = 0.010              # inter-slot guard (reverb decay)
FADE_S = 0.004               # per-tone raised-cosine fade
AMP = 0.40

SYM_N = int(round(SYM_S * SR))
GUARD_N = int(round(GUARD_S * SR))
SLOT_N = SYM_N + GUARD_N

# Counter frames carry a 16-bit rolling index (4 hex digits); epoch frames
# additionally pin absolute master-time. We keep two payload widths.
COUNTER_NDIG = 4             # 16-bit counter
EPOCH_NDIG = 11              # 44-bit ms value (~557 years) — generous headroom


def _data_freq(digit: int) -> float:
    return DATA_F0 + digit * DATA_DF


def _tone(freq: float, n: int = SYM_N, amp: float = AMP) -> np.ndarray:
    t = np.arange(n, dtype=np.float64) / SR
    x = amp * np.sin(2 * np.pi * freq * t)
    nf = int(round(FADE_S * SR))
    if nf > 0 and 2 * nf < n:
        ramp = 0.5 * (1.0 - np.cos(np.pi * np.arange(nf) / nf))
        x[:nf] *= ramp
        x[-nf:] *= ramp[::-1]
    return x


def _digits(value: int, ndig: int) -> list[int]:
    if value < 0 or value >= RADIX ** ndig:
        raise ValueError(f"value {value} does not fit in {ndig} base-{RADIX} digits")
    out = []
    for _ in range(ndig):
        out.append(value % RADIX)
        value //= RADIX
    return out[::-1]  # most-significant first


def encode_frame(value: int, ndig: int) -> np.ndarray:
    """Render a frame (SYNC + ndig digit tones + checksum tone) as audio."""
    digs = _digits(value, ndig)
    chk = sum(digs) % RADIX
    slots = [SYNC_FREQ] + [_data_freq(d) for d in digs] + [_data_freq(chk)]
    out = np.zeros(len(slots) * SLOT_N, dtype=np.float64)
    for i, f in enumerate(slots):
        out[i * SLOT_N : i * SLOT_N + SYM_N] = _tone(f)
    return out


def frame_len(ndig: int) -> int:
    return (ndig + 2) * SLOT_N


def build_coded_burst(sweep: np.ndarray, value: int, ndig: int,
                      gap_s: float = 0.05) -> tuple[np.ndarray, int]:
    """
    sweep || guard || frame(value). Returns (samples, frame_start_offset).
    `frame_start_offset` is where the frame begins relative to the sweep start,
    which on the decode side is `arrival_sample + frame_start_offset`.
    """
    gap = int(round(gap_s * SR))
    frame = encode_frame(value, ndig)
    out = np.concatenate([sweep, np.zeros(gap), frame])
    return out, len(sweep) + gap


# --- Single-bin power (Goertzel-equivalent) --------------------------------
def _bin_power(x: np.ndarray, freq: float) -> float:
    """Energy of `x` at `freq` (one DFT bin). Phase-agnostic |re+jim|^2."""
    t = np.arange(len(x), dtype=np.float64) / SR
    re = np.dot(x, np.cos(2 * np.pi * freq * t))
    im = np.dot(x, np.sin(2 * np.pi * freq * t))
    return re * re + im * im


def _slot_core(rec: np.ndarray, start: int) -> np.ndarray:
    """The fade-free centre of a tone slot at `start` (for clean Goertzel)."""
    nf = int(round(FADE_S * SR))
    lo, hi = start + nf, start + SYM_N - nf
    return rec[max(0, lo):hi]


def decode_frame(rec: np.ndarray, approx_sweep_arrival: int, ndig: int,
                 frame_offset: int, search_s: float = 0.012) -> int | None:
    """
    Decode a frame whose sweep arrived near `approx_sweep_arrival`.

    `frame_offset` is the samples from sweep arrival to the frame's SYNC slot
    (= len(sweep) + gap; the value `build_coded_burst` returns). On the real
    pipeline the reference sweep length and gap are known, so this is fixed.

    Aligns to the SYNC tone within ±search_s (absorbs sweep-peak jitter and the
    label layer's own slop), then reads digit slots at fixed stride and verifies
    the checksum. Returns the integer payload, or None if SYNC is absent / the
    checksum fails (i.e. a reverb-corrupted or missing frame — caller drops it).
    """
    nominal = approx_sweep_arrival + frame_offset  # expected SYNC slot start
    search = int(round(search_s * SR))

    # Coarse SYNC search: maximise SYNC-band power over candidate starts.
    best_p, best_start = 0.0, nominal
    for s in range(nominal - search, nominal + search + 1, max(1, SR // 4000)):
        if s < 0 or s + SYM_N > len(rec):
            continue
        p = _bin_power(_slot_core(rec, s), SYNC_FREQ)
        if p > best_p:
            best_p, best_start = p, s

    # Validity gate: SYNC must dominate the data band at its slot.
    sync_core = _slot_core(rec, best_start)
    if best_p <= 0:
        return None
    data_max = max(_bin_power(sync_core, _data_freq(d)) for d in range(RADIX))
    if best_p < 4.0 * data_max:        # SYNC not clearly present
        return None

    # Read digit + checksum slots.
    digs = []
    for i in range(1, ndig + 2):
        core = _slot_core(rec, best_start + i * SLOT_N)
        if len(core) < SYM_N // 2:
            return None
        powers = [_bin_power(core, _data_freq(d)) for d in range(RADIX)]
        digs.append(int(np.argmax(powers)))
    payload, chk = digs[:-1], digs[-1]
    if sum(payload) % RADIX != chk:
        return None
    value = 0
    for d in payload:
        value = value * RADIX + d
    return value


# --- Anchor fit: master_time(sample_position), ADC drift removed -----------
def fit_emit_schedule(epoch_counters: np.ndarray, epoch_ms: np.ndarray) -> tuple[float, float]:
    """
    From the sparse epoch frames recover the master emit schedule:
        master_ms(counter) = pts0 + period_ms * counter
    Needs >= 2 epoch frames. Returns (pts0, period_ms). This is the master's OWN
    cadence in master-clock ms — independent of the recording, so it carries no
    ADC drift.
    """
    if len(epoch_counters) < 2:
        raise ValueError("need >= 2 epoch frames to fix the emit schedule")
    period_ms, pts0 = np.polyfit(epoch_counters.astype(float), epoch_ms.astype(float), 1)
    return float(pts0), float(period_ms)


def fit_adc_model(positions: np.ndarray, master_ms: np.ndarray, deg: int = 2):
    """
    Fit master_ms ≈ poly(sample_position). deg=1 removes a constant ADC rate
    error; deg=2 also removes the slow (thermal) curvature. The constant term
    absorbs capture-start + reference-DAC latency + propagation (all fixed for a
    single reference emitter), so only the shape matters. Returns a np.poly1d
    mapping sample_position -> master_ms.
    """
    return np.poly1d(np.polyfit(positions.astype(float), master_ms.astype(float), deg))


def build_anchors(detections: list[dict], deg: int = 2):
    """
    detections: one dict per detected sweep:
        {"pos": <sub-sample arrival>, "counter": <int>, "epoch_ms": <float|None>}
    Returns (sample_to_master_ms_poly, residual_us) where residual_us is the
    per-anchor RMS of the fit (a self-consistency check, not ground truth).
    """
    pos = np.array([d["pos"] for d in detections], float)
    cnt = np.array([d["counter"] for d in detections], float)
    ep = [(d["counter"], d["epoch_ms"]) for d in detections if d.get("epoch_ms") is not None]
    ec = np.array([c for c, _ in ep], float)
    em = np.array([m for _, m in ep], float)
    pts0, period_ms = fit_emit_schedule(ec, em)
    master_ms = pts0 + period_ms * cnt
    poly = fit_adc_model(pos, master_ms, deg)
    resid_us = float(np.std(poly(pos) - master_ms) * 1e3)
    return poly, resid_us


# ---------------------------------------------------------------------------
# Self-test (no hardware): codec round-trip under noise+reverb, and the anchor
# fit recovering master-time through a drifting ADC. `python codec.py`.
# ---------------------------------------------------------------------------
def _selftest() -> int:
    rng = np.random.default_rng(1)
    fails = 0

    # 1) Codec round-trip: encode -> echoes + noise -> decode, many trials.
    sweep = np.zeros(int(0.2 * SR))  # placeholder; codec is independent of sweep
    for ndig in (COUNTER_NDIG, EPOCH_NDIG):
        ok = 0
        trials = 200
        for _ in range(trials):
            val = int(rng.integers(0, RADIX ** ndig))
            burst, frame_off = build_coded_burst(sweep, val, ndig)
            # Embed in a longer buffer at a known sweep arrival.
            arr = int(rng.integers(SR // 2, SR))
            buf = np.zeros(arr + len(burst) + SR)
            buf[arr:arr + len(burst)] += burst
            # Room: two attenuated, delayed echoes.
            for delay_ms, g in ((11.0, 0.5), (29.0, 0.3)):
                d = int(delay_ms * 1e-3 * SR)
                buf[arr + d:arr + d + len(burst)] += g * burst
            # White noise at ~20 dB SNR relative to tone amplitude.
            buf += rng.normal(0, AMP / 10.0, len(buf))
            # Decoder is told the sweep arrival with a few-ms jitter.
            jit = int(rng.integers(-int(0.004 * SR), int(0.004 * SR)))
            got = decode_frame(buf, arr + jit, ndig, frame_off)
            ok += (got == val)
        rate = ok / trials
        tag = "counter" if ndig == COUNTER_NDIG else "epoch"
        print(f"codec[{tag}, {ndig} digits]: {ok}/{trials} decoded ({rate*100:.1f}%)")
        if rate < 0.98:
            fails += 1

    # 2) Anchor fit: synthesize a drifting ADC and recover master-time.
    n = 240                                   # ~240 sweeps
    counters = np.arange(n)
    period_ms = 1_200.0                        # master emits every 1.2 s
    pts0 = 5_000.0
    te_ms = pts0 + period_ms * counters        # true master emit times
    # ADC maps master-time -> recorded sample with a rate error + slow thermal
    # curvature, plus a fixed offset (capture start + DAC latency + propagation).
    rate_err_ppm = 8.0
    curv = -2.0e-11                            # quadratic drift in ms^-1 (slow, ~2 ms over the run)
    P0 = 3_456.7
    spm = SR / 1_000.0                          # samples per ms (nominal)
    t = te_ms - te_ms[0]
    pos = P0 + spm * (1 + rate_err_ppm * 1e-6) * t + spm * curv * t ** 2
    pos += rng.normal(0, 0.25, n)              # sub-sample peak jitter (~5 µs)

    # Epoch frames every 20th sweep carry absolute master ms; all carry counter.
    dets = []
    for i in range(n):
        dets.append({"pos": pos[i], "counter": int(counters[i]),
                     "epoch_ms": (te_ms[i] if i % 20 == 0 else None)})
    poly, resid_us = build_anchors(dets, deg=2)

    # Corrected master-time per sweep vs truth.
    corr_err_us = (poly(pos) - te_ms) * 1e3
    corr_rms = float(np.std(corr_err_us))
    # Naive (assume ADC == nominal 48k, no anchoring) for comparison.
    naive_ms = pts0 + (pos - P0) / spm
    naive_rms = float(np.std((naive_ms - te_ms) * 1e3))
    print(f"anchor fit: residual {resid_us:.1f} µs | corrected RMS {corr_rms:.1f} µs "
          f"| naive (uncorrected) RMS {naive_rms:.0f} µs")
    if corr_rms > 8.0:        # ~ sub-sample peak jitter; ADC drift fully removed
        fails += 1
    if naive_rms < 50.0:        # sanity: the ADC error must actually be large
        fails += 1

    print("PASS" if fails == 0 else f"FAIL ({fails})")
    return 1 if fails else 0


if __name__ == "__main__":
    import sys
    sys.exit(_selftest())
