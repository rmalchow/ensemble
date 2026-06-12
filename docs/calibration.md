# Coherence Measurement & Calibration

> Status: design spec. Implemented in three independent pieces (1 → 2 → 3).
> Piece 3 is independent of 1 & 2 and may be built first for an early visible win.

Ensemble keeps players in sync with an NTP-style clock follower
(`internal/clock`) and a PI rate-servo that resamples to correct drift
(`internal/sink/servo.go`). We trust this chain, but we have **never measured the
actual acoustic coherence** — the real, at-the-speaker offset between two players.
This document covers how we measure it, how we build that measurement into the
master as an on-demand calibration, and how we surface per-node sync health.

Three goals, three pieces:

1. **Prove we can measure it** — a lab experiment with a single microphone and two
   players: can we resolve the inter-player offset in samples / µs?
2. **Build calibration into the master** — a UI feature on master+input nodes to run
   the measurement on demand and isolate each player's hardware latency.
3. **Surface sync health on the Nodes page** — per-node servo drift, clock latency,
   and buffer underruns/restarts.

---

## Findings — what the microphone measured (2026-06)

Piece 1 is built and validated on the live cluster: two Raspberry-Pi players
(`pi01` → left, `pi02` → right) and a single USB microphone in the room. The
measurement is a wideband log-sine-sweep, time-interleaved L/R so the two
speakers never overlap, matched-filtered with sub-sample parabolic interpolation.
Each `pi02` sweep is referenced to the **midpoint of its bracketing `pi01`
sweeps** — that midpoint trick cancels the (large, common) playback-vs-microphone
clock rate exactly, leaving only the `pi01`↔`pi02` acoustic offset. Tooling:
[`tools/calib/`](../tools/calib/) (`lr_drift.py`, `compare_drift.py`).

A deliberate ~50 cm speaker move was recovered as **46 cm** purely from the
acoustic measurement, so the absolute scale is trustworthy.

### Inter-speaker coherence — ±716 µs RMS

![Inter-speaker drift](img/coherence_interspeaker.png)

Over a 12-minute run the two speakers held to **716 µs RMS**, peak-to-peak
**~2.7 ms**. The curve is a smooth thermal "bowl", not jitter — it wanders slowly
as the sound-card crystals warm up, with no fast hops. That matters acoustically:
the precedence (Haas) effect fuses two arrivals into one perceived source up to
roughly **5–10 ms**, so this offset stays comfortably inside the "single source"
range the whole time. **Honest read:** sub-millisecond most of the session, with
a slow multi-millisecond thermal excursion the rate-servo does not currently
cancel — inaudible, but real, and the thing Piece 2 calibration would null.

### What the servo can — and can't — see

![Microphone vs servo telemetry](img/compare.png)

We polled the master's per-node STATUS telemetry (`GET /api/playback/statuses`)
during the same run and plotted the players' **self-reported clock-offset
difference** (`pi02.offsetNs − pi01.offsetNs`) against what the microphone heard.
The correlation is only **r = +0.20**: the servo's books stay nearly flat while
the microphone sees the ~2.7 ms thermal wander. The drift therefore lives
**downstream of the clock the servo controls** — in the DAC / analog output path —
where the current chain is effectively *blind* to it. This is the central
motivation for acoustic calibration (Piece 2): close the loop on what the
speakers actually emit, not just on what their clocks agree to.

### vs the recording clock

![Playback drift vs recording clock](img/coherence_vsclock.png)

For completeness, `pi01`'s arrivals detrended of nominal cadence show the shared
playback-vs-recording-clock drift (~−3 ppm with small servo wiggle and visible
re-sync steps). Both speakers move together here — cluster re-syncs preserve
coherence; they don't split the speakers apart.

---

## Background: what the system already knows and does

- **Canonical audio:** 48 kHz, stereo, s16le, 20 ms frames (960 samples/ch = 3840 B).
- **Clock:** NTP-style over STREAM_PORT UDP, nanosecond units. The follower computes
  `offset = master_ns − local_ns` (median of the best-5-RTT samples) and exposes
  `MasterNow()`. Every playback node reports its `OffsetNs`/`RTTNs` to its master via
  the STATUS control payload (`internal/stream/control.go`, type `0x40`), landing in
  `source.Server.Statuses()` (`internal/source/server.go:113`).
- **Servo:** `internal/sink/servo.go` — a PI loop producing a playback-rate correction
  in ppm (±500, slew-limited). This is the drift correction we want to report.
