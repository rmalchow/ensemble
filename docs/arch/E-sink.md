# E — sink & playout

Source of truth: [docs/README.md](../README.md) (§8.5 sink/playout, §8.6 stop/end,
§8.1 canonical format, §7 clock). Contracts: [S-skeleton.md](S-skeleton.md)
(`internal/contracts`: `Backend`, `Sink`, `SinkStats`, `Clock`; `internal/stream`:
`FrameBytes`, `FrameNanos`).

This piece owns `internal/sink/*` only. It implements the **output backends**
(exec-pipe auto-pick, null, file) and the **playout pipeline** (jitter buffer +
scheduler) that consumes wire-decoded frames on every group member and writes
canonical PCM to a backend at the right local instant.

Design stance: **one scheduler goroutine, one mutex, no abstraction beyond the two
contract interfaces already fixed in S** (`Backend` has two impls in v1 → stays an
interface; everything else is concrete). The receiver (G) and group engine (H) call
`Sink.Push`/`Reset`; nothing in this piece dials sockets or knows about transports.

---

## 1. Package / file layout

All files in `internal/sink/`. One package `sink`.

```
sink.go            Playout: implements contracts.Sink. Constructor New(cfg), the
                   public Push/Reset/Stats/Close. Holds the jitter buffer, stats,
                   generation, and owns the scheduler goroutine. The only stateful
                   type; one mutex.
jitter.go          jitterBuffer: fixed-capacity map[uint64]*slot keyed by Seq,
                   insert (drop-if-full / drop-if-too-old), pop-by-seq, drain,
                   reset. Pure data structure, no locking (caller holds the mutex).
backend.go         Backend factory: PickBackend(cfg) chooses exec/null/file from
                   env+capability. Shared frame-size validation helper.
backend_exec.go    execBackend: pipes raw s16le into pw-play/pw-cat/aplay/paplay.
                   Auto-detect order, stdin pipe, process lifecycle, stderr drain.
backend_null.go    nullBackend: timed discard pacing one frame per 20 ms wall time,
                   counts frames written. Used in tests + playback-less nodes.
backend_file.go    fileBackend: append raw PCM to a debug file (no pacing). Debug
                   only; selected by ENSEMBLE_OUTPUT=file:/path.
config.go          Config struct, env parsing (ENSEMBLE_OUTPUT), defaults wiring
                   from contracts.Default* and stream consts.
sink_test.go       Playout scheduler tests (fake clock, null backend).
jitter_test.go     jitterBuffer unit tests (no goroutines).
backend_test.go    null/file backend tests; exec backend skipped if no tool on PATH.
```

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

// Config configures one Playout instance for one session-capable member.
// Constructed once per node; BufferMs/Gen are refreshed per session via Reset.
type Config struct {
	Backend  contracts.Backend // output device (PickBackend or a test fake)
	Clock    contracts.Clock   // master-time translation (F); never nil
	BufferMs int               // playout lead: audio for pts hits device at pts+BufferMs (§8.5)
	Log      *slog.Logger      // component logger; defaulted if nil

	// Tunables with sane defaults (overridable in tests). Zero => default.
	Capacity     int           // jitter-buffer slot cap (default 256 frames ≈ 5.1 s)
	Watchdog     time.Duration // starvation timeout (default 2 s, §8.6)
	now          func() int64  // local monotonic ns; default monotoNow (tests inject)
}

// BackendKind is the selected output backend (for logging / status).
type BackendKind string

const (
	KindExec BackendKind = "exec"
	KindNull BackendKind = "null"
	KindFile BackendKind = "file"
)

