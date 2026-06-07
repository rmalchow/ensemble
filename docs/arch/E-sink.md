# E — sink & playout

Source of truth: [docs/README.md](../README.md) (§8.5 sink/playout + rate servo,
§8.6 stop/end/getting-lost, §8.1 canonical format, §7 clock). Contracts:
[S-skeleton.md](S-skeleton.md) (`internal/contracts`: `Backend`, `DelayReporter`,
`Sink`, `SinkStats` incl. `RatePPM`/`Buffered`, `Clock`; `internal/stream`:
`FrameBytes`, `FrameSamples`, `FrameNanos`, `Channels`). Integrator decisions:
[DECISIONS.md](DECISIONS.md) — **D25** (rate servo), **D27** (backend registry,
amended), **D32** (`internal/dl` runtime loading), **D34** (alsa backend),
D21 (live settings / `Synced` live / `Write` may block), D3/D2
(`ENSEMBLE_OUTPUT`, capabilities assembled by K).

This piece owns `internal/sink/*` only. It implements:

1. a **named output-backend registry** (D27): `alsa` (raw device via
   runtime-loaded libasound, also a `DelayReporter`), `exec` (auto-pick pipe),
   `null` (timed discard), `file` (debug append). All four ship in the one and
   only build (D32 — no build tags); `alsa` registers itself only when the
   `internal/dl` dlopen probe of `libasound.so.2` succeeds (D34);
2. the **playout pipeline** on every member: jitter buffer → playout scheduler
   (timestamp-driven coarse anchor, silence gaps, late drops, gen gating,
   unsynced gate) → **4-tap Catmull-Rom fractional resampler** → backend;
3. the **continuous DAC rate servo** (D25): skew estimator → PI controller
   (clamped ±500 ppm, slew-limited) driving the resampler's playback rate, so
   crystal drift never pulls rooms apart;