- **Source fan-out:** `source.Server.ReleaseFrame(pts, payload)`
  (`internal/source/server.go:158`) stamps one wire header and writes the **same
  packet to every live subscriber** (`reg.live()`, line 178).

### Precision reality

At 48 kHz, **1 sample = 20.8 µs = 7.1 mm** of sound travel (c ≈ 343 m/s). With a
**single mic** and **time-interleaved** bursts (one player at a time), each player's
directly measured quantity is:

```
T_arrival − T_intended_emit  =  clock_offset + hardware_latency + distance / c
```

- **Differences between players** = the real perceived offset a listener hears.
- The system **already knows `clock_offset` per node** (above). Subtracting the known
  offset isolates `hardware_latency + propagation`; fix or measure the mic→speaker
  distances and you isolate **hardware latency** itself — that is goal #2, and it
  falls out of the same recording.
- **Honest target:** sample-level (~20 µs) *relative* offset, sub-sample with sweep
  interpolation. That is already finer than the network clock sync (RTT-floored at
  tens-to-hundreds of µs on Wi-Fi), which is exactly what makes the acoustic
  measurement a useful, independent validation of the sync chain. **"ns" is not
  physically meaningful here** — report in samples / µs.

### Why one player at a time

A single mic hears a sum of delayed copies; it cannot separate two simultaneous
players reliably. So calibration **interleaves**: play the sweep on player A, settle,
then player B, etc., against one continuous recording. The intended emit PTS of each
burst is known exactly, so each player's arrival is recovered independently.

---

## Piece 1 — Lab coherence experiment (Python prototype)

**Goal:** answer "can we measure inter-player offset?" entirely outside the Go repo,
and produce a **validated DSP** whose math Piece 2 mirrors in Go.

Lives in `tools/calib/` (Python, numpy/scipy). No repo/runtime dependency on Python —
this is a development tool.

### Signal

A **logarithmic sine sweep** (chirp), e.g. 100 Hz → 12 kHz over ~1.0 s, windowed at
both ends. Sweeps have excellent autocorrelation and let us recover the impulse
response by deconvolution (Farina method) — robust to room reflections and noise.
Generate a reference WAV at 48 kHz, plus its analytic inverse filter.

### Apparatus & procedure (manual)