// BackendConfig is parsed from ENSEMBLE_OUTPUT and capability.
type BackendConfig struct {
	Kind    BackendKind
	Tool    string // resolved exec tool path (KindExec) or "" 
	Path    string // file target (KindFile)
}
```

`BufferMs` default comes from `contracts.DefaultBufferMs` (150); the group engine
passes the per-group setting. `now` returns nanoseconds from a monotonic source
(`time.Now()` carries a monotonic reading; we read it as ns via a package helper
so the fake clock in tests can supply a deterministic counter).

### 2.2 `backend.go` / backends

```go
// PickBackend resolves the output backend per ENSEMBLE_OUTPUT and host capability.
//
//	ENSEMBLE_OUTPUT unset / "auto" / "exec" -> first of pw-play, pw-cat -p, aplay,
//	                                           paplay on $PATH; falls back to null
//	                                           if none found.
//	"null"                                  -> nullBackend (timed discard)
//	"file:/abs/path"                        -> fileBackend appending raw PCM
//
// Returns the chosen Backend, a BackendConfig describing it (for /api/status and
// the capabilities.playback flag), and an error only for an explicit-but-broken
// request (e.g. file path unopenable). "auto" never errors: it degrades to null.
func PickBackend(log *slog.Logger) (contracts.Backend, BackendConfig, error)

// HasPlaybackTool reports whether any exec tool is on $PATH, for the node-record
// capabilities.playback flag (§1). Pure lookup, no process spawn.
func HasPlaybackTool() bool

// execTools is the auto-pick order (§8.5).
var execTools = []struct{ name string; args []string }{
	{"pw-play", []string{"--rate", "48000", "--channels", "2", "--format", "s16", "-"}},
	{"pw-cat",  []string{"-p", "--rate", "48000", "--channels", "2", "--format", "s16", "-"}},
	{"aplay",   []string{"-q", "-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw", "-"}},
	{"paplay",  []string{"--raw", "--rate=48000", "--channels=2", "--format=s16le"}},
}
```

```go
// execBackend pipes canonical PCM into a player subprocess via stdin.
type execBackend struct {
	cmd  *exec.Cmd
	in   io.WriteCloser // stdin pipe
	log  *slog.Logger
	once sync.Once      // Close idempotency
}

func newExecBackend(tool string, args []string, log *slog.Logger) (*execBackend, error)
func (b *execBackend) Write(frame []byte) error // validates len, writes all bytes
func (b *execBackend) Close() error             // close stdin, Wait with timeout, kill on hang
```

```go
// nullBackend discards frames but paces at real time so playout timing is
// exercised exactly as with a real device (one frame per FrameDuration).
type nullBackend struct {
	mu      sync.Mutex
	written uint64
	last    time.Time
	pace    bool          // true: sleep to maintain 20 ms cadence (default)
	sleep   func(time.Duration) // injectable for tests (default time.Sleep)
}

func newNullBackend() *nullBackend
func (b *nullBackend) Write(frame []byte) error // validate len, pace, written++
func (b *nullBackend) Close() error
func (b *nullBackend) Written() uint64
```

```go
// fileBackend appends raw PCM to a debug file. No pacing (caller/scheduler paces).
type fileBackend struct {
	f   *os.File
	mu  sync.Mutex
}

func newFileBackend(path string) (*fileBackend, error)
func (b *fileBackend) Write(frame []byte) error
func (b *fileBackend) Close() error
```

All `Write` implementations reject `len(frame) != stream.FrameBytes` with an error
(defensive; the scheduler always passes exactly `FrameBytes`, including the
pre-allocated silence frame).

### 2.3 `jitter.go`

```go
// slot holds one buffered frame's payload + its pts. payload is owned (copied on
// Push) so the receiver may reuse its read buffer.
type slot struct {
	pts     int64
	payload []byte // exactly stream.FrameBytes
}

// jitterBuffer is a bounded seq-keyed reorder buffer. NOT goroutine-safe; the
// Playout mutex guards every call. nextSeq is the seq the scheduler will play next.
type jitterBuffer struct {
	slots    map[uint64]*slot
	cap      int
	nextSeq  uint64 // first seq of the current session (set on reset/first frame)
	hasNext  bool   // false until the first frame establishes the seq origin
}

func newJitterBuffer(capacity int) *jitterBuffer

// insert stores frame at seq. Returns false (dropped) if: seq < nextSeq (already
// passed → late), or the buffer is full and seq is not closer-than-current. A
// duplicate seq overwrites (idempotent). Copies payload into the slot.
func (j *jitterBuffer) insert(seq uint64, pts int64, payload []byte) (stored bool)

// pop removes and returns the slot for seq, or nil if absent (gap).
func (j *jitterBuffer) pop(seq uint64) *slot

