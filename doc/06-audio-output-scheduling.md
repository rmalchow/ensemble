# 06 — Audio output & scheduling

> **Scope.** This document specifies the *output half* of an Ensemble player node: the
> `AudioSink` implementations, the render loop that maps a group's
> `Timeline.NowSample()` to this node's physical playout, the **corrected content‑domain
> drift PI loop**, hardware‑buffer‑aware scheduling via `AudioSink.Delay()`, and the
> per‑node channel role / gain / `HWDelayUs` trim.
>
> Read the [spine](./README.md) first. This section elaborates the **locked decisions**
> D12 (audio out) and D13 (channel role), the canonical `AudioSink` contract
> ([§6.1](./README.md#61-audio-output--internalaudiosink)) and the `Timeline` contract
> ([§6.2](./README.md#62-group-timeline--clock--internalclock-internalgroup)). It uses
> those exact names and **must not** redefine them.
>
> **Siblings.** Where another section owns a thing, this one references it and stops:
> - **[04 — clock-and-groups](./04-clock-and-groups.md)** owns `Timeline`/`ClockSource`,
>   `NowSample()`, `streamGen`, and the group lifecycle. We *consume* the timeline.
> - **[05 — audio-streaming-protocol](./05-audio-streaming-protocol.md)** owns the
>   receive side (UDP → FEC recover → decode → **ring input**). We *drain* that ring.
> - **[07 — config-and-replication](./07-config-and-replication.md)** owns `ConfigDoc`,
>   `NodeRecord` (`Channel`, `GainDB`, `HWDelayUs`), and the device string. We *read* it.
> - **[09 — ui-screens](./09-ui-screens.md)** owns the **calibration UI** that drives the
>   calibration flow (MVP: operator enters `HWDelayUs` by ear after `/calibrate/play`). We
>   *apply* the value it produces; the built‑in signal + `/calibrate/play` are specified in
>   §5.3 here.
>
> **Reuse.** The `resampler` and `ring` designs are lifted as‑is from the proven
> `mpvsync` renderer (per spine §5). The `drift` loop is lifted **with a correction**:
> mpvsync regulated an output‑domain error that the actuator could not move. The
> corrected content‑domain loop is the central contribution of this section (§3).

---

## 0. The output pipeline at a glance

One node, one group, one output device. The producer fills a jitter ring; the consumer
drains it into the sink; a control tick steers the resampler so playout tracks the group
timeline to sub‑millisecond accuracy.

```
            (from 05: udp → fec → decode)                         this document (06)
   recv ───────────────────────────────────▶ recv‑ring ──┐
   (group stream, canonical 48k/stereo,                   │
    chunks tagged sampleIndex on the                      │  producer goroutine
    group timeline — wire §6.4)                           ▼
                                          ┌───────────────────────────────────────┐
                                          │ Resampler (near‑unity, ratio = content │
                                          │   per output frame; §3 actuator)       │
                                          │      │                                 │
                                          │      ▼                                 │
                                          │ channel‑select + GainDB (§5)           │
                                          │      │                                 │
                                          │      ▼                                 │
                                          │ playout Ring (~LeadMs jitter buffer)   │
                                          └───────────────────────────────────────┘
                                                          │  consumer goroutine
                                                          ▼
                                          AudioSink.Write (blocks = playout pacing)
                                                          │
                          control tick (20 ms) ───────────┤  reads NowSample()+Delay()
                                                          ▼
                                                    DAC  ───▶ speaker
```

Two rings exist in a follower; do not conflate them:

| Ring | Owner | Domain | Purpose |
|---|---|---|---|
| **recv‑ring** | [05](./05-audio-streaming-protocol.md) | network jitter | absorbs packet jitter/reorder before decode |
| **playout Ring** | **06 (this doc)** | playout jitter | absorbs producer scheduling jitter ahead of the sink |

This document's `Ring` is the **playout** ring. The recv‑ring is upstream and out of
scope here except as the producer's input.

---

## 1. `AudioSink` implementations

The seam is the canonical interface from spine [§6.1](./README.md#61-audio-output--internalaudiosink); reproduced
verbatim, do not redefine:

```go
type AudioSink interface {
    Start(rate, channels int) error
    Write(frames []float32) (n int, err error) // interleaved; blocks for backpressure
    Delay() (samples int, ok bool)             // outstanding in device; ok=false => coarse
    Close() error
}
// Runtime backend registry (NOT a build-time switch — every backend is compiled into the
// one pure-Go binary and probed at startup; details below):
//   Probe() []Backend                                   // backends that actually work here, minus config-disabled
//   Open(preferred []string, device string) (AudioSink, error)
//   type Backend struct{ Name string; Precise bool }    // "alsa"(direct ioctl, precise) | "exec:aplay"/"exec:pw-play"(coarse)
// A node with zero usable+enabled backends reports Render=false (control/media-only).
```

`Write` returns the number of **float32 samples** consumed (not frames); `Delay` reports
**samples‑per‑channel** still outstanding in the device. Both conventions are load‑bearing
for the scheduling math in §2/§4 — honor them exactly.

### 1.1 Runtime backend registry — `Probe` / `Open`

Selection is a **runtime** choice, **not** a build‑time one. **Every backend is compiled
into the one pure‑Go binary** (no `-tags alsa`, no build‑tagged twin files, no cgo variant,
no compile‑time `NewSink`). At startup the registry **probes** each backend and keeps only
the ones that *actually work on this machine*; per‑node config then **subtracts disabled
paths** and fixes a preference order; `Open` opens the best remaining backend.

There are exactly **two tiers**:

- **precise** — `alsa`, talking to the kernel sound device **directly via ioctl** (§1.3);
- **coarse** — `exec:*`, a player subprocess (§1.2);
- and if neither tier yields a usable backend, the node reports **`Render=false`** (§1.5).

```go
type Backend struct {
    Name    string // "alsa" | "exec:aplay" | "exec:pw-play"
    Precise bool   // true => Delay() returns a precise hardware figure (kernel ioctl)
}

// Probe tries every compiled-in backend, returns those that actually work here, minus
// config-disabled, in preference order. Cheap, run once at startup.
func Probe(cfg ProbeConfig) []Backend

// Open opens the first backend in `preferred` that is present in the probe result.
// `preferred` is the per-node order (config); `device` is the per-node device string.
func Open(preferred []string, device string) (AudioSink, error)
```

Each backend's probe is a **real** liveness check, not a "is the device node present" guess:

| Backend | `Precise` | Probe (must succeed to be usable) | `Delay()` |
|---|---|---|---|
| `alsa` (direct kernel ioctl) | **true** | `open()` a `/dev/snd/pcmC*D*p` node **and** complete the `HW_PARAMS`/`PREPARE` ioctl setup successfully | `SNDRV_PCM_IOCTL_DELAY` (precise) |
| `exec:aplay` | false | `aplay` resolvable on `PATH` | `(0,false)` coarse |
| `exec:pw-play` | false | `pw-play` resolvable on `PATH` | `(0,false)` coarse |

A backend whose device node exists but **cannot be configured/opened** (busy, owned by a
sound server, unsupported format) is **not** returned by `Probe` — presence of the
`/dev/snd` node is necessary but not sufficient. This is what lets a headless/NAS box (or a
box whose card is owned by PipeWire/Pulse) truthfully report no usable precise sink and fall
back to `exec:*` or to `Render=false` (§1.5).

`device` is the **per‑node device string** from the node's `ConfigDoc` record (spine §6.5
`NodeRecord` — the device field; `node.Run` threads it into `Open`). `""` maps to each
backend's `"default"`. The `rate`/`channels` format is committed later when the renderer
calls `Start(rate, channels)` on the opened sink.

**Pure‑Go / cross‑compile constraint, preserved.** The precise `alsa` backend talks to the
kernel **directly through ioctls** on the `/dev/snd/pcmC*D*p` character device using
`golang.org/x/sys/unix` (`unix.Syscall(SYS_IOCTL, …)` / `unix.IoctlSetInt` and friends) —
**no `libasound`, no `dlopen`/purego, no cgo**. The whole binary therefore stays **pure‑Go
and cross‑compiles to arm64 with no C toolchain** while *still* getting a precise outstanding
‑frame figure straight from the kernel. The hard project constraint
(`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` must pass) holds for the entire
build — there is **no** cgo variant and **no** build‑tagged twin in the normal path (a
cgo/purego path can only ever resurface for *optional* Opus, later — never for the sink).

> **Verification spike.** The direct‑ioctl `alsa` backend is a **verification spike**: it must
> be proven on real hardware (a Raspberry Pi + DAC HAT) before it is relied on, because the
> ALSA ioctl ABI (`snd_pcm_*` uapi structs, the `SYNC_PTR` mmap dance) is intricate and
> kernel/driver‑version sensitive. It **assumes direct hardware access** — i.e. no sound
> server (PipeWire/Pulse/JACK) owns the card; if one does, the probe fails and the node uses
> the coarse `exec:*` tier (e.g. `pw-play`) instead.

### 1.2 `exec` backends — coarse, always‑available (pure‑Go)

`ExecSink` backs the `exec:aplay` / `exec:pw-play` backends: it spawns a player subprocess
and writes interleaved **S16_LE** PCM to its stdin. It needs no audio library at all (only
the player binary on `PATH`), so it is the universal fallback the registry can almost always
offer. The pipe is the entire mechanism:

- **`Start(rate, channels)`** launches the player and grabs its stdin pipe. Default
  command template (configurable so PipeWire/Pulse boxes can swap in `pw-play`/`paplay`/
  `ffplay`):
  ```
  aplay -q -t raw -f S16_LE -r {rate} -c {channels} -D {device} -
  ```
  `{rate}`/`{channels}`/`{device}` are substituted at `Start`. Do **not** inflate the
  player's internal buffer — the jitter buffer is our `Ring`, not aplay's.
- **`Write(frames)`** clamps to `[-1,1]`, converts f32 → S16_LE (round‑to‑nearest, scale
  `32767`, symmetric so `+1.0` never wraps the rail), and writes to stdin. The pipe gives
  **natural backpressure**: aplay drains at the DAC rate, so once its kernel buffer fills,
  the `Write` blocks — and that block *is* the playout pacing. Returns `len(frames)` on
  success. A broken pipe / dead player surfaces as an error so the renderer can recover.
- **`Delay() → (0, false)`** — a pipe gives **no readback** of outstanding DAC samples.
  This forces the renderer onto the coarse content‑domain estimate (§4.2). Deliberate
  accuracy tradeoff for the portable default.
- **`Close()`** closes stdin (signalling drain), waits for the process; idempotent.

### 1.3 `alsa` backend — precise (direct kernel ioctl, pure‑Go) — **verification spike**

`alsaSink` backs the `alsa` backend: it implements the seam by driving the kernel ALSA PCM
device **directly through ioctls** on the character node `/dev/snd/pcmC<card>D<dev>p`, using
`golang.org/x/sys/unix` only — **no `libasound`, no `dlopen`/purego, no cgo, no headers, no C
toolchain**, so it ships in the same pure‑Go arm64 binary as the rest. Its sole reason to
exist is **precision**. This backend is a **verification spike**: prove it on a Pi + DAC HAT
before relying on it, and note it **assumes direct hardware access** (no sound server owns
the card — §1.1).

The ioctl set (ALSA PCM uapi, the same ABI `libasound` itself sits on):

- **`Start`** `open()`s the `p` (playback) node `O_RDWR`, then runs the standard uapi
  bring‑up: `SNDRV_PCM_IOCTL_HW_REFINE`/`SNDRV_PCM_IOCTL_HW_PARAMS` to fix
  `FORMAT_FLOAT_LE` (interleaved RW), `channels`, `rate`, and the period/buffer sizes
  (≈100 ms buffer; the `Ring` is the real jitter buffer), then `SNDRV_PCM_IOCTL_SW_PARAMS`
  and `SNDRV_PCM_IOCTL_PREPARE`. Blocking mode *is* the backpressure.
- **`Write`** submits interleaved float frames via `SNDRV_PCM_IOCTL_WRITEI_FRAMES` in a loop
  until all are accepted; returns consumed **samples**. On `-EPIPE` (underrun) it
  `SNDRV_PCM_IOCTL_PREPARE`s to recover and **surfaces the underrun** (`errUnderrun`,
  comparable via `errors.Is`) so the renderer treats it as a gross error and reseeks.
  Transient `-ESTRPIPE` (suspend) recovers and retries silently.
- **`Delay() → (frames, true)`** — the **precise** outstanding‑sample count straight from the
  kernel via **`SNDRV_PCM_IOCTL_DELAY`** (or, equivalently, `SNDRV_PCM_IOCTL_SYNC_PTR` and
  then `appl_ptr − hw_ptr`). This is the figure the drift loop wants for sub‑ms control. A
  negative/failed query degrades to `ok=false` (coarse model), never lies.
- **`Close()`** `SNDRV_PCM_IOCTL_DRAIN`s then `close()`s the fd; idempotent.

### 1.4 Why `Delay()` precision matters

`Delay()` is the only window into what the DAC is *actually* playing right now versus what
the renderer has *handed off*. Its precision sets the floor on the controllable error and
therefore the steady‑state inter‑node offset:

| Backend | `Delay()` | Source of estimate | Steady‑state offset |
|---|---|---|---|
| `alsa` (`Precise=true`) | `SNDRV_PCM_IOCTL_DELAY`, `ok=true` | kernel hardware pointer | sub‑millisecond, stable |
| `exec:*` (`Precise=false`) | `(0,false)` | coarse content model (§4.2) | few‑ms‑stable |

Crucially — and this is the heart of §3 — what `Delay()` returns is **output‑domain**
(samples the DAC has yet to consume). The corrected loop converts that into a
**content‑domain** quantity before regulating it. Precision of `Delay()` improves the
*estimate* of buffered content; it does **not** by itself fix controllability. The fix in
§3 is what makes the loop converge regardless of which backend the registry opened (precise
`alsa` vs coarse `exec:*` — the coarse path uses the fallback model of §4.2).

### 1.5 Capability detection & advertisement

The registry's probe result is also how this node tells the cluster **what it can do**. The
flow at startup is:

```
Probe() ──▶ [usable backends here]
            ─ config-disabled         (per-node config subtracts unwanted paths, §1.1)
            = usable+enabled sinks ──▶ NodeRecord.Caps  ──gossip──▶ rest of cluster
```

The **usable + enabled** backend set (probed, minus config‑disabled) directly populates this
node's `Capabilities` (spine §6.5):

- `Caps.Sinks` = the backend `Name`s that survived probe **and** config (e.g.
  `["alsa","exec:aplay"]`, or `[]`).
- `Caps.Render` = `len(Caps.Sinks) > 0` — **`Render=false` when there is no usable + enabled
  sink**.

`Caps` is written into this node's own `NodeRecord.Caps` and **gossiped** to the cluster
(spine §6.5; replication owned by [07](./07-config-and-replication.md)); group/profile
negotiation ([04](./04-clock-and-groups.md)) consumes it. This document only *produces* the
audio‑output portion (`Render`, `Sinks`) from the probe; the codec/FEC/rate fields are filled
by their owners.

**`Render=false` ⇒ the renderer is not started.** When no sink is usable+enabled, `node.Run`
does **not** open a sink and does **not** start the render loop of §2 — the §2–§7 pipeline is
simply absent on that node. Such a node is **control / media‑only** (a *sink‑less node*, spine
D17): it can still serve UI/API, store media, run the clock, and be a group **master / stream
origin** (decode→encode→FEC→unicast for *other* listeners) — it just is not itself a listener.
That role split is owned by [04](./04-clock-and-groups.md); see also spine D16/D17. The render
loop below therefore assumes `Render=true`; a `Render=false` node never reaches it.

---

## 2. The render loop: `Timeline.NowSample()` → local playout

The renderer is the audio sibling of the video `Scheduler`: both chase the same group
timeline, but where the Scheduler actuates mpv playback speed, the renderer actuates a
**resample ratio** against an `AudioSink`. It runs two goroutines under `Run(ctx)`.

### 2.1 Parameters

```go
type RendererParams struct {
    Rate     int           // canonical rate, 48000
    Channels int           // sink output channels, 2
    LeadMs   int           // playout buffer ahead, default 300
    Tick     time.Duration // control tick, default 20ms
    Drift    DriftParams
}
func DefaultRendererParams() RendererParams // {48000, 2, 300, 20ms, DefaultDriftParams()}
```

`LeadMs` sizes the playout `Ring` (the producer fills to `LeadMs`; `Ring.Cap()` is
`2×LeadMs` of slack). `Tick` is the control cadence. `SetParams` re‑tunes `LeadMs`/`Drift`
live (next tick); `Rate`/`Channels` changes require a restart.

> **Canonical values.** The starting `Rate`/`Channels`/`LeadMs`/`Tick` and all drift‑PI gains
> below are **defined once in [Appendix A.12](./A-appendix-algorithms-and-pinned-choices.md#a12-starting-parameters)**
> (the single source of truth); the numbers shown here are illustrative defaults that **must
> track A.12** — do not treat a value in this doc as authoritative if it ever diverges.

### 2.2 Producer goroutine

Keeps the `Ring` filled to ~`LeadMs` from the active stream:

```
recv‑ring → Resampler.Process(ratio) → channel‑select + GainDB → Ring.Write
```

- Reads decoded canonical‑rate frames (the recv side, [05](./05-audio-streaming-protocol.md)).
- Runs them through the `Resampler` at the **current ratio** (the §3 actuator). The
  resampler is 4‑tap cubic Hermite, near‑unity (`1 ± MaxPPM`, `MaxPPM = 200`); it carries
  phase + tap history across calls so block boundaries are seamless.
- Channel‑selects to the node's role and applies `GainDB` (§5).
- `Ring.Write` returns a short count when full; the producer backs off (~2 ms) and
  retries — backpressure from the consumer side propagates here as a full ring.

The producer holds the resampler lock across `Process` so a concurrent `SetRatio` from the
control goroutine cannot race the interpolation.

### 2.3 Consumer / control goroutine

Drains the `Ring` into the sink between ticks and runs the control law on each tick:

- **Drain:** `Ring.Read` up to a chunk, `sink.Write` it. The sink's `Write` blocks for
  backpressure, so the drain loop is **paced by the DAC**. Each successful write advances
  the running counter `framesWritten` (per‑channel frames handed to the sink since the
  last reseek baseline). On a ring underrun the consumer does **not** push silence (that
  would advance `framesWritten` and fight the upcoming reseek) — it bumps `underruns` and
  lets the next tick observe the gross error.
- **Tick (every `Tick`):** the control law of §2.4.

### 2.4 Control tick — the mapping

Per tick the renderer maps the group timeline to a target playout position and steers the
ratio. The exact ordering and the **corrected** error definition:

1. `sample, playing, ok := timeline.NowSample()` (spine §6.2). `!ok` ⇒ no sync yet ⇒
   **hold** (silence, no advance, no reseek), `RenderTick.HaveSync=false`, return.
2. Read the node's `ConfigDoc` record + group media. If outside any active clip ⇒ gap ⇒
   feed silence, return.
3. **Stream/clip change** (`streamGen` changed, or selected media changed): resolve the
   asset; if not ready, feed silence and return; else (re)prime the source, `reseek` to
   the want position, and hold `settleTicks` (≈2) while the buffer primes.
4. Compute **`wantContent`** — the content‑domain target (§3.2, includes the `HWDelayUs`
   trim of §5.3).
5. Compute **`playedContent`** — the content‑domain progress (§3.3, uses `Delay()`).
6. `errSamples = int(playedContent − wantContent)`. Run the PI loop (§3.4):
   - `DriftHold` ⇒ `resampler.SetRatio(ratio)` (anti‑redundant: skip if `|Δ| < 1e‑7`).
   - `DriftReseek` ⇒ hard refill + re‑baseline (§6).
7. Populate `RenderTick` and fire the `OnTick` callback (UI/logging).

`RenderTick` mirrors the video `TickInfo`: `HaveSync, Playing, WantSample, PlayedSample,
ErrorSamples, RatioPPM, Action, Underruns, Clip`.

`NowSample()` returns the index in **canonical‑rate frames** (48 kHz). All want/played
math below is in canonical‑rate per‑channel frames unless stated otherwise.

---

## 3. The corrected drift PI loop (content‑domain) — **critical**

This is the central correction relative to mpvsync. Read §3.1 (the bug) before §3.2–§3.5
(the fix); the fix only makes sense against the bug.

### 3.1 The mpvsync bug — an uncontrollable regulated variable

mpvsync's renderer regulated an **output‑domain** error:

```
played_output = framesWritten(handed to sink) − Delay()        // OUTPUT domain
err           = played_output − wantSample
```

`framesWritten` counts frames *handed to the output device*, and `Delay()` is in the same
output domain (samples the DAC has not yet drained). So `played_output` advances at the
**DAC crystal rate** and is entirely outside the controller's authority.

The actuator, meanwhile, is the **resample ratio**, which is *content per output frame*:
it changes how fast the resampler **consumes source/content** to produce one output frame.
It does **not** change how fast the DAC drains output — the crystal does that, fixed.

The result is a structural controllability failure:

> The regulated variable (`played_output`) is paced by a clock the actuator (resample
> ratio) cannot influence. Moving the ratio changed *content consumption* while the loop
> measured *output drain*. The PI loop's gain on the regulated error was effectively zero;
> it **could not move the error it was regulating**. In practice it rode the `±MaxPPM`
> clamp and relied on periodic **reseeks** to yank the error back — audible, and never
> truly converged. The integral term wound up against a wall.

Concretely: a +40 ppm crystal drift makes the DAC drain 40 ppm fast. The loop wants to
slow down, so it commands ratio `< 1`. But ratio `< 1` only makes the resampler consume
*content* slower — it produces the same number of output frames per second, which the DAC
still drains at its own +40 ppm. `played_output` keeps running away; the error grows until
it trips `HardErrSamp` and reseeks. Repeat forever.

### 3.2 The fix — regulate a content/source‑domain error

Regulate the variable the actuator **can** move: cumulative **content** (source frames)
the resampler has consumed, minus the source‑equivalent still buffered downstream.

```
wantContent = group sampleIndex (from Timeline.NowSample())     // CONTENT domain, canonical frames
            + HWDelayUs trim (per-node fixed offset, §5.3)
```

`wantContent` is what the group timeline says this node's *content* position should be
*right now* at the speaker (the `HWDelayUs` trim places "now" at the speaker rather than at
the sink handoff — §5.3).

```
playedContent = sourceConsumed                       // cumulative SOURCE frames the resampler has consumed
              − sourceEquivalentBuffered             // source-frames still sitting downstream of the resampler
```

where

```
sourceEquivalentBuffered = (ringFrames + deviceDelayFrames) × ratio_content_per_output
```

- **`sourceConsumed`** — running total of input frames the `Resampler` has consumed
  (returned as `consumed/channels` from `Process`), since the reseek baseline. This is in
  the **content domain** and advances exactly as fast as the producer feeds content into
  the resampler.
- **`ringFrames`** — `Ring.Len()/Channels`, output‑domain frames buffered in the playout
  ring (resampled, not yet handed to the sink).
- **`deviceDelayFrames`** — `Delay()` (precise on ALSA; coarse model on exec, §4.2),
  output‑domain frames in the device not yet played.
- **`ratio_content_per_output`** — the current resample ratio expressed as content frames
  per output frame, converting the downstream output‑domain backlog back into the content
  domain so it is dimensionally consistent with `sourceConsumed`. Near unity; using `1.0`
  here is acceptable since `|ratio−1| ≤ 200 ppm` makes the conversion error on a
  few‑hundred‑ms backlog sub‑sample.

**Why this is now controllable.** The resample ratio is *content consumed per output
frame*. The DAC drains output at its fixed crystal rate `f_out`, so output frames leave
the system at `f_out` regardless of ratio. Therefore:

```
d(sourceConsumed)/dt ≈ ratio × f_out
d(sourceEquivalentBuffered)/dt ≈ 0 in steady state (backlog held near LeadMs)
⇒ d(playedContent)/dt ≈ ratio × f_out
```

The ratio now sits **directly in the time‑derivative of the regulated variable**. Command
ratio `< 1` and `playedContent` genuinely slows; command ratio `> 1` and it genuinely
speeds. The actuator has authority over the error, the PI loop has nonzero loop gain, and
it converges to `errSamples → 0` without riding the clamp and without relying on reseeks.
Reseeks revert to what they should be: a *startup / underrun / gross‑error* recovery, not
a steady‑state crutch.

### 3.3 Exact error definition

```
// All quantities in canonical-rate per-channel frames, since the reseek baseline.
ratio       := current resample ratio (content per output frame), near 1.0
ringFrames  := Ring.Len() / Channels
devFrames, ok := sink.Delay()
if !ok { devFrames = coarseDeviceDelay()  /* §4.2 */ }

sourceEquivalentBuffered := int64(round(float64(ringFrames+devFrames) * ratio))
playedContent := baseSourceConsumed + sourceConsumed - sourceEquivalentBuffered

wantContent   := sampleIndex + hwDelayOffset   // §3.2, §5.3

errSamples    := int(playedContent - wantContent)
//  + errSamples  => we have consumed past where the timeline wants  => AHEAD => slow (ratio<1)
//  - errSamples  => we lag the timeline                              => BEHIND => speed (ratio>1)
```

`baseSourceConsumed` is the `sourceConsumed` captured at the last reseek so `playedContent`
and `wantContent` share an origin. The **sign convention is identical to mpvsync's**
(`error = played − want`, positive ⇒ ahead ⇒ ratio `< 1`); only the *domain* of `played`
changed. That keeps `DriftLoop` itself unchanged — the correction lives entirely in how the
renderer computes `errSamples` it feeds to `DriftLoop.Update`.

### 3.4 The PI update

`DriftLoop` is reused unchanged from the resampler/ring sibling design. It is a stateful PI
controller over the error in **samples**, with the resample ratio as actuator:

```go
type DriftParams struct {
    Kp            float64 // proportional, samples->ppm.        Default 0.05
    Ki            float64 // integral,    samples->ppm per tick. Default 0.005
    MaxPPM        float64 // ratio-trim clamp, ppm.             Default 200
    HardErrSamp   int     // |error| above this => reseek.      Default 2400 (=50ms@48k)
    IntegralClamp float64 // anti-windup on Ki*integral, ppm.   Default 200
}

func (d *DriftLoop) Update(errSamples int) (DriftAction, float64)
```

`Update`:

1. **Gross error first.** `if abs(errSamples) > HardErrSamp → (DriftReseek, 1.0)` and do
   **not** integrate (the caller will reseek + reset everything, §6).
2. **Integrate + anti‑windup.** `integral += errSamples`, then clamp so
   `Ki·integral ∈ [−IntegralClamp, +IntegralClamp]` ppm.
3. **PI law (note the negative sign — ahead ⇒ negative ppm ⇒ ratio < 1):**
   ```
   ppm   = -(Kp·errSamples + Ki·integral)
   ppm   = clamp(ppm, ±MaxPPM)        // ±200 ppm
   ratio = 1 + ppm·1e-6
   return DriftHold, ratio
   ```

The `Resampler.SetRatio` independently re‑clamps to `1 ± MaxPPM·1e‑6` as a belt‑and‑braces
guard, so the actuator can **never** make an audible pitch shift even if a gain is
mistuned.

The default values are **canonical in [Appendix A.12](./A-appendix-algorithms-and-pinned-choices.md#a12-starting-parameters)**;
the table restates them only to explain each knob's *effect* (A.12 wins on the numbers):

| Knob | Default (per A.12) | Effect |
|---|---|---|
| `Kp` | 0.05 | proportional pull; higher = faster, more ratio chatter |
| `Ki` | 0.005 | removes standing offset; the integrator nulls steady crystal drift |
| `MaxPPM` | 200 | ±0.02% ratio clamp — guarantees inaudible pitch |
| `HardErrSamp` | 2400 | 50 ms @ 48 k — reseek threshold (§3.5) |
| `IntegralClamp` | 200 | keeps `Ki·integral` well under `MaxPPM` (anti‑windup) |

With the **content‑domain** error feeding this loop, a steady tens‑of‑ppm crystal drift
nulls in a few seconds at the 20 ms tick without overshoot, and the integrator settles to a
**non‑clamped** value equal to the standing drift — the diagnostic that the bug is fixed.
Under mpvsync the integrator pinned at `IntegralClamp` and the error never nulled.

### 3.5 Gross‑error reseek threshold

`|errSamples| > HardErrSamp` (default 2400 = 50 ms @ 48 k) ⇒ `DriftReseek`. This is for
**startup, underrun, and stream discontinuity** only — not steady‑state correction. On
reseek the loop returns ratio `1.0`, does **not** integrate the gross error, and the
renderer performs the §6 hard refill (which calls `DriftLoop.Reset()` to zero the
integrator). With the corrected loop, steady‑state operation should **never** trip this
threshold; a recurring reseek in steady state is a regression signal that the error domain
has been confused again.

### 3.6 Convergence test must model the **real plant**

A passing convergence test must model the actual physics, not a toy where "apply ratio to a
played counter" trivially nulls (that toy is exactly what hid the mpvsync bug — it modeled
the actuator as if it drove the regulated variable directly, which on real hardware it does
**not**).

The harness MUST model:

- **DAC drain at a fixed crystal rate.** Output frames leave the device at `f_out =
  Rate·(1 + crystalPPM·1e‑6)` *independent of the commanded ratio*. The crystal offset is
  the disturbance the loop must reject.
- **Ratio sets source→output coupling.** The resampler consumes `ratio` content frames per
  output frame produced; output is produced/drained at `f_out`. So `sourceConsumed`
  advances at `ratio·f_out` and the buffered backlog responds to the producer/consumer rate
  difference — exactly the plant of §3.2.
- **`Delay()`/ring backlog evolve from that drain**, not as free inputs. The test feeds the
  *modeled* device delay back into `playedContent`, closing the loop the way the real
  system does.

Assertions: with a constant `crystalPPM` (e.g. ±40 ppm), `errSamples → 0` and the
**commanded ratio settles to `1 − crystalPPM·1e‑6`** (not at the clamp), the integrator
holds a finite non‑clamped value, and **no reseek** occurs after settling. A test that
nulls against an output‑domain model, or that only checks "ratio<1 when error>0", does not
catch the bug and must be rejected in review.

---

## 4. Hardware‑buffer‑aware scheduling

The buffer lead and the device buffer both sit *downstream* of the actuator, so both must
be subtracted from progress (§3.3) and both factor into how `wantSample`/`playedSample` are
positioned in time.

### 4.1 Where the lead and device buffer live

```
producer fills ──▶ [ playout Ring  ~LeadMs ] ──▶ [ device buffer ~Delay() ] ──▶ DAC ──▶ now
                    output-domain backlog          output-domain backlog
```

- **`LeadMs`** (the producer's fill target) is the jitter headroom: the producer stays
  ~`LeadMs` ahead so transient scheduling stalls do not underrun. It is *content the
  resampler has already produced but the sink has not yet taken* → counted in `ringFrames`.
- **Device buffer** (`Delay()`) is *output the sink has taken but the DAC has not yet
  played* → counted in `devFrames`.

Both are **output‑domain backlog between "handed off" and "heard"**. `playedContent`
subtracts the source‑equivalent of `(ringFrames + devFrames)` so that `playedContent`
measures content that has actually **reached the DAC**, which is what the group timeline's
`sampleIndex` refers to. Forgetting either term biases the node early by that amount —
`LeadMs` of it would be a gross, audible lead.

### 4.2 `Delay()` precise vs coarse

```go
func (r *Renderer) coarseDeviceDelay() int {
    // exec sink: no readback. Estimate outstanding ≈ wall time since the last
    // Write × rate, i.e. what should still be draining. Few-ms stable; the ratio
    // trim nulls the residual bias because the bias is (near-)constant.
    elapsed := time.Since(r.lastWriteAt).Seconds()
    return int(float64(r.params().Rate) * elapsed)
}
```

| Path | `devFrames` source | Bias character | Loop response |
|---|---|---|---|
| ALSA (`ok=true`) | `SNDRV_PCM_IOCTL_DELAY` (kernel ioctl) | sub‑sample | nulls to sub‑ms |
| exec (`ok=false`) | coarse wall‑time model | small, near‑constant | integrator absorbs the constant; nulls residual |

The key property: even the coarse estimate has a **near‑constant** bias, and a PI
integrator rejects a constant disturbance. So the corrected loop converges on *both* sinks;
the precise sink merely converges tighter (sub‑ms vs few‑ms steady offset). This is only
true because the error is content‑domain — a constant output‑domain bias did the loop no
good under the mpvsync formulation.

---

## 5. Channel role, gain, and `HWDelayUs`

These are the per‑node personality knobs (spine D13, `NodeRecord` §6.5). They are applied
in the producer's channel‑select/gain stage (role + gain) and in the want computation
(`HWDelayUs`). The canonical stream is **stereo**; this node's `NodeRecord.Channel` selects
its role out of that stereo pair.

### 5.1 Channel role (`stereo` / `left` / `right`)

`NodeRecord.Channel ∈ {"stereo","left","right"}` (spine §6.5, D13). Selection maps the
canonical stereo frame to this node's `Channels`‑wide output:

| `Channel` | Output meaning |
|---|---|
| `"stereo"` | passthrough: src ch0→out ch0, src ch1→out ch1 |
| `"left"` | take src ch0, fan out to **all** output channels |
| `"right"` | take src ch1, fan out to **all** output channels |

A stereo *pair* is two physical nodes, one `"left"` and one `"right"`, each fanning its
selected source channel to both of its own output channels (so a stereo DAC plays the same
mono on both pins). Selection is a per‑frame pick in the producer, after resampling, before
gain.

### 5.2 Gain (`GainDB`)

`NodeRecord.GainDB` is a per‑node linear trim applied after channel select:

```
g = 10^(GainDB/20)   // 0 dB => 1.0
out = g · selectedSample
```

`0 dB` short‑circuits to `1.0` (no multiply). Clamping to `[-1,1]` is the sink's job (S16
conversion / ALSA float), not the gain stage's.

### 5.3 `HWDelayUs` — static calibration trim (fixed sample offset)

`NodeRecord.HWDelayUs` is the per‑node **hardware/acoustic latency trim** — the fixed
offset (speaker distance, DAC/amp latency, room placement), persisted to the `ConfigDoc`.
This document *applies* it; the calibration UI ([09](./09-ui-screens.md)) drives the
**calibration flow** that arrives at the value.

**Calibration flow (MVP = manual measurement).**

- **Built‑in calibration signal.** There is **no external clip**: the signal is generated
  in‑process at the canonical rate (48 kHz, A.12) and is **identical across all nodes**. Per
  1 s it is a ~1 ms full‑scale **click** + a ~200 ms **1 kHz tone** + **silence** for the
  remainder. The click gives a sharp transient for offset judgement; the tone gives an
  audible steady reference. The exact signal spec lives in Appendix A.
- **`POST /api/v1/calibrate/play`** `{groupId | nodeIds, durationSec}` (endpoint owned by
  [08](./08-http-api-reference.md)) plays this signal **synchronously** on the selected
  nodes — i.e. scheduled on the shared group timeline so every selected node emits the same
  click at the same group instant, which is precisely what makes a between‑node offset
  audible.
- **MVP measurement is manual.** The operator judges the residual offset by ear (or a
  phone mic) and **enters `HWDelayUs` directly** (a `PATCH` on the node record via
  [09](./09-ui-screens.md)/[08](./08-http-api-reference.md)). P6 acceptance = the trim
  applies and the array audibly tightens.
- **Automated measurement is a documented later enhancement.**
  **`POST /api/v1/calibrate/measure`** — upload a recording of the played signal, run a
  click+tone **cross‑correlation** against the known built‑in signal, and return a suggested
  `HWDelayUs` — is **not** in the MVP; it is specified as a future enhancement only.

It is applied **once** as an integer sample offset on the want side (not in the audio data,
not as a resampler perturbation — it is a fixed phase, not a rate):

```go
hwDelayOffset := int64(math.Round(float64(node.HWDelayUs) * 1e-6 * float64(p.Rate)))
wantContent   := sampleIndex + hwDelayOffset
```

A positive `HWDelayUs` advances this node's want target (it must play *earlier* in content
to compensate a *later* acoustic arrival), so it counteracts that node's extra physical
delay and the wavefronts line up at the listening position. Because it is added to
`wantContent`, the drift loop steers `playedContent` to meet it — the trim is held
**exactly** in steady state, not approximately.

> Naming: the spine's `NodeRecord.HWDelayUs` is the same quantity D13 lists as the per‑node
> `DelayUs` trim. This document uses `HWDelayUs` (the field name in §6.5). See the
> inconsistency note at the end.

---

## 6. Startup, seek (`streamGen` change), underrun

All three converge on the same primitive — a **hard refill / reseek** — differing only in
the trigger and the target position. The reseek is the only place the want/played baseline
is re‑established.

### 6.1 `reseek(want)` — the primitive

```
1. Seek the source to `want` (content position).         // re-prime input at the target
2. Resampler.Reset()                                      // drop phase + tap history
3. Ring.Reset()                                           // drop stale playout backlog
4. DriftLoop.Reset()                                      // zero the integrator
5. baseSourceConsumed = want ; sourceConsumed = 0         // re-baseline content progress
6. framesWritten = 0 ; lastWriteAt = now                  // re-baseline output counters
7. SetRatio(1.0)                                           // start neutral
8. settle = settleTicks (≈2)                               // ignore a couple ticks while priming
```

After a reseek, `playedContent` is clip‑relative to `want` and the loop re‑converges from
neutral. The `settle` window suppresses the control law for ~2 ticks so the half‑primed
ring/device backlog does not produce a spurious gross error and oscillate.

### 6.2 Startup

No sync until `Timeline.NowSample()` returns `ok=true`. Until then: hold silence, no
advance, no reseek (`HaveSync=false`). On first `ok` with an active clip, the renderer
resolves the asset and `reseek`s to `wantContent` for that instant, then enters steady‑state
control. The initial buffer prime (filling `LeadMs`) happens during the settle window.

### 6.3 Seek / stream change (`streamGen`)

`streamGen` (wire header, spine §6.4) bumps on any media change or seek on the group
timeline; [04](./04-clock-and-groups.md) owns it, [05](./05-audio-streaming-protocol.md)
delivers it. On a `streamGen` change the renderer treats the stream as discontinuous:
resolve the (possibly new) asset, drop the old source/ring/resampler state, and `reseek` to
the new `wantContent`. The content baseline reset (step 5) is what keeps `playedContent`
honest across the discontinuity — without it, `sourceConsumed` would carry the old stream's
count and the first post‑seek error would be garbage.

### 6.4 Underrun

Two underrun sources, one response:

- **Producer fell behind** (ring drains to empty): the consumer bumps `underruns` and does
  **not** push silence. The next tick sees `playedContent` lagging `wantContent` past
  `HardErrSamp` and reseeks.
- **Device underrun** (ALSA `-EPIPE`): `sink.Write` surfaces `errUnderrun`; the renderer
  treats it as a gross error and reseeks immediately (the sink already re‑`PREPARE`d the PCM
  via `SNDRV_PCM_IOCTL_PREPARE`).

Either way the recovery is `reseek(wantContent)` + `settle`. Underruns are counted into
`RenderTick.Underruns` for the UI; a rising count under steady network conditions points at
an undersized `LeadMs` or a starved producer, not at the drift loop.

---

## 7. Full control‑tick pseudocode

```text
// runs every RendererParams.Tick (default 20ms) on the consumer/control goroutine.
// All sample quantities are canonical-rate per-channel frames.
func tick():
    p    := params()                         // snapshot live params (LeadMs, Drift, Rate)
    info := RenderTick{ Underruns: r.underruns }
    defer fire OnTick(info)

    // (1) timeline gate
    sample, playing, ok := timeline.NowSample()       // spine §6.2
    if !ok:
        info.HaveSync = false
        return                                         // hold: no advance, no reseek
    info.HaveSync, info.Playing = true, playing

    // (2) locate active clip for this node on the group media/timeline
    node := configDoc.Node(r.nodeID)                   // Channel, GainDB, HWDelayUs, device
    clip, active := activeClip(group.media, sample)
    if !active:
        feedSilence(); info.Clip = ""; return          // gap / before first / after last

    info.Clip = clip.Media

    // (3) stream/clip discontinuity → resolve + reseek
    if streamGenChanged() or clip.Media != r.loadedMedia:
        path, ready := resolveAsset(clip)
        if !ready:
            feedSilence(); return                      // transcode in flight; do not block
        if !loadSource(path, node):                    // assert canonical 48k/2; snapshot role+gain
            feedSilence(); return
        r.loadedMedia = clip.Media
        want := wantContent(clip, sample, node, p)
        reseek(want)                                   // §6.1
        r.settle = settleTicks
        // fallthrough into the steady-state computation this same tick

    // (4) content-domain WANT (includes HWDelayUs trim, §3.2 / §5.3)
    hwOff := round(node.HWDelayUs * 1e-6 * p.Rate)
    want  := clipContentOffset(clip, sample, p) + hwOff
    info.WantSample = want

    // (5) content-domain PLAYED (the corrected variable, §3.3)
    ratio       := r.lastRatio                         // content-per-output, near 1.0
    ringFrames  := Ring.Len() / p.Channels             // output-domain backlog (downstream of actuator)
    devFrames, dok := sink.Delay()                     // ALSA precise; exec => !dok
    if !dok:
        devFrames = coarseDeviceDelay()                // §4.2 wall-time model
    bufferedContent := round(float64(ringFrames+devFrames) * ratio)
    played := r.baseSourceConsumed + r.sourceConsumed - int64(bufferedContent)
    info.PlayedSample = played

    // (6) settle window after a reseek/load: hold the law while the buffer primes
    if r.settle > 0:
        r.settle--
        info.Action  = DriftHold
        info.RatioPPM = (r.lastRatio - 1) * 1e6
        return

    // (7) error + PI law
    errSamples      := int(played - want)              // + => ahead => slow (ratio<1)
    info.ErrorSamples = errSamples
    action, ratio2  := drift.Update(errSamples)        // §3.4
    info.Action     = action
    info.RatioPPM   = (ratio2 - 1) * 1e6

    switch action:
    case DriftHold:
        applyRatio(ratio2)                             // SetRatio; skip if |Δ|<1e-7
    case DriftReseek:                                  // gross error: startup/underrun/discontinuity only
        reseek(want)                                   // §6.1
        r.settle = settleTicks
```

`sourceConsumed` is advanced by the **producer** each `Resampler.Process` (it returns input
frames consumed); the control goroutine only reads it (under the resampler lock) and
re‑baselines it on reseek. `framesWritten`/`lastWriteAt` are advanced by the **drain**
between ticks and feed only the *coarse* `Delay()` model — in the corrected loop they are
no longer the regulated quantity, which is the whole point of §3.

---

## 8. Cross‑reference summary

| Concern | Owner | This doc |
|---|---|---|
| `Timeline.NowSample()`, `streamGen`, group lifecycle | [04](./04-clock-and-groups.md) | consumes (§2.4, §6.3) |
| recv‑ring, FEC, decode (the producer's input) | [05](./05-audio-streaming-protocol.md) | drains into producer (§0, §2.2) |
| `AudioSink`, runtime backend registry (`Probe`/`Open`), all backends | **06 (here)** | §1 |
| Capability detection → `Caps.Render`/`Caps.Sinks` (sink half) | **06 (here)** | §1.5 |
| `NodeRecord.Caps` gossip/replication; group role split | [04](./04-clock-and-groups.md), [07](./07-config-and-replication.md) | produces sink caps (§1.5) |
| Content‑domain drift PI loop | **06 (here)** | §3 |
| `Delay()`‑aware scheduling | **06 (here)** | §4 |
| `NodeRecord.Channel` / `GainDB` / `HWDelayUs` schema | [07](./07-config-and-replication.md) | reads + applies (§5) |
| Built‑in calibration signal + `/calibrate/play` (synchronous) | **06 (here)** | §5.3 |
| `HWDelayUs` calibration flow / UI (MVP: manual entry) | [09](./09-ui-screens.md) | applies the result (§5.3) |
```