1. Single mic → one continuous recording (any interface; `arecord`, `pw-record`, or
   the interface's own tool) at 48 kHz.
2. Place two real players in the room. For the prototype, drive playback by hand —
   either via the existing test paths (`cmd/soundcheck/main.go`, `internal/sink/tone.go`)
   or by dropping the sweep WAV in as a file source. Emit on player A, settle (~1 s),
   then player B. No new Go code in this piece.
3. Stop recording. You now have one WAV containing two interleaved sweep arrivals plus
   a rough log of which burst was which.

### Analysis (`tools/calib/`)

- **Arrival estimation:** for each burst window, either (a) cross-correlate /
  matched-filter the recording against the reference sweep, or (b) deconvolve to the
  impulse response and take its peak. Both give the **arrival sample** per player.
- **Sub-sample refinement:** parabolic (or sinc) interpolation around the correlation
  peak → sub-sample arrival time.
- **Report:** per-player arrival, inter-player offset in **samples + µs + ms**, and the
  **noise floor** (repeat N runs, report mean ± stddev).

### Validation

Inject a *known* delay on one player by setting its `outputDelayMs` via the UI (the
sink re-anchors playout, `SetDelayOffset`). Confirm the measured offset tracks the
injected value within ~1–2 samples. This proves the measurement and the system's own
delay control agree.

### Deliverables

- `tools/calib/sweep.py` — reference sweep + inverse filter generation.
- `tools/calib/analyze.py` — arrival estimation + interpolation + reporting.
- A short results note (measured offset, repeatability) committed alongside.

**Complexity: Medium** — DSP iteration, zero repo risk.

---

## Piece 2 — Master-built calibration feature

Productize Piece 1: a **Calibrate** button, orchestrated by the master, that runs the
interleaved measurement live and reports per-player results.

### The constraint that shapes the design

`ReleaseFrame` **broadcasts** — it writes the same packet to every subscriber, and the
registry (`internal/source/registry.go`) keys subscribers only by their observed
`netip.AddrPort`. There is **no node-id → subscriber mapping** (node identity arrives
on a separate control path, `server.go:384`). So we **cannot target one player through
the fan-out**, and broadcasting the sweep would make every player chirp at once —
defeating the single-mic method.

**Resolution — Option A: direct unicast burst.** The master already resolves any
peer's address via `Cluster.DialCandidates(playerID)` (`internal/group/deps.go`) plus
that node's `SourcePort` from the snapshot. The calibration burst is sent **straight to
the chosen player's SOURCE_PORT**, bypassing the broadcast fan-out. No registry change,
no wire change, production hot path untouched.

### Signal injection

- **Sweep generator** implementing the `MediaSource` interface (`internal/group/deps.go`),
  synth pattern as in `internal/sink/tone.go` / `cmd/soundcheck/main.go` →
  `internal/audio/calib.go`. Returns `io.EOF` after the planned burst length.
- **`source.Server.ReleaseFrameTo(addr, gen, pts, payload)`** — new method, stamps the
  header exactly as `ReleaseFrame` (`server.go:168-176`) and `writeUDP`s to a single
  addr (`server.go:213`). Records the exact first-frame PTS for the DSP.
- The target's sink must be **armed at the burst `gen`** — run the burst inside the
  player's live calibration session (the orchestrator arms it).

### Mic capture — reuse the existing path (no new backend)

`internal/audio/input.go:71 RawCapture` / `OpenRawCapture(ctx, device)` already exists
**for exactly this** — raw 48 kHz stereo s16le straight off `pw-record`/`arecord`,
**no silence-on-underflow** (the live `input:` source inserts silence and would shred a
calibration recording; see the file comment at `input.go:65`). The orchestrator opens
it once for the whole run, reads the window into a buffer, closes.

Device enumeration is already wired end-to-end: `internal/audio/devices.go:27
ListInputDevices()` → boot probe (`cmd/ensemble/main.go`) → `NodeView.InputDevices`
→ **already in the UI snapshot**. The mic selector needs no new plumbing.

### UI — the Calibration section

Shown on a node that is **a group master AND has the `input` capability**.

- **Input capability** = `"input"` present in `capabilities.sources` (existing helper
  `effHas("input")`, `web/src/components/NodeRow.svelte:95`).
- **Master role** is not on `NodeView`; derive it client-side from the snapshot — a
  node is its group's master when `group.master === node.id`. Reuse/extend
  `web/src/lib/derive.js` rather than duplicating the server-side `roleAndGroup`
  (`internal/api/handlers.go:65`).
- **Gate:** `show = isMaster && effHas("input")`.
- **Controls:** mic/input selector (from `node.inputDevices`), optional player
  multi-select (default = all group members with `playback`), **Calibrate** button.
  Button **disabled while a playback session is active** (calibration needs the source
  server idle).
- **Results:** per-player table — arrival, raw offset vs reference player, isolated
  hardware+propagation latency (after subtracting known clock offset).

### Orchestration + result transport (async job)

Calibration takes seconds, so it is an async job, not a blocking request.

- **`POST /api/calibrate`** (register in `internal/api/api.go`, handler in
  `handlers.go`, DTO `CalibrateReq{inputDevice, players[]}` in `dto.go`) → `202 {jobId}`,
  or `409` if a job is already running. Proxied to the master via the existing
  `/api/<id>/…` node proxy.
- **`group.Engine.Calibrate(ctx, device, players)`** (new method on the `api.Group`
  interface, `internal/api/deps.go`):
  1. Open `RawCapture(device)` — one continuous recording for the whole run.
  2. **Per player, serially:** arm a unicast calibration session to that player, emit
     the sweep starting at a known master PTS (anchor via `Clock.MasterNow()`), settle,
     advance. Record `(playerID, burstStartPTS)` markers against the recording's sample
     index (sample index ↔ master time via the capture-start instant).
  3. Compute each player's arrival with the **Go port of Piece 1's DSP** (new
     `internal/calib` package, pure numeric).
  4. Subtract each player's known `OffsetNs` (`srcSrv.Statuses()[playerID]`) → isolated
     `hardware_latency + propagation`.
- **`GET /api/calibrate`** → `{state: running|done|error, progress, results[]}`.
  Ephemeral, in-memory on the master (mirrors the queue's pull-on-demand pattern,
  `handlers.go` `handleQueueList`). The UI polls while running.
- **Mutual exclusion:** a `calibrating` flag under `engine.mu` — refuse calibration
  while a session is live, refuse `Play` while calibrating (mirror the tone path's
  `ErrBusy`, `internal/sink/tone.go:25`).

### Duration

~2–3 s per player (sweep + buffer/lead + acoustic settle). 4 players ≈ 11 s, 8 ≈ 21 s —
confirms the async-job design.

### Out of scope (deliberate follow-up)

Auto-applying results to each node's `outputDelayMs` (`SetDelayOffset`). First cut
**reports** the numbers; applying them is a later, opt-in step.

**Complexity: High** — job lifecycle, per-player arming, PTS↔sample alignment, DSP
port, concurrency guards.

---

## Piece 3 — Nodes-page telemetry (independent)

Surface per-node sync health: **servo drift (max/avg ppm)**, **protocol-corrected clock
latency (offset/RTT)**, and **buffer underruns/restarts**.

### Counters — sink

In `internal/sink/sink.go:617 checkStarvationLocked` (the 2 s watchdog):

- Count a **restart** at the RESTART-fire site (`restartHit`, ~lines 621-631).
- Count a **disarm** at the disarm site (`p.armed = false`, ~line 332 / the watchdog's
  second trip).
- Count an **underrun episode** on the first watchdog trip.

Add `restarts, disarms, underruns uint64` to the `Playout` struct (near `restartHit`,
`sink.go:43`), incremented under `p.mu`.

(Note: `Silence` already counts silence frames; the new counters track *episodes*, which
is what an operator wants to see.)

### Servo ppm max/avg

The servo already produces ppm each `observe()` (`internal/sink/servo.go`). Add a
**windowed max** (short ring of recent samples) and an **EMA mean** to the servo
telemetry block; expose `ratePPMMax()` / `ratePPMAvg()`. Windowed, not
session-cumulative, so the numbers reflect *current* drift behaviour.

### Clock RTT fix

`RTTNs` is currently hard-wired to 0 in the status closure (`cmd/ensemble/main.go`,
the `StatusStats` wiring). Thread the follower's real RTT from `clockFol.Stats()` so
"protocol-corrected latency" is meaningful.

### Contracts / DTO / UI

- `contracts.SinkStats` (`internal/contracts/contracts.go:103`): add `Restarts`,
  `Disarms`, `Underruns`, `RatePPMMax`, `RatePPMAvg`. Populate in `Playout.Stats()`
  (`sink.go:362`).
- `internal/api/dto.go`: extend `SinkStatsResp` and its mapper with lowercase-camel
  keys per **D19** (`ratePPM` precedent → `ratePPMMax`, `ratePPMAvg`, `restarts`,
  `disarms`, `underruns`).
- `web/src/components/NodeRow.svelte`: a new **"sync health"** section (pattern at the
  existing `node-section` blocks) showing drift max/avg, offset/RTT, restart/underrun
  counts.

### Distribution — per-row status fetch

Sink stats are **not** in the gossiped snapshot — they are served by each node's own
`/api/status` (`handlers.go:29`). So `NodeRow` fetches the target node's status via the
existing proxy (`base(node.id) + "/status"`) on an interval. No new transport, no
gossip bloat — the data already exists per node. (Do **not** push high-rate counters
into `NodeView`/the cluster doc.)

**Complexity: Low–Medium** — counters and DTO are mechanical; windowed ppm and the
per-row fetch cadence are the only judgment calls.

---

## Verification

- **Piece 1:** inject a known `outputDelayMs` on one player; confirm the Python
  measurement tracks it within ~1–2 samples; report repeatability stddev over N runs.
- **Piece 2:** on a real master+input node, run Calibrate over ≥2 players; capture the
  WAV and cross-check the Go result against the Python prototype on the same recording;
  confirm isolated-latency numbers are stable and that calibration is refused during
  playback (and `Play` refused during calibration).
- **Piece 3:** `go test ./...`; force a starvation (briefly kill the source) and confirm
  restart/underrun counters increment; confirm drift max/avg and offset/RTT render per
  node in the Nodes UI via the proxied status fetch.

## Decisions (override on review)

| Fork | Decision |
|------|----------|
| Mic apparatus | Single mic, players interleaved (one at a time). |
| Signal source | Built into the master. |
| Single-player transport | Direct unicast burst (`ReleaseFrameTo`), bypassing fan-out. |
| Calibration gate | Visible on master+input; button disabled during playback. |
| DSP | Prototype in Python, port validated algorithm to Go. |
| Result lifetime | Ephemeral in-memory job, polled; no auto-apply (follow-up). |
| Players default | All current group members with `playback`. |
| Units | Report samples / µs / ms — not ns. |