// setOrigin fixes nextSeq to seq on the first frame of a session.
func (j *jitterBuffer) setOrigin(seq uint64)

// advance moves nextSeq forward by one (after playing or silence).
func (j *jitterBuffer) advance()

// reset empties the buffer and clears the origin (new generation).
func (j *jitterBuffer) reset()

// len reports buffered slot count (for tests / pressure logging).
func (j *jitterBuffer) len() int
```

### 2.4 `sink.go` — `Playout` (implements `contracts.Sink`)

```go
package sink

// Playout is the per-node sink: jitter buffer + scheduler + backend.
// Implements contracts.Sink. One scheduler goroutine, one mutex.
type Playout struct {
	mu      sync.Mutex
	jb      *jitterBuffer
	gen     uint32        // current accepted generation; Push drops others
	armed   bool          // a session is active (Reset called, scheduler running step loop)
	stats   contracts.SinkStats
	lastPkt int64         // local-ns of the most recent accepted Push (watchdog)

	backend  contracts.Clock // (see note) -- actually:
	clock    contracts.Clock
	out      contracts.Backend
	bufferNs int64         // BufferMs in ns
	cap      int
	watchdog time.Duration
	now      func() int64
	log      *slog.Logger

	silence []byte        // pre-allocated zeroed FrameBytes frame
	wake    chan struct{} // scheduler wakeup on Push/Reset
	done    chan struct{} // closed by Close to stop scheduler
	wg      sync.WaitGroup
}

// New builds a Playout and starts its scheduler goroutine (which idles until the
// first Reset arms a session). cfg.Backend and cfg.Clock must be non-nil.
func New(cfg Config) *Playout

// Push enqueues a frame for playout (contracts.Sink). Non-blocking. Drops+counts
// stale-gen and obviously-late frames; copies payload; signals the scheduler.
func (p *Playout) Push(gen uint32, seq uint64, pts int64, payload []byte)

// Reset arms the sink for a new generation: discards queued frames, sets gen,
// clears per-session counters, re-establishes the seq origin on the next Push.
func (p *Playout) Reset(gen uint32)

// Stats snapshots playout counters (contracts.Sink). Synced is read live from the
// clock.
func (p *Playout) Stats() contracts.SinkStats

