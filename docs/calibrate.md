# Acoustic auto-calibration

A mode that **measures each node's real output delay with a microphone** and
sets its `outputDelayMs` automatically, so every speaker in a group is
phase-aligned at the listening position — no more guessing with the manual
slider. Status: **implemented** (`internal/calibrate` + `POST /api/calibrate`;
see [DECISIONS D48](arch/DECISIONS.md)). The per-node volume-ramp attribution
(§4) and a lower-latency capture path remain follow-ups for very noisy rooms.

---

## 1. Why

Even with the clock sync and rate servo keeping every node at the same *rate*,
each node has a different fixed *phase* offset before sound reaches the ear:
the device buffer (ALSA vs a pipe player vs an ESP I2S DMA), the DAC/amp/Bluetooth
chain, and air propagation. The rate servo cannot fix this (rate ≠ phase). Today
it's the manual per-node **output delay** slider ([D36](arch/DECISIONS.md));
calibration measures and sets it for you.

The control knob already exists and is uniform across node types: every node —
software member *or* ESP thin node ([esp32.md](esp32.md) §1b) — exposes
`PATCH /api/node {outputDelayMs}`. Calibration just measures the right value and
writes it.

---

## 2. The idea in one paragraph

Pick a node that has a **microphone** (a laptop/phone running ensemble, or a
dedicated mic node). It plays a **reference clip on a loop** through the group's
normal playback path, then, **one node at a time**, mutes every other member and
records the mic. Because the calibrating node is a clock-synced group member, it
knows the *master-clock time* of every mic sample and the *master-clock pts* at
which each reference frame was emitted. Cross-correlating the recording against
the known reference gives the node's **scheduled-emit → ear delay** `D[n]` in
master-clock nanoseconds. From the set `{D[n]}` it computes and `PATCH`es an
`outputDelayMs[n]` that lines every node up.

---

## 3. The reference signal

A short signal optimized for **delay estimation under room reverb and noise**,
looped every ~1 s:

- A **logarithmic sine sweep** ("chirp"), ~100 ms, 500 Hz→8 kHz, windowed
  (raised-cosine edges, no clicks), followed by silence to the loop period.
- Matched-filter (cross-correlate the mic against the known sweep) → a sharp
  correlation peak whose lag is the delay, with sub-sample precision and strong
  rejection of broadband noise and reverberant tails (a sweep's autocorrelation
  is near-impulsive). MLS or a click train are alternatives; the sweep wins on
  SNR and is gentle on speakers.

It ships as a normal media file (`calibrate.wav` / a generated buffer) that the
group master plays like any track; calibration just `play`s it and loops.

---

## 4. Per-node measurement (the "down-then-up")

For each member `n` of the group, in turn:

1. **Isolate** — set every *other* member's volume to 0 and `n`'s to a known
   level via `PATCH /api/<id>/node {volume}`. Now the mic hears only `n`.
2. **Modulate for attribution** — ramp `n`'s volume in a known pattern over a
   couple of loop periods (e.g. 0 → full → 0, the user's "turn down then up").
   This is the robustness trick: the recorded **envelope** must track that ramp,
   which *confirms the measured peak belongs to `n`* and rejects a stray sound,
   a neighbour bleeding through, or HVAC noise. A measurement whose envelope
   doesn't correlate with the commanded ramp is discarded and retried.
3. **Record & correlate** — capture the mic for several loop periods, each
   sample stamped in master-clock time (`LocalToMaster(captureLocal)`). For each
   loop, cross-correlate the recorded sweep against the known reference; the peak
   lag, plus the known emit pts, yields `D` for that loop. Take the **median**
   across loops (rejects outliers from a transient noise).
4. **Result** — `D[n]` = median scheduled-emit → ear delay, master-clock ns,
   with a confidence (peak sharpness × envelope-correlation). Low confidence →
   repeat with more loops or flag the node.

The master's own loopback sink is measured the same way (it's just another
member). Live sources are paused; the loop runs at a fixed, known pts schedule
throughout.

---

## 5. Computing the delays

We want the **ear arrival time equal for all nodes**. Arrival for node `n`:

```
arrival[n] = C − outputDelayMs[n] + D[n]      (C common; deadline subtracts outputDelayMs, D36)
```