4. a **2 s starvation watchdog** (§8.6) that calls an injected `RestartFunc`
   (G's subscriber issues the wire RESTART) before disarming.

Design stance: **one scheduler goroutine, one mutex** on the `Playout`; the
servo and resampler are plain helper structs touched only under that mutex (or
owned by the scheduler goroutine). No abstraction beyond the contract interfaces
fixed in S. Nothing here dials sockets or knows about transports — the receiver
(G) and group engine (H) call `Push`/`Reset`; the watchdog calls back out
through the injected `RestartFunc`. (D: "Sink `Push` is fire-and-forget; no
backpressure/close signal to G.")

---

## 1. Package / file layout

All files in `internal/sink/`, one package `sink`.

```
config.go          Config struct; ENSEMBLE_OUTPUT parsing lives in registry.go.
sink.go            Playout: implements contracts.Sink. New(cfg), Push/Reset/
                   Stats/Close. Owns the jitter buffer, servo, resampler, stats,
                   generation, scheduler goroutine. The only stateful type; one mutex.
jitter.go          jitterBuffer: bounded seq-keyed reorder buffer (insert/pop/
                   setOrigin/advance/reset/len). Pure; caller holds the mutex.
servo.go           rateServo: skew estimator + PI controller (±500 ppm clamp,
                   slew limit). Pure numeric struct; no goroutine.
resampler.go       resampler: 4-tap Catmull-Rom fractional resampler with a
                   fractional cursor across frame boundaries. Pure; no goroutine.
registry.go        Backend registry (D27): Register(name, factory), Open(spec),
                   built-in registrations alsa/exec/null/file, ENSEMBLE_OUTPUT
                   parse, capability probe (BackendNames, HasPlayback).
backend_alsa.go    alsaBackend: raw device via runtime-loaded libasound (D34,
                   internal/dl). Registers "alsa" only when dl.Open succeeds.
                   Implements contracts.Backend + DelayReporter. No build tag.
backend_exec.go    execBackend: pipes raw s16le 48k stereo into pw-play/pw-cat -p/
                   aplay/paplay (first on PATH). Stdin pipe, process lifecycle.
backend_null.go    nullBackend: timed discard; optional 20 ms pacing.
backend_file.go    fileBackend: append raw PCM to a debug file.

servo_test.go      rateServo convergence + clamp + slew (no goroutines).
resampler_test.go  Catmull-Rom continuity across frame boundaries, rate==1 identity.
jitter_test.go     jitterBuffer unit tests.
registry_test.go   Open(null/file/exec/auto), BackendNames, HasPlayback.
backend_test.go    null/file backend writes; exec skipped if no tool on PATH;
                   alsa skipped (t.Skip) when libasound is not loadable.
sink_test.go       Playout scheduler + servo integration (fake clock, fake DAC).
```

There is no build tag for alsa (D32 — build tags abolished). `backend_alsa.go`
is always compiled; in its `init()` it attempts `dl.Open` of `libasound.so.2`
and registers the name "alsa" only on success. On a host without the library the
probe yields `dl.ErrUnavailable`, the name "alsa" is simply absent from the
registry, so `capabilities.backends` omits it (D3) and `ENSEMBLE_OUTPUT=alsa`
errors cleanly.

---

## 2. Concrete Go API

### 2.1 `config.go`

```go
package sink

import (
	"log/slog"
	"time"

	"ensemble/internal/contracts"
)

// RestartFunc is the watchdog's escape hatch (§8.6). When playout starves for
// Watchdog (2 s) the sink calls it ONCE before disarming; G's subscriber turns
// that into a wire RESTART to the source ("I got lost, re-prime me"). The sink
// itself never touches the network. nil is allowed (no-op; tests / source-side
// loopback that re-arms via Reset anyway).
type RestartFunc func()

// Config configures one Playout for one session-capable member. Constructed
// once per node; BufferMs and Gen refresh per session via Reset/SetBufferMs.
type Config struct {
	Backend  contracts.Backend // output device (registry Open or a test fake); never nil
	Clock    contracts.Clock   // master-time translation (F); never nil
	BufferMs int               // playout lead: audio for pts hits device at pts+BufferMs (§8.5)
	Restart  RestartFunc       // watchdog hook (§8.6); may be nil
	Log      *slog.Logger      // component logger; defaulted if nil

	// Tunables; zero => default (overridable in tests).
	Capacity int           // jitter-buffer slot cap (default 256 frames ≈ 5.1 s)
	Watchdog time.Duration // starvation timeout (default 2 s, §8.6)
	now      func() int64  // local monotonic ns; default monotoNow (tests inject)
	servoCfg servoConfig   // PI gains / clamps; default tuned values (tests override)
}
```

`BufferMs` defaults to `contracts.DefaultBufferMs` (150); the group engine (H)
passes the per-group setting and may update it live across a `Reset` (D23 — a
settings change bumps the generation, so a new `Reset(gen)` carries the new
buffer target). `now` returns nanoseconds from a monotonic source
(`time.Now()` has a monotonic reading we read as ns via a package helper) so the
fake clock in tests supplies a deterministic counter. `servoCfg` is unexported;
production uses the tuned defaults, tests inject aggressive gains for fast
convergence.

### 2.2 `registry.go` — named backend registry (D27)

```go
package sink

// Factory builds a backend from its spec argument (the part after the colon in
// ENSEMBLE_OUTPUT, e.g. the path for "file:/tmp/x", "" for "null"/"exec").
type Factory func(arg string, log *slog.Logger) (contracts.Backend, error)

// Register adds a named backend factory. Called from each backend file's init().
// Replacing an existing name panics (programmer error). "exec", "null", "file"
// always register; "alsa" registers itself only when its dl.Open probe of
// libasound.so.2 succeeds (D34) — so on a host without the library the name is
// simply absent (no build tags, D32).
func Register(name string, f Factory)

// BackendNames returns the registered backend names, sorted, for
// capabilities.backends (§1; assembled by K, D3). Pure; no process spawn.
func BackendNames() []string

// HasPlayback reports whether a real (non-null) backend is usable on this host:
// true iff "exec" can resolve a player tool on $PATH, or an explicit non-null
// backend is registered and openable. Drives capabilities.playback (§1, D27).
// Pure lookup for "exec" (exec.LookPath), no spawn.
func HasPlayback() bool

// Open resolves ENSEMBLE_OUTPUT-style spec into a backend (D2/D27):
//
//	"" | "auto"           -> best available, in order alsa -> exec -> null: "alsa"
//	                          if registered (libasound loaded) and it opens; else
//	                          "exec" if a player tool is on $PATH; else "null".
//	"alsa"                -> alsaBackend; errors if "alsa" is not registered
//	                          (libasound not loadable) or the device won't open.
//	"exec"                -> first of pw-play, pw-cat -p, aplay, paplay on $PATH;
//	                          explicit "exec" with no tool degrades to "null" with
//	                          a WARN (don't fail the node).
//	"null"                -> nullBackend (timed discard)
//	"file:/abs/path"      -> fileBackend appending raw PCM
//	"<name>" / "<name>:a" -> any registered factory, arg after the first colon
//
// Returns the backend and the resolved name (for /api/status + logging). An
// explicit-but-broken request (bad file path, unknown name, "alsa" when
// libasound is not loadable) errors; "auto" never errors (degrades to null).
func Open(spec string, log *slog.Logger) (contracts.Backend, string, error)
```

`ENSEMBLE_OUTPUT` is read by K (D2/D3) and passed as `spec`; E does not read the
environment itself except a thin `os.Getenv` convenience used by tests. The
registry pattern keeps adding `alsa` (and any future backend) to one
`Register("alsa", …)` call gated on its `dl.Open` probe — no switch to touch,
no build tag (D32).

### 2.3 backends

```go
// execBackend pipes canonical PCM (s16le 48k stereo) into a player subprocess.
type execBackend struct {
	cmd  *exec.Cmd
	in   io.WriteCloser // stdin pipe
	log  *slog.Logger
	once sync.Once
}

// execTools is the auto-pick order (§8.5). First found on $PATH wins.
var execTools = []struct{ name string; args []string }{
	{"pw-play", []string{"--rate", "48000", "--channels", "2", "--format", "s16", "-"}},
	{"pw-cat",  []string{"-p", "--rate", "48000", "--channels", "2", "--format", "s16", "-"}},
	{"aplay",   []string{"-q", "-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw", "-"}},
	{"paplay",  []string{"--raw", "--rate=48000", "--channels=2", "--format=s16le"}},
}

func newExecBackend(log *slog.Logger) (*execBackend, error) // resolves+spawns first tool
func (b *execBackend) Write(frame []byte) error             // validates len, writes all bytes
func (b *execBackend) Close() error                         // close stdin, Wait w/ timeout, kill on hang
// execBackend deliberately does NOT implement DelayReporter (no snd_pcm_delay):
// the servo falls back to backpressure inference for it (§8.5, §3.5).

// nullBackend discards frames; optionally paces one frame / FrameDuration so
// playout timing is exercised like a real device. Tests disable pacing.
type nullBackend struct {
	mu      sync.Mutex
	written uint64
	last    time.Time
	pace    bool
	sleep   func(time.Duration) // injectable; default time.Sleep
}
func newNullBackend() *nullBackend
func (b *nullBackend) Write(frame []byte) error
func (b *nullBackend) Close() error
func (b *nullBackend) Written() uint64

// fileBackend appends raw PCM to a debug file (no pacing; scheduler paces).
type fileBackend struct {
	mu sync.Mutex
	f  *os.File
}
func newFileBackend(path string) (*fileBackend, error)
func (b *fileBackend) Write(frame []byte) error
func (b *fileBackend) Close() error
```

All `Write` implementations reject `len(frame) != stream.FrameBytes` with an
error (defensive; the resampler always emits exactly `FrameBytes`). Backends own
their own mutex because `Close` may race the scheduler's last `Write`.

```go
// alsaBackend writes canonical PCM (s16le 48k stereo) straight to an ALSA PCM
// device via runtime-loaded libasound (D34, internal/dl). It is the one v1
// backend that also implements contracts.DelayReporter (snd_pcm_delay), giving
// the servo an *exact* skew measurement (§3.5).
type alsaBackend struct {
	lib    *dl.Lib   // libasound handle (held for the device lifetime)
	pcm    uintptr   // snd_pcm_t* from snd_pcm_open
	log    *slog.Logger
	closed bool

	// bound symbols (purego); see init() / dl.Func.
	open      func(pcmp *uintptr, name string, stream, mode int32) int32
	setParams func(pcm uintptr, fmt, access, channels, rate, softResample, latencyUs int32) int32
	writei    func(pcm uintptr, buf *byte, frames uint) int  // returns frames or -errno
	recover   func(pcm uintptr, err, silent int32) int32
	delay     func(pcm uintptr, delayp *int) int32           // delayp in frames
	close      func(pcm uintptr) int32
	strerror  func(err int32) string
}

func newAlsaBackend(log *slog.Logger) (*alsaBackend, error) // dl.Open + snd_pcm_open + set_params
func (b *alsaBackend) Write(frame []byte) error             // snd_pcm_writei, recover on xrun
func (b *alsaBackend) DeviceDelay() (int64, bool)           // snd_pcm_delay frames -> ns
func (b *alsaBackend) Close() error                         // snd_pcm_close
```

`backend_alsa.go` carries **no build tag** (D32). Its `init()` calls
`dl.Open([]string{"libasound.so.2","libasound.so"}, …)` to dlsym-verify the
required symbols and, only on success, `Register("alsa", newAlsaBackend)`. A
missing or wrong-version library yields `dl.ErrUnavailable` (soft), the name
"alsa" is never registered, and the node degrades cleanly. The factory, when
invoked:

- **Open**: `snd_pcm_open(&pcm, "default", SND_PCM_STREAM_PLAYBACK, 0)`.
- **Params**: `snd_pcm_set_params(pcm, SND_PCM_FORMAT_S16_LE,
  SND_PCM_ACCESS_RW_INTERLEAVED, 2, 48000, 1 /*soft_resample*/,
  latencyUs)` where `latencyUs ≈ bufferMs·1000` — the device buffer target
  matches the playout lead.
- **Write**: `snd_pcm_writei(pcm, &frame[0], 960)` per canonical 20 ms period
  (`FrameSamples` = 960 frames/channel); on `-EPIPE` (underrun) or `-ESTRPIPE`
  (suspend) call `snd_pcm_recover(pcm, err, 1)` and retry once, else log and
  drop the frame (the scheduler keeps cadence, §3.4).
- **DeviceDelay**: `snd_pcm_delay(pcm, &frames)` → `frames·FrameNanos/FrameSamples`
  ns, `ok=true`; this is the `contracts.DelayReporter` path the servo prefers
  (§3.5). On error return `ok=false` so the servo falls back to inference.
- **Close**: `snd_pcm_close(pcm)`; idempotent via `closed`.

All ALSA calls run on the scheduler goroutine (the only caller of `Write`),
except `Close` which guards on `closed` under the Playout mutex like the other
backends.

### 2.4 `jitter.go`

```go
// slot holds one buffered frame's payload + pts. payload is owned (copied on
// Push) so the receiver may reuse its read buffer.
type slot struct {
	pts     int64
	payload []byte // exactly stream.FrameBytes
}

// jitterBuffer is a bounded seq-keyed reorder buffer. NOT goroutine-safe; the
// Playout mutex guards every call.
type jitterBuffer struct {
	slots   map[uint64]*slot
	cap     int
	nextSeq uint64 // seq the scheduler plays next
	hasNext bool   // false until the first frame fixes the seq origin
}

func newJitterBuffer(capacity int) *jitterBuffer
func (j *jitterBuffer) insert(seq uint64, pts int64, payload []byte) (stored bool)
func (j *jitterBuffer) pop(seq uint64) *slot
func (j *jitterBuffer) setOrigin(seq uint64) // fixes nextSeq on first frame
func (j *jitterBuffer) advance()             // nextSeq++ after play/silence
func (j *jitterBuffer) reset()               // empty + clear origin (new gen)
func (j *jitterBuffer) len() int
```

`insert` drops (returns false) when `seq < nextSeq` (already passed → late) or
the buffer is full and `seq` is not nearer-future than its furthest slot (then
the furthest is evicted to make room). Duplicate seq overwrites idempotently.

### 2.5 `servo.go` — skew estimator + PI controller (D25)

```go
package sink

// servoConfig holds the PI gains and limits. Defaults are tuned for a ~3 s skew
// window and gentle correction; tests override for fast convergence.
type servoConfig struct {
	Window   int64   // skew-averaging window, ns (default 3e9 = 3 s)
	Kp       float64 // proportional gain (ppm per ppm of measured skew)
	Ki       float64 // integral gain (ppm per (ppm·s) of accumulated skew)
	ClampPPM float64 // output clamp, ± (default 500, §8.5)
	SlewPPM  float64 // max |Δoutput| per update, ppm (default 5 ppm/update)
}

func defaultServoConfig() servoConfig // Window 3e9, Kp 0.3, Ki 0.05, Clamp 500, Slew 5

// rateServo turns a stream of (samplesConsumed, masterElapsed[, deviceDelay])
// observations into a playback-rate correction in ppm. One per session; Reset
// on generation change. Pure: no goroutine, no clock, no locking (the Playout
// mutex guards it).
type rateServo struct {
	cfg       servoConfig
	have      bool    // a baseline has been established
	baseCons  int64   // cumulative samples consumed at window start
	baseMaster int64  // master-clock ns at window start
	integ     float64 // PI integral accumulator, ppm·s
	outPPM    float64 // last emitted correction, ppm (slew-limited, clamped)
}

func newRateServo(cfg servoConfig) *rateServo

// observe folds one measurement and returns the updated correction in ppm.
//   consumedSamples : cumulative samples per channel the backend has consumed
//                     since session start (monotonic).
//   masterNanos     : current master-clock time (ns) for the same instant.
//   deviceDelayNs,ok: queued audio between Write and speaker if the backend is a
//                     DelayReporter; ok=false => fall back to backpressure
//                     inference (consumed-vs-elapsed only).
// The skew over the window is:
//     wantSamples = (masterNanos - baseMaster) * SampleRate / 1e9   // master says
//     gotSamples  = consumedSamples - baseCons                       // DAC did
//     skewPPM     = 1e6 * (gotSamples - wantSamples) / wantSamples
// When deviceDelayNs is available it refines gotSamples by subtracting the
// still-queued (not-yet-heard) samples: gotHeard = gotSamples - delaySamples,
// giving an exact "what the speaker has actually emitted" figure. The PI step:
//     integ += skewPPM * dtSeconds
//     raw    = -(Kp*skewPPM + Ki*integ)        // negative: DAC fast => slow down
//     out    = slew(clamp(raw, ±ClampPPM), prev, ±SlewPPM)
// A positive return means "produce samples faster" (resample ratio > 1).
func (s *rateServo) observe(consumedSamples, masterNanos, deviceDelayNs int64, ok bool) float64

// ratePPM returns the last correction (for SinkStats.RatePPM).
func (s *rateServo) ratePPM() float64

// reset clears baseline + integral for a new session.
func (s *rateServo) reset()
```

**Skew sign & loop polarity.** "DAC fast" means the device consumed *more*
samples than master-clock elapsed implies (`gotSamples > wantSamples` ⇒
`skewPPM > 0`); the device is draining the buffer, so we must *slow production*
→ the controller output is the negated PI sum, and we apply it as a resampler
ratio `1 + out/1e6` slightly **below** 1 (fewer output samples per input → the
buffer refills). Conversely a slow DAC yields negative skew, positive output,
ratio > 1. The window baseline (`baseCons/baseMaster`) is re-seeded whenever the
elapsed master time first exceeds `cfg.Window`, giving a sliding ~3 s average
without retaining a sample history (a single re-baselined difference is the
average rate over that span).

### 2.6 `resampler.go` — 4-tap Catmull-Rom fractional resampler

```go
package sink

// resampler converts a stream of input PCM frames into output frames at a
// fractional playback rate (≈ 1.0, nudged by the servo's ppm correction). It is
// a 4-tap Catmull-Rom interpolator with a fractional read cursor that persists
// across frame boundaries, run independently per channel (interleaved L/R).
//
// Catmull-Rom for fractional position t∈[0,1) between samples p1 and p2, with
// neighbours p0 (before) and p3 (after):
//
//   y(t) = 0.5 * ( (2*p1)
//                + (-p0 + p2)*t
//                + (2*p0 - 5*p1 + 4*p2 - p3)*t^2
//                + (-p0 + 3*p1 - 3*p2 + p3)*t^3 )
//
// Output sample k reads input position cursor += step, where
//   step = 1.0 / rate,  rate = 1 + ppm/1e6   (rate>1 => consume input slower =>
//   stretch => MORE output per input; rate<1 => compress). cursor's integer part
//   indexes p1; its fractional part is t. p0..p3 are cursor-1..cursor+2.
type resampler struct {
	// hist holds the last 3 input samples per channel (p0,p1,p2 carryover) so the
	// first output of a new input frame can interpolate across the boundary
	// without re-reading the previous frame. Indexed [channel][0..2].
	hist [stream.Channels][3]int32
	cursor float64 // fractional read position WITHIN the current input frame, in samples/ch
	rate   float64 // current playback rate (1 + ppm/1e6); set per output frame
	primed bool    // false until the first input frame seeds hist
}

func newResampler() *resampler

// setRate sets the resampling ratio from a ppm correction (clamped by the servo).
func (r *resampler) setRate(ppm float64)

// process consumes input frame `in` (exactly FrameBytes) and appends exactly one
// output frame (FrameBytes) of interpolated PCM into out[:0], returning out.
// Because rate ≈ 1, one input frame yields ~one output frame; the fractional
// cursor carries the sub-sample remainder across calls so there is no
// per-frame discontinuity. When the cursor would run past the input frame
// (rate>1, stretch) the resampler signals it needs the buffer to provide an
// extra silence/duplicate-free hold by returning needMore; when it falls behind
// (rate<1, compress) it may skip producing for one call. See §3.4 for the exact
// 1-in/1-out bookkeeping the scheduler relies on.
func (r *resampler) process(in []byte) (out []byte)

// reset clears history + cursor (new session / gen).
func (r *resampler) reset()
```

The interpolation formula above is the **load-bearing contract** of this file;
§3.4 specifies the cursor bookkeeping across frame boundaries precisely.

### 2.7 `sink.go` — `Playout` (implements `contracts.Sink`)

```go
package sink

// Playout is the per-node sink: jitter buffer → scheduler → resampler → backend,
// with the continuous rate servo and the starvation watchdog. Implements
// contracts.Sink. One scheduler goroutine, one mutex.
type Playout struct {
	mu      sync.Mutex
	jb      *jitterBuffer
	servo   *rateServo
	rs      *resampler
	gen     uint32
	armed   bool
	closed  bool
	stats   contracts.SinkStats // Played/Silence/LateDrop/StaleGen/RatePPM/Buffered
	lastPkt int64               // local-ns of most recent accepted Push (watchdog)

	// session servo accounting (samples the backend has consumed since Reset)
	originSeq  uint64
	originPTS  int64
	consumed   int64 // cumulative output samples/ch written to the backend
	restartHit bool  // RESTART already fired this starvation episode

	clock    contracts.Clock
	out      contracts.Backend
	delay    contracts.DelayReporter // out asserted to DelayReporter, or nil
	bufferNs int64
	cap      int
	watchdog time.Duration
	restart  RestartFunc
	now      func() int64
	log      *slog.Logger

	silence []byte
	wake    chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
}

// New builds a Playout and starts its scheduler goroutine (idle until the first
// Reset). cfg.Backend and cfg.Clock must be non-nil. The backend is type-
// asserted to contracts.DelayReporter once at construction (D25).
func New(cfg Config) *Playout

// Push enqueues a frame for playout (contracts.Sink). Non-blocking; drops+counts
// stale-gen / late frames; copies payload; signals the scheduler. (D: fire-and-
// forget — no return value, no backpressure.)
func (p *Playout) Push(gen uint32, seq uint64, pts int64, payload []byte)

// Reset arms the sink for a new generation: discards queued frames, resets servo
// + resampler, sets gen, clears per-session counters, re-establishes the seq/pts
// origin on the next Push. Carries the (possibly changed) BufferMs via SetBufferMs
// beforehand if H changed it live (D23).
func (p *Playout) Reset(gen uint32)

// SetBufferMs updates the playout lead live (D21/D23); takes effect on the next
// scheduled slot. Optional helper H calls around a settings-driven Reset.
func (p *Playout) SetBufferMs(ms int)

// Stats snapshots playout counters (contracts.Sink). Synced is read live from the
// clock; RatePPM is the servo's current correction; Buffered is jb.len().
func (p *Playout) Stats() contracts.SinkStats

// Close stops the scheduler and closes the backend (contracts.Sink). Idempotent.
func (p *Playout) Close() error
```

---

## 3. Control flow, goroutines, locking

### 3.1 Startup

`New(cfg)` allocates the jitter buffer (`cfg.Capacity` or 256), the servo
(`newRateServo(cfg.servoCfg|default)`), the resampler (`newResampler()`), the
pre-zeroed `silence` frame (`make([]byte, stream.FrameBytes)`), the `wake`
(buffered, cap 1) and `done` channels, type-asserts `cfg.Backend` to
`contracts.DelayReporter` (storing `delay` or nil), and starts **one** scheduler
goroutine. Until the first `Reset` the scheduler is *disarmed*: it blocks on
`wake`/`done` and does nothing (`armed=false`).

### 3.2 Arming (`Reset(gen)`)

Under the mutex: `jb.reset()`, `servo.reset()`, `rs.reset()`, `gen=gen`, zero the
per-session counters (`Played/Silence/LateDrop/StaleGen`; `RatePPM` zeroes,
`Buffered` recomputed live; `Synced` is live), `consumed=0`,
`restartHit=false`, `lastPkt=now()`, `armed=true`. Signal `wake`.

### 3.3 Push path (receiver/G goroutine)

`Push(gen, seq, pts, payload)` under the mutex:

1. `closed` → return (post-Close no-op).
2. `gen != p.gen` → `StaleGen++`, return. Covers stop/master-change races where a
   stale sender's frames are still in flight (§8.4, §8.6).
3. `!armed` → `StaleGen++`, return (no session).
4. origin unset → `jb.setOrigin(seq)`; record `originSeq=seq`, `originPTS=pts`
   (the slot-PTS basis, §3.6).
5. `!jb.insert(seq, pts, payload)` → `LateDrop++`. Else `lastPkt=now()`.
6. Signal `wake` non-blocking.

Push **never blocks and never sleeps** (§8.5).

### 3.4 Scheduler loop + resampler bookkeeping (the one goroutine)

The scheduler plays exactly one **output** frame per device write, in seq order,
sleeping until each slot's deadline. The crucial change from the pre-servo design
is that the frame written to the backend is the **resampler's** output, not the
raw jitter slot — the servo's ppm correction is what keeps long-run cadence
locked to the master clock while the backend's own buffer provides the immediate
20 ms pace.

```
for {
  mu.Lock
  if closed { mu.Unlock; return }
  if !armed || !jb.hasNext {
     mu.Unlock
     // disarmed/empty: block on wake/done with a watchdog timer (§3.7)
     select { case <-wake: ; case <-done: return ; case <-watchTimer.C: checkStarvation() }
     continue
  }
  seq      := jb.nextSeq
  pts      := slotPTS(seq)                       // originPTS + (seq-originSeq)*FrameNanos
  target   := pts + bufferNs                     // master-clock instant for the device
  local, ok := clock.MasterToLocal(target)
  if !ok {                                        // unsynced gate (§7)
     mu.Unlock; sleep ~5 ms or return on done; continue
  }
  // --- servo update (every slot) ---
  mNow, mok := clock.MasterNow()
  dDelay, dok := int64(0), false
  if delay != nil { dDelay, dok = delay.DeviceDelay() }
  if mok {
     ppm := servo.observe(consumed, mNow, dDelay, dok)
     rs.setRate(ppm)
     stats.RatePPM = ppm
  }
  s := jb.pop(seq)
  mu.Unlock

  // sleep until the coarse deadline (interruptible by done)
  if d := local - now(); d > 0 { sleep(d) or return on done }

  // --- choose the input frame, run the resampler, write ---
  var in []byte
  late := now() > local + FrameNanos
  switch {
  case s != nil && !late:  in = s.payload
  case s != nil && late:   mu:LateDrop++; in = silence   // present but too late: drop audio, keep cadence
  default:                 mu:Silence++;  in = silence    // gap (§8.5)
  }
  out := rs.process(in)               // exactly FrameBytes (§3.4 invariant below)
  if err := out.Write(out); err != nil { log once }
  else if s != nil && !late { Played++ }

  consumed += FrameSamples            // one output frame = FrameSamples/ch
  mu.Lock; jb.advance(); stats.Buffered = jb.len(); mu.Unlock
}
```

**Resampler 1-in/1-out invariant (the frame-boundary bookkeeping).** The servo
correction is tiny (|ppm| ≤ 500 ⇒ rate within [0.9995, 1.0005]), so over one
20 ms frame the cursor advances by `FrameSamples · (1/rate)` input samples, i.e.
within ±0.5 sample of 960. To keep the scheduler's "one input frame → one output
frame" contract exact while still applying the correction, the resampler keeps a
**persistent fractional cursor** measured in *input samples relative to the start
of the current input frame*:

- On `process(in)`: seed/extend per-channel history `hist` with the carryover
  samples (`p0,p1,p2` = the last three samples of the *previous* input frame),
  then the current input frame supplies the rest. Produce exactly
  `FrameSamples` output samples per channel: for output index `k`, read input
  position `cursor + k·step` (`step = 1/rate`), pick `p0..p3` straddling that
  position (negative indices reach into `hist`), apply the Catmull-Rom formula
  (§2.6).
- After emitting `FrameSamples` outputs, `cursor` has advanced by
  `FrameSamples·step`. Subtract `FrameSamples` (one frame's worth of input
  consumed nominally) so `cursor` becomes the **sub-frame remainder** carried
  into the next call: `cursor += FrameSamples*step; cursor -= FrameSamples`.
  Because `step ≈ 1`, this remainder drifts by only ≈ ppm·960/1e6 samples per
  frame (≤ 0.48 sample) and accumulates smoothly — *this* is how a fractional
  ±ppm correction is realized without ever changing the 1-in/1-out frame cadence.
- When the accumulated remainder crosses ±1 sample (after ~thousands of frames at
  the clamp), the cursor naturally wraps and one extra/fewer input sample is
  effectively consumed at the boundary; history carryover makes that wrap
  glitch-free because `p0..p3` always come from contiguous real samples
  (`hist` + current frame), never a re-zeroed edge. The correction's job is
  exactly to make that long-run drift cancel the DAC's crystal error.

This keeps the design **simple**: the jitter buffer, the scheduler deadline math,
and the backend all stay frame-quantized; only the *contents* of each output
frame are resampled, and only the cursor remainder persists between frames.

Key scheduler points (unchanged from the playout contract, now downstream of the
servo):

- **One output frame per device write, in seq order.** A missing seq is played as
  resampled silence and the loop advances — never blocks for a hole (§8.5). FEC/
  reorder recovery is upstream in G.
- **Coarse anchor via `MasterToLocal(pts+bufferNs)`** sets *when* a frame hits the
  device (mid-stream joins land at the right wall-clock instant); the **servo**
  sets the *rate* so cadence stays locked after the device buffer fills (§8.5's
  three-clock split: scheduler = clocks 1→2, servo = clock 3).
- **Unsynced gate (§7)**: `ok=false` from `MasterToLocal` ⇒ hold (~5 ms re-poll),
  no write, `Stats().Synced=false`. The servo also only updates when `MasterNow`
  is synced.
- **Late slot**: deadline already > one frame past ⇒ a *present* frame is dropped
  (`LateDrop++`, audio replaced by silence to preserve cadence), a *missing* frame
  emits silence. Either way advance.

### 3.5 Skew measurement: DelayReporter vs backpressure (D25)

The servo's accuracy depends on whether the backend reports queued audio:

- **DelayReporter present** (alsa, v1 — `snd_pcm_delay`): `consumed` counts
  samples *written*, and `DeviceDelay()` gives the still-queued ns; subtracting
  the queued samples yields exactly what the speaker has *emitted*, so the skew
  estimate is exact and absolute inter-room accuracy is tight. This is now a
  **real v1 path** taken whenever `auto` selects (or the user names) the
  runtime-loaded alsa backend (D34) — not just a reserved seam.
- **DelayReporter absent** (exec pipes, null, file — when alsa isn't the chosen
  backend): the servo uses **backpressure inference** — it equates "samples consumed" with
  "samples written", treating the pipe/device buffer as a fixed latency. This is
  correct *in the rate domain* (the long-run write rate equals the DAC consume
  rate once the pipe is full, because a full pipe back-pressures `Write`), which
  is all the servo needs to cancel drift; only the constant per-device offset is
  unknown, capping absolute accuracy at ≈ ±10–20 ms (§8.5). The 3 s averaging
  window makes the rate estimate robust to per-`Write` jitter.

Either way the servo runs **continuously** from the first synced slot — it is
drift *prevention*, not an underrun reaction (§8.5).

### 3.6 Slot PTS basis

`slotPTS(seq) = originPTS + int64(seq-originSeq)·FrameNanos`. The origin is the
first frame *of the session* (not assumed to be seq 0); a gap still has a
well-defined deadline so cadence and inter-room alignment survive losses. `seq`
is uint64 and never wraps in a v1 session.

### 3.7 Starvation watchdog → RestartFunc (§8.6)

The scheduler tracks `lastPkt` (set on every accepted Push). Whenever it blocks
(disarmed/empty/holding), it arms a timer of `cfg.Watchdog` (2 s). On expiry,
under the mutex, if `armed && now()-lastPkt >= watchdog`:

1. If **not** `restartHit`: set `restartHit=true`, log "starved, requesting
   RESTART", and (outside the mutex) call `p.restart()` once. G's subscriber
   turns this into a wire RESTART to the source ("re-prime me", §8.6); the source
   burst-replays the ring and frames resume → the next accepted Push updates
   `lastPkt`, the scheduler catches up, and `restartHit` clears on the next
   `Reset`. The sink stays armed during this grace window so resumed frames flow
   straight back into playout.
2. If `restartHit` **and still** starved after a second watchdog interval (the
   source stayed silent — master died): `disarm()` — `armed=false`, `jb.reset()`,
   `servo.reset()`, `rs.reset()`. G then unsubscribes locally and group self-heal
   (§5) takes over. We do **not** close the backend (the node stays ready for the
   next session); a subsequent `Reset(gen)` re-arms.

`restart` may be nil (tests, or a source-side loopback that re-arms via `Reset`);
then step 1 is a no-op and step 2 (disarm) still fires after 2 s. This matches
D25: "starved > 2 s → RESTART; still starved → unsubscribe, group self-heal."

### 3.8 Shutdown (`Close`)

`Close()` is idempotent (guards on `closed` under the mutex): set `closed=true`,
close `done` (unblocking the scheduler from any sleep/wait), `wg.Wait()`, then
`out.Close()`; return the backend's close error. After Close, `Push`/`Reset` are
no-ops.

### 3.9 Locking strategy

**One mutex** (`p.mu`) guards every `Playout` field except the channels and `wg`.
The scheduler releases the mutex **before** it sleeps and **before** `out.Write`
(a backend write can block on the pipe — D21: "`Backend.Write` may block";
holding the mutex there would stall `Push`/`Stats`). `consumed` is mutated only
by the scheduler goroutine (after the unlocked write) so the servo's next read
(under the lock) sees a consistent value. The jitter buffer, servo, and resampler
are plain structs touched only under `p.mu` (servo/resampler reads inside the
locked region; the resampler `process` call itself runs *unlocked* on the
scheduler goroutine, which solely owns the resampler between lock releases — no
other goroutine touches `rs`). The clock and backend own their own locking.
This honors S's convention: one mutex per stateful component, no struct shared
across two mutexes.

---

## 4. Edge cases & failure handling

- **Stale generation (§8.4, §8.6)**: `Push` with `gen != p.gen` → `StaleGen++`,
  dropped. After `stop` bumps the gen and `Reset(newGen)` arrives, in-flight old
  datagrams are discarded; new-gen frames arriving *before* `Reset` are also
  StaleGen-dropped — acceptable, the source's `leadMs` + `bufferMs` give slack.
- **Unsynced clock (§7)**: no playout before the follower has a sample.
  `MasterToLocal`/`MasterNow` return `ok=false`; scheduler holds, servo skips its
  update, `Stats().Synced=false`. Buffer accumulates to `cap`, furthest dropped.
- **Servo before warm-up**: `observe` returns 0 ppm until a baseline exists and at
  least a small fraction of the window has elapsed (`wantSamples` small ⇒ noisy
  ratio); guard with a minimum elapsed (e.g. ≥ 200 ms) before emitting non-zero
  correction. `RatePPM` reads 0 until settled (matches `SinkStats` doc: "0 until
  settled").
- **Servo clamp (§8.5)**: PI output is clamped to ±500 ppm *before* slew, then
  slewed by ≤ `SlewPPM` per update so the resampler rate never jumps audibly.
  Tested explicitly (`TestServoClampsPPM`).
- **Resampler at rate==1**: `step==1`, `cursor` stays integer-aligned, `t==0`,
  Catmull-Rom returns `p1` exactly → bit-identical passthrough (no coloration
  when no correction is needed). Tested (`TestResamplerIdentityAtUnitRate`).
- **Resampler frame boundary**: first output of each new input frame interpolates
  using carryover `hist` (previous frame's tail) for `p0..p3`, so there is no
  discontinuity or zero-edge click at the seam. Tested with a continuous ramp/sine
  across two frames (`TestResamplerContinuityAcrossFrames`).
- **Resampler over silence**: silence frames (gaps/late) feed the resampler like
  any input; interpolated silence is still silence, and the cursor/history advance
  so the seam *after* a gap is continuous. No special-casing.
- **Buffer overflow**: bounded at `cap` (256 ≈ 5.1 s). A far-behind/unsynced member
  keeps it full; `insert` evicts the furthest-future slot (`LateDrop++`). No
  unbounded growth.
- **Late frame after deadline (§8.5)**: present but past `deadline+FrameNanos` →
  `LateDrop++`, audio replaced by resampled silence, slot advances (writing it
  would desync).
- **Gap / missing frame (§8.5)**: `pop==nil` → resampled silence, `Silence++`,
  advance. Never blocks.
- **Duplicate seq (FEC double-delivery)**: `insert` overwrites idempotently; an
  already-popped seq is `< nextSeq` → dropped as late, never replayed.
- **DelayReporter absent (exec/null/file)**: servo uses backpressure inference
  (§3.5); accuracy capped at ±10–20 ms absolute, drift still cancelled. The exec
  backend deliberately omits `DeviceDelay`; alsa supplies it (§3.5).
- **Backend write error (player died)**: logged once; scheduler keeps advancing
  (cadence preserved, `Played` simply doesn't increment). Auto-respawn is out of
  scope (v1 simplicity). `consumed` still advances so the servo doesn't see a fake
  stall.
- **Watchdog RESTART then recovery (§8.6)**: first 2 s starvation fires
  `RestartFunc` once and stays armed; resumed (re-primed) frames flow straight
  back. A second 2 s of silence disarms + resets servo/resampler/jitter; G
  unsubscribes; self-heal takes over. `restartHit` clears on the next `Reset`.
- **`RestartFunc` nil**: watchdog still disarms after 2 s; the RESTART step is a
  no-op (tests, loopback master-self sink that re-arms via Reset).
- **Live settings change (D23)**: a codec/transport/bufferMs change bumps the gen;
  H calls `SetBufferMs` then `Reset(newGen)`. The new buffer lead applies from the
  next scheduled slot; servo/resampler reset so the new session starts clean.
- **Close during active playout**: `done` unblocks any sleep/wait; an in-progress
  `out.Write` completes (backend mutex); then `out.Close()`. No goroutine leak
  (`-race`).
- **Null backend pacing vs scheduler sleep**: the scheduler already sleeps to each
  slot deadline; null pacing is a secondary 20 ms cadence guard for realism. Tests
  disable null pacing and drive a fake clock + fake `now` for determinism.
- **`alsa` selected without libasound**: the `dl.Open` probe failed at `init()`,
  so "alsa" is not registered; `Open("alsa")` errors ("not registered") and K
  reports it. `auto` skips alsa and degrades to exec → null. Where libasound
  loads, `Open("alsa")` returns a working backend; only a device-open failure
  (`snd_pcm_open` error) makes an explicit `alsa` request error.

---

## 5. Test plan

`servo_test.go` (pure; no goroutines, no clock)
- **`TestServoConvergesOnFastDAC`** — drive `observe` with a synthetic DAC
  consuming at **+200 ppm** (each step: `consumed` grows by
  `FrameSamples·(1+200e-6)`, `masterNanos` by exactly `FrameNanos`); assert the
  emitted correction converges to ≈ **−200 ppm** (within tolerance) within a
  bounded number of steps, and that it then holds steady (the buffer-drift the
  correction implies cancels). Mirrors D25's required "servo converges on a fake
  DAC consuming at +200 ppm".
- `TestServoConvergesNegative` — symmetric, DAC at −150 ppm → output ≈ +150 ppm.
- **`TestServoClampsPPM`** — feed an extreme skew (e.g. 5000 ppm) → output clamps
  at +500 / −500, never exceeds; required by the prompt.
- `TestServoSlewLimited` — a step change in skew moves the output by ≤ `SlewPPM`
  per update (no instantaneous jump to the clamp).
- `TestServoUsesDeviceDelay` — with `ok=true` device delay, the emitted
  correction matches the speaker-emitted skew (delay subtracted), differing from
  the write-only inference when a constant queue is present.
- `TestServoZeroBeforeWarmup` — first few observes (elapsed < warm-up) return 0.
- `TestServoResetClearsIntegral` — after convergence, `reset()` → next output 0.

`resampler_test.go` (pure)
- **`TestResamplerIdentityAtUnitRate`** — rate ppm=0 over several frames → output
  bit-identical to input (Catmull-Rom at t=0 returns p1).
- **`TestResamplerContinuityAcrossFrames`** — feed a continuous sine split into
  consecutive frames at rate +300 ppm; assert no discontinuity at the seam (the
  difference between the last output of frame N and first of frame N+1 matches the
  local slope within tolerance — i.e. no click/zero-edge). Required by the prompt.
- `TestResamplerCursorCarryover` — after N frames at +ppm, the accumulated cursor
  remainder equals `N·FrameSamples·(1/rate−1) mod 1` (bookkeeping invariant §3.4).
- `TestResamplerOutputFrameSize` — every `process` returns exactly `FrameBytes`.
- `TestResamplerSilenceStaysSilence` — silence in → silence out; seam after a gap
  is continuous.
- `TestResamplerStereoIndependence` — L and R channels interpolated independently
  (distinct L/R ramps stay distinct, not cross-contaminated).

`jitter_test.go`
- `TestJitterInsertPopInOrder`, `TestJitterReorderRecovers`,
  `TestJitterPopMissingReturnsNil`, `TestJitterLateInsertRejected`,
  `TestJitterFullDropsFurthest`, `TestJitterDuplicateOverwrites`,
  `TestJitterResetClearsOrigin` — as in the playout data-structure contract.

`registry_test.go`
- `TestOpenNull` — `Open("null")` → null backend, name "null".
- `TestOpenFile` — `Open("file:"+tmp)` → file backend, file created; name "file".
- `TestOpenAutoDegradesToNull` — PATH emptied, `Open("auto")` → null, no error.
- `TestOpenExecExplicitNoToolDegrades` — `Open("exec")` no tool → null + WARN.
- `TestOpenUnknownErrors` — `Open("bogus")` → error.
- `TestOpenAlsaUnloadableErrors` — when libasound is not loadable (the dl probe
  failed, "alsa" unregistered): `Open("alsa")` → error (not registered), and
  `Open("auto")` skips alsa, degrading to exec → null without error.
- `TestBackendNamesIncludesAlsaWhenLoaded` — `t.Skip` unless libasound is
  loadable; then `BackendNames()` contains "alsa". The base set is always
  {exec, file, null}, sorted.
- `TestHasPlayback` — boolean without spawning (smoke; value depends on PATH).

`backend_test.go`
- `TestNullBackendCounts`, `TestNullBackendRejectsWrongSize`,
  `TestNullBackendPaceOff`, `TestFileBackendAppends`, `TestFileBackendBadPath`,
  `TestExecBackendSkippedIfNoTool` (t.Skip unless a tool on PATH; else spawn,
  write a few frames, Close cleanly).
- `TestAlsaBackendSkippedIfNoLib` — `t.Skip` unless `dl.Open` of `libasound.so.2`
  succeeds; else open "default", write a few 960-frame periods, assert
  `DeviceDelay()` returns `ok=true`, Close cleanly. CI without libasound skips it;
  the dl-probe-failure path (registry omits "alsa", auto degrades to exec) is
  covered by `TestOpenAlsaUnloadableErrors`.

`sink_test.go` (fake `contracts.Clock` + injected `now`; null backend, pace off)
- `TestPlayoutBasicInOrder` — Reset, push 10 in-order frames → 10 written in
  order; Played==10, Silence==0.
- `TestPlayoutInsertsSilenceForGap` — push 0,1,3,4 (miss 2) → 5 frames out, gap
  is silence; Silence==1, Played==4.
- `TestPlayoutReorderWithinBuffer` — push 0,2,1,3 with clock slack → all 4 in
  order, Silence==0.
- `TestPlayoutStaleGenDropped` — Reset(gen=2), Push(gen=1,…) → StaleGen++, not
  played.
- `TestPlayoutLateFrameDropped` — clock advanced so a present frame's deadline is
  past → LateDrop++, that slot silent, loop advances.
- `TestPlayoutUnsyncedHolds` — clock ok=false → no writes, Synced==false; flip to
  synced → frames flush, servo begins updating.
- **`TestPlayoutServoDrivesRate`** — fake **DAC consuming at +200 ppm** behind the
  backend (a fake `contracts.Backend`+`DelayReporter` whose `DeviceDelay`/consume
  model runs +200 ppm); over a few seconds of simulated frames assert
  `Stats().RatePPM` converges to ≈ −200 ppm and the jitter buffer depth stays
  bounded (drift cancelled). Ties the servo into the live scheduler.
- **`TestPlayoutRatePPMClamps`** — extreme fake skew → `Stats().RatePPM` ∈
  [−500,500]. Required by the prompt.
- `TestPlayoutBufferStat` — `Stats().Buffered` tracks `jb.len()` as frames flow.
- `TestPlayoutBufferMsLead` — frame pts P with bufferMs=150 → device write
  scheduled at `MasterToLocal(P+150ms)` (assert via fake clock translation).
- `TestPlayoutResetZeroesCounters` — accumulate stats, Reset → per-session
  counters + RatePPM zero, gen updated, origin re-armed on next Push, servo/
  resampler reset.
- **`TestPlayoutWatchdogFiresRestart`** — Reset, push one frame, advance `now`
  past 2 s with no pushes → injected `RestartFunc` called exactly once; sink stays
  armed. Then resume pushes → playout continues.
- **`TestPlayoutWatchdogDisarmsAfterRestart`** — as above but stay silent a
  second 2 s → sink disarms (no further writes), servo/resampler/jitter reset;
  next Reset re-arms.
- `TestPlayoutWatchdogNilRestart` — `Restart=nil` → no panic; disarms after 2 s.
- `TestPlayoutCloseNoLeak` — `-race`: New, Reset, Push, Close → scheduler exits,
  backend closed, second Close no-op.
- `TestPlayoutPushAfterCloseNoop` — Push/Reset after Close do nothing, no panic.
- `TestPlayoutStatsConcurrent` — `-race`: Push from one goroutine, Stats from
  another while the scheduler runs.

All tests use the null/file backends (or a fake `contracts.Backend`/
`DelayReporter`), a fake `contracts.Clock`, an injected `now`, and pace disabled
— no audio hardware, no multicast, no root, no sleeps.

---

## 6. Notes on contract fit

- `contracts.Sink` (Push/Reset/Stats/Close) maps 1:1 to `Playout`. `Push` is
  fire-and-forget (no return), per the confirmed-as-designed rule.
- `contracts.SinkStats` now carries `RatePPM` (the servo's current correction,
  0 until settled) and `Buffered` (jitter depth) in addition to
  Played/Silence/LateDrop/StaleGen/Synced — all produced exactly by the scheduler
  + servo (D25, §9.1 / D19 envelope).
- `contracts.Backend` (Write/Close) is implemented by `alsa`/`exec`/`null`/`file`,
  the alsa one runtime-loaded via `internal/dl` (D32/D34, no build tag); the
  registry (D27) keeps backend selection in one place with `ENSEMBLE_OUTPUT`
  (D2/D3). `capabilities.backends` = `BackendNames()` (includes "alsa" only when
  libasound loaded), `capabilities.playback` = `HasPlayback()` (assembled by K, D3).
- `contracts.DelayReporter` is **consumed** here (type-asserted once at `New`) and
  **implemented** by the alsa backend (v1) via `snd_pcm_delay`; exec/null/file do
  not implement it. The servo's `DelayReporter`-vs-backpressure split is §3.5.
- `contracts.Clock.MasterToLocal` is the coarse scheduling primitive;
  `MasterNow` feeds the servo's master-elapsed term and the live `Synced` flag
  (cached from the latest `ok`, refreshed each tick and on `Stats()` via a
  `MasterNow()` probe).
- The source-side `DefaultLeadMs` (§8.2) is H's concern; E consumes only
  `bufferMs`. The two add at the device (frame leaves master at `pts−leadMs`,
  lands at device at `pts+bufferMs`); E never references leadMs.
- The watchdog's `RestartFunc` is the *only* outward seam to G (§8.6); there is no
  push-model endpoint management here, no `SetEndpoints`/`Resolver`/master-dials
  anything — subscribers (G) own the wire RESTART; E just signals "I'm starved."
```