// Close stops the scheduler and closes the backend (contracts.Sink). Idempotent.
func (p *Playout) Close() error
```

(The struct sketch above lists `backend` twice by accident in narration; the real
fields are `clock contracts.Clock`, `out contracts.Backend` — there is no field
named `backend`.)

---

## 3. Control flow

### Startup
`New(cfg)` allocates the jitter buffer (`cfg.Capacity` or 256), the pre-zeroed
`silence` frame (`make([]byte, stream.FrameBytes)`), the `wake`/`done` channels,
and starts **one** scheduler goroutine. Until the first `Reset` the scheduler is
*disarmed*: it blocks on `wake`/`done` and does nothing. `armed=false`.

### Arming a session (`Reset(gen)`)
Group/receiver calls `Reset(gen)` at the start of each play session (new
generation, §8.4). Under the mutex: `jb.reset()`, `gen=gen`, zero the per-session
counters (`Played/Silence/LateDrop/StaleGen`; `Synced` is computed live), set
`armed=true`, `lastPkt = now()`. Signal `wake`.

### Steady state — Push path (receiver/G goroutine)
`Push(gen, seq, pts, payload)` under the mutex:
1. If `gen != p.gen` → `stats.StaleGen++`, return (drop). Covers stop/master-change
   races where a stale sender's frames are still in flight (§8.4, §8.6).
2. If `!armed` → drop (no session). (Counts as StaleGen.)
3. If the jitter origin is unset → `jb.setOrigin(seq)` (first frame of session
   defines `nextSeq`). The scheduler's first deadline is derived from this frame's
   `pts`.
4. `if !jb.insert(seq, pts, payload)` → `stats.LateDrop++` (seq already passed or
   buffer full). Else `lastPkt = now()`.
5. Signal `wake` non-blocking (buffered cap-1 channel; a pending wake is enough).

Push **never blocks and never sleeps** (§8.5 "never block").

### Steady state — scheduler loop (the one goroutine)
While `armed`, the scheduler plays exactly one frame slot per iteration, advancing
`nextSeq` monotonically and sleeping until each slot's deadline:

```
for {
  mu.Lock
  if !armed { mu.Unlock; wait on wake/done; continue }
  if !jb.hasNext { mu.Unlock; wait on wake/done with watchdog timer; continue }
  seq   := jb.nextSeq
  // Determine this slot's target pts. We track sessionPTS = originPTS +
  // (seq-originSeq)*FrameNanos so a gap (missing slot) still has a deadline.
  pts   := p.slotPTS(seq)
  local, ok := clock.MasterToLocal(pts + bufferNs)   // when this frame must hit device
  if !ok { mu.Unlock; wait briefly for sync; continue }   // unsynced: hold (§7)
  deadline := local
  s := jb.pop(seq)
  mu.Unlock

  // sleep until deadline (interruptible by done)
  d := deadline - now()
  if d > 0 { sleep(d) or return on done }

  if now() > deadline + FrameNanos {        // we are hopelessly late on this slot
     // frame is late even though present → drop+count, do NOT write
     if s != nil { lateDrop++ }
     else        { /* missing & late: still emit silence to keep cadence */ }
  }
  if s != nil { out.Write(s.payload); played++ }
  else        { out.Write(silence);  silence++ }   // gap → silence (§8.5)

  mu.Lock; jb.advance(); mu.Unlock
}
```

Key points:
- **One frame per device write, in seq order.** A missing seq is played as a
  pre-zeroed silence frame and the loop advances — never blocks waiting for a hole
  to fill (§8.5). FEC/reorder recovery happens upstream in G; by the time the
  scheduler reaches a slot, whatever arrived is in the buffer.
- **PTS → local** via `clock.MasterToLocal(pts + bufferNs)`. The backend's own
  pacing (null sleeps 20 ms; exec/PipeWire buffers) provides fine cadence; the
  scheduler provides the coarse anchor so a member that joins mid-stream lands at
  the right wall-clock instant.
- **Unsynced gate (§7)**: if `MasterToLocal` returns `ok=false` the member has no
  clock offset yet and must not start playout. The scheduler holds (short re-poll,
  ~5 ms) instead of writing. `Stats().Synced` reflects this.
- **Late slot**: if the computed deadline is already more than one frame in the
  past, a *present* frame is dropped as `LateDrop` (it would play out of time);
  a *missing* frame still emits silence so the device cadence and downstream seq
  alignment stay intact. Either way the loop advances.

### Watchdog (§8.6)
The scheduler tracks `lastPkt` (updated on every accepted Push). On each disarmed
or starved wait it arms a timer of `cfg.Watchdog` (2 s). If `now()-lastPkt >=
watchdog` while armed, the scheduler logs "starvation" and calls an internal
`disarm()`: `armed=false`, `jb.reset()`. This is the follower-side stop when
frames cease (master died / network gone) without an explicit `stop`. A subsequent
`Reset(gen)` re-arms. We do **not** close the backend on starvation (the node stays
ready for the next session); we just stop emitting.

### Shutdown (`Close`)
`Close()` (idempotent via `sync.Once` semantics on `done`): close `done` to unblock
the scheduler from any sleep/wait, `wg.Wait()`, then `out.Close()`. Returns the
backend's close error. After Close, Push/Reset are no-ops (guarded by a `closed`
flag checked under the mutex).

### Locking strategy
**One mutex** (`p.mu`) guards every Playout field except the channels (`wake`,
`done`) and `wg`. The scheduler releases the mutex before it sleeps and before it
calls `out.Write` (a backend write can block on the pipe; holding the mutex there
would stall `Push`/`Stats`). The jitter buffer is plain (no internal lock); it is
only ever touched under `p.mu`. The clock is read through its own interface (F owns
its locking). Backends own their own internal mutex for `Write`/`Close`
concurrency (`Close` may race the scheduler's last `Write`).

---

## 4. Edge cases & failure handling

- **Stale generation (§8.4, §8.6)**: `Push` with `gen != p.gen` is dropped and
  counted in `StaleGen`. After `stop` bumps the generation and `Reset` is called
  with the new gen, in-flight datagrams from the old session are discarded. A
  *late* `Reset` (frames of the new gen arrive before Reset) → those are also
  StaleGen-dropped until Reset arms the new gen; acceptable, the source's `leadMs`
  (§8.2) plus bufferMs gives slack.
- **Unsynced clock (§7)**: playout must not start before the follower has a
  sample. `MasterToLocal` returns `ok=false`; the scheduler holds and `Stats().
  Synced=false`. No frames written, no silence emitted (we are not yet in the
  session timeline) — buffer accumulates up to `cap`, oldest dropped past cap.
- **Buffer overflow**: bounded at `cap` (256 ≈ 5.1 s). A member that is unsynced or
  far behind keeps the buffer full; `jb.insert` drops the *furthest-future* frame
  to keep room for nearer ones (or simply rejects when full and seq isn't nearer),
  counted as `LateDrop`. No unbounded memory growth.
- **Late frame after deadline (§8.5)**: present but past `deadline+FrameNanos` →
  dropped, `LateDrop++`, never written (would desync). The slot still advances.
- **Gap / missing frame (§8.5)**: `jb.pop(seq)==nil` → write the pre-zeroed silence
  frame, `Silence++`, advance. Never blocks waiting for the hole.
- **Seq wrap / origin**: `nextSeq` is established from the first frame's seq of the
  session (sessions start at seq 0 per wire.go, but we don't assume 0 — we anchor
  to whatever arrives first and derive each slot's pts from `originPTS +
  (seq-originSeq)*FrameNanos`). uint64 seq never wraps in a v1 session.
- **Duplicate seq (FEC double-delivery)**: `jb.insert` overwrites the slot
  idempotently; if the slot was already popped (already played), the seq is
  `< nextSeq` → dropped as late, not re-played.
- **Backend write error (exec pipe broken — player died)**: logged once; the
  scheduler keeps advancing (silence-equivalent: cadence preserved) and increments
  a `LateDrop`-adjacent counter? No — to keep SinkStats minimal we log and treat
  it as a dropped slot (no counter inflation beyond Played not incrementing). On
  repeated errors the exec backend's own `Write` returns error each time; the node
  stays "alive" but silent until restart. (Auto-respawn of the player is out of
  scope for v1 simplicity.)
- **Starvation watchdog fires mid-session (§8.6)**: disarm + reset; `Stats()` keeps
  cumulative counters until the next `Reset` zeroes per-session ones. The node is
  ready for a fresh `Reset`.
- **Close during active playout**: `done` unblocks the scheduler from its sleep;
  any in-progress `out.Write` completes (backend mutex); then `out.Close()`. No
  goroutine leak (verified by `-race` test).
- **Null backend pacing vs scheduler sleep**: the scheduler already sleeps to the
  per-slot deadline; the null backend's 20 ms pace is a secondary cadence guard so
  null-backend tests see realistic timing without a clock. In tests we disable null
  pacing (`pace=false`) and drive a fake clock + fake `now` for determinism.
- **File backend on bad path**: `PickBackend` returns the open error for an
  explicit `file:` request (fatal at wiring time, K decides); `auto` never selects
  file.
- **No exec tool + ENSEMBLE_OUTPUT=exec explicitly**: degrade to null with a
  warning (don't fail the node); `auto` already degrades silently. The node's
  `capabilities.playback` is set from `HasPlaybackTool()`, so a tool-less node
  honestly reports no playback even though it still runs a null sink (so it
  participates in clock/stream/stats for the e2e test, §K).

---

## 5. Test plan

`jitter_test.go`
- `TestJitterInsertPopInOrder` — insert 0..4, pop in order returns each payload.
- `TestJitterReorderRecovers` — insert 2,0,1,3 out of order; pop 0..3 in order.
- `TestJitterPopMissingReturnsNil` — pop a never-inserted seq → nil (gap).
- `TestJitterLateInsertRejected` — after advance past seq 5, insert seq 3 → false.
- `TestJitterFullDropsFurthest` — fill cap, insert nearer seq evicts furthest.
- `TestJitterDuplicateOverwrites` — insert same seq twice → second payload wins.
- `TestJitterResetClearsOrigin` — reset empties buffer and unsets hasNext.

`backend_test.go`
- `TestNullBackendCounts` — Write N frames → Written()==N; Close idempotent.
- `TestNullBackendRejectsWrongSize` — Write(<FrameBytes) → error.
- `TestNullBackendPaceOff` — pace=false writes return immediately (no sleep).
- `TestFileBackendAppends` — Write two frames → file is 2*FrameBytes, bytes match.
- `TestFileBackendBadPath` — newFileBackend("/no/such/dir/x") → error.
- `TestHasPlaybackTool` — returns a bool without spawning (smoke; no assertion on
  value since CI may lack tools).
- `TestPickBackendNull` — ENSEMBLE_OUTPUT=null → KindNull, non-nil backend.
- `TestPickBackendFile` — ENSEMBLE_OUTPUT=file:tmp → KindFile, file created.
- `TestPickBackendAutoDegrades` — with PATH emptied, auto → KindNull (no error).
- `TestExecBackendSkippedIfNoTool` — t.Skip unless a tool is on PATH; else spawn,
  write a few frames, Close cleanly.

`sink_test.go` (fake clock implementing contracts.Clock + injected `now`)
- `TestPlayoutBasicInOrder` — Reset, push 10 in-order frames; null backend (pace
  off) receives 10 in order; Played==10, Silence==0.
- `TestPlayoutInsertsSilenceForGap` — push seq 0,1,3,4 (miss 2); backend gets 5
  frames, frame at gap is zeroed; Silence==1, Played==4.
- `TestPlayoutReorderWithinBuffer` — push 0,2,1,3 with clock slack; all 4 played in
  order, Silence==0.
- `TestPlayoutStaleGenDropped` — Reset(gen=2); Push(gen=1,...) → StaleGen++, not
  played.
- `TestPlayoutLateFrameDropped` — fake clock advanced so a present frame's deadline
  is past → LateDrop++, that slot silent, loop advances.
- `TestPlayoutUnsyncedHolds` — clock.ok=false → no writes, Stats().Synced==false;
  flip to synced → frames flush.
- `TestPlayoutResetZeroesCounters` — accumulate stats, Reset → per-session counters
  zero, gen updated, origin re-armed on next Push.
- `TestPlayoutWatchdogDisarms` — Reset, push one frame, advance fake now past 2 s
  with no more pushes → scheduler disarms (no further writes); next Reset re-arms.
- `TestPlayoutBufferMsLead` — frame pts P with bufferMs=150 → device write scheduled
  at MasterToLocal(P+150ms); assert deadline via fake clock translation.
- `TestPlayoutCloseNoLeak` — `-race`; New, Reset, Push, Close → scheduler exits,
  backend closed, second Close is a no-op.
- `TestPlayoutPushAfterCloseNoop` — Push/Reset after Close do nothing, no panic.
- `TestPlayoutStatsConcurrent` — `-race`; Push from one goroutine, Stats from
  another while the scheduler runs.

All tests use the null/file backends, a fake `contracts.Clock`, an injected `now`
func, and pace disabled — no audio hardware, no sleeps, no sockets.

---

## 6. Notes on contract fit

- `contracts.Sink` (Push/Reset/Stats/Close) maps 1:1 to `Playout`. `SinkStats`
  fields (Played/Silence/LateDrop/StaleGen/Synced) are exactly what the scheduler
  tracks; no extra fields needed for §9.1.
- `contracts.Backend` (Write/Close) is implemented by exec/null/file. The "second
  implementation exists in v1" rule justifies keeping it an interface (exec+null
  both ship); file is a third, debug-only.
- `contracts.Clock.MasterToLocal` is the single scheduling primitive used; `Synced`
  in stats comes from the `ok` return (we cache the last `ok` seen, refreshed on
  every scheduler tick and on `Stats()` via a `MasterNow()` probe).
- The source-side `DefaultLeadMs` (§8.2) is H's concern, not E's. E only consumes
  `bufferMs`. The two add up at the device (frame leaves master at `pts−leadMs`,
  lands at device at `pts+bufferMs`), but E never references leadMs.