so `arrival` is equal for all when `outputDelayMs[n] = D[n] + K` for any common
`K`. `K` sets the group's overall latency; pick it to keep every value feasible:

- A node can only be **advanced** by up to ~`bufferMs` (you can't emit a frame
  before it's buffered), but **delayed** freely (just more buffering).
- If the spread `max(D) − min(D) ≤ bufferMs − margin`: choose `K = −min(D)` →
  `outputDelayMs[n] = D[n] − min(D)` (all ≥ 0; advance the slow chains toward the
  fastest; lowest added latency).
- Else: choose `K = −max(D)` → `outputDelayMs[n] = D[n] − max(D)` (all ≤ 0;
  delay the fast nodes toward the slowest; always feasible, adds `spread` of
  latency).

Round to ms, clamp to the backend's ±500 ms (D36), and write each via
`PATCH /api/<id>/node {outputDelayMs}`. Restore every member's volume.

**Propagation caveat.** `D[n]` includes air travel from speaker to mic, so this
aligns arrivals **at the mic's position** — exactly right when the mic sits at
the main listening spot. For rooms with independent listeners (each near their
own speaker), propagation should *not* be compensated: either place the mic
equidistant from the speakers, run calibration per room, or supply a per-node
speaker→mic distance to subtract (`distance / 343 m/s`). Calibration reports the
raw `D[n]` and the distance assumption it used.

---

## 6. Orchestration & API

Calibration runs on a node whose capabilities include a **microphone**
(`capabilities.sources` ⊇ `input`, or a dedicated `mic` capability). It drives
the group through existing primitives — `play` (the reference, looped),
per-node `SetVolume`, and its own mic capture — then writes `outputDelayMs`.

| Method/path | Action |
|---|---|
| `POST /api/calibrate` | `{group, micNode?, distancesM?}` → start a run on this (or the named mic) node; streams progress over the WS |
| `GET  /api/calibrate` | current run: phase, per-node `D`, confidence, computed `outputDelayMs` |
| `POST /api/calibrate/cancel` | abort; restore volumes, leave existing delays |

Run lifecycle: `acquire mic → play reference loop → for each member {isolate,
ramp, record, correlate} → compute → PATCH outputDelayMs → restore volumes →
report`. It is **idempotent and safe to cancel**: on cancel or error it restores
every member's pre-run volume and never half-writes the delay set (all-or-nothing
at the end).

**UI.** A **Calibrate** button on the group card, enabled when some group member
reports a mic. It shows live progress (which node is being measured, the ramp,
the running estimate), then a results table (`D`, confidence, applied
`outputDelayMs`) with **Apply** / **Discard**. A node with low confidence is
flagged for a manual retry or a hand-set delay.

---

## 7. Accuracy, edge cases, limits

- **Resolution** — at 48 kHz a 1-sample lag is ~21 µs; the sweep matched-filter
  resolves well under a millisecond, far finer than audible alignment needs
  (~a few ms).
- **Reverberant rooms** — the sweep's near-impulsive autocorrelation and the
  median-over-loops reject reflections; very live rooms reduce confidence (the
  report says so).
- **Mic placement** — aligns at the mic; §5's caveat applies. Document the
  assumption in the result.
- **Wi-Fi jitter** — the per-node servo and clock sync are already converged
  before measuring; calibration measures the *steady-state* phase, not the
  startup transient (it waits for `sink.synced` and a settled servo first).
- **Thin/ESP nodes** — measured and set identically; their `outputDelayMs` is a
  normal `PATCH` (esp32.md §1b), applied in the I2S playout deadline.
- **Failure** — any node that can't be confidently measured keeps its prior
  delay; the run reports it rather than guessing.

## 8. Acceptance

After a run on a group of 3+ nodes with a mic at the listening spot: re-measure
each node's arrival and confirm the spread is within a few ms (down from tens of
ms uncalibrated). A bench check: play a click track post-calibration and confirm
a single sharp transient at the mic instead of a smeared/flammed one. Add an e2e
leg using the `null`+`file` backends with synthetic, known per-node delays (no
real mic) to verify the correlation/compute/PATCH math end-to-end.
