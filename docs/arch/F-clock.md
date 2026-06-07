# F — clock sync

Source of truth: [docs/README.md](../README.md) §7 (clock sync), §8.4 (wire /
generation), and the shared contracts in [S-skeleton.md](S-skeleton.md). This
piece is master-anchored, NTP-style clock sync over the **STREAM_PORT UDP**
socket, multiplexed by packet type (`0x10` request, `0x11` reply).

It owns only `internal/clock/*`. It depends *only* on:

- `stream.Mux` — `Register`, `WriteTo`, `LocalAddr` (S contract).
- `stream.Header`, `stream.Magic`, `stream.TypeClockReq`, `stream.TypeClockRsp`,
  `stream.HeaderSize`, `stream.Encode/Decode` (S contract).
- `stream` clock-payload layout pinned in S §2.2: `payload = t1(8) | t2(8) |
  t3(8)`, big-endian int64, t2/t3 zero on a request.

It implements `contracts.Clock` (`MasterNow`, `MasterToLocal`).

**Design stance:** smallest thing that satisfies §7. No abstraction over time
beyond an injectable `now func() int64` (monotonic ns) for tests. No interface
for the server. One follower struct, one server struct, one mutex each.

---

## 1. Package / file layout

```
internal/clock/clock.go        Server + Follower types, constructors, exported API
internal/clock/sample.go       sample ring + 5-best-of-30 median-offset estimator (pure, no I/O)
internal/clock/payload.go      clock payload encode/decode (t1|t2|t3), thin over stream.Header
internal/clock/clock_test.go   server reply correctness; follower offset over loopback mux; resync
internal/clock/sample_test.go  estimator math: median, best-RTT selection, ring eviction, sync gate
internal/clock/payload_test.go request/reply byte round-trip, short-buffer rejection
```

Six files. `sample.go` is the only non-trivial logic and is pure (table-test
friendly). `clock.go` is the I/O wiring. `payload.go` is ~40 lines.

---

## 2. Concrete Go API

### 2.1 `payload.go` — clock packet (de)serialization

A clock packet is a `stream.Header` (Magic + Type set, Gen carried for resync,
Seq/PTS/PayloadLen used as below) followed by a 24-byte payload `t1|t2|t3`.

We reuse the header's `Gen` field for the generation gate (§8.4) and stash the
follower's probe sequence in `Header.Seq` so a reply can be matched to its
request without follower-side bookkeeping. `PTS` is unused (0). `PayloadLen` is
always `clockPayloadSize` (24).

```go
package clock

import (
	"encoding/binary"

	"ensemble/internal/stream"
)

// clockPayloadSize is the fixed clock payload: t1|t2|t3, three big-endian int64.
const clockPayloadSize = 24

// packetSize is header + payload for a clock datagram.
const packetSize = stream.HeaderSize + clockPayloadSize // 24 + 24 = 48

// encodeClock writes a clock datagram into dst[:packetSize] and returns the
// number of bytes written (packetSize). typ is TypeClockReq or TypeClockRsp.
// gen is the session generation; seq is the probe sequence (echoed in replies).
// On a request, t2 and t3 are 0.
func encodeClock(dst []byte, typ byte, gen uint32, seq uint64, t1, t2, t3 int64) int

// decodeClock parses a clock datagram. It returns the header plus t1,t2,t3.
// It validates Magic, that PayloadLen >= clockPayloadSize, and that the buffer
// holds the full payload; otherwise returns an error (stream.ErrShort /
// stream.ErrBadMagic).
func decodeClock(pkt []byte) (h stream.Header, t1, t2, t3 int64, err error)
```

Implementation: `encodeClock` calls `stream.Header{Magic, Type:typ, Gen:gen,
Seq:seq, PayloadLen:clockPayloadSize}.Encode(dst)` then three
`binary.BigEndian.PutUint64` at offsets 24/32/40. `decodeClock` calls
`stream.Decode`, checks magic + length, then reads the three int64.

### 2.2 `sample.go` — offset estimator

Pure value logic, no clock, no I/O. Holds the last 30 samples in a ring; the
estimate is the **median offset of the 5 samples with smallest RTT** (§7).

```go
package clock

import "sort"

// sample is one completed NTP-style exchange.
//   offset = ((t2 - t1) + (t3 - t4)) / 2   (master_ns - local_ns)
//   rtt    = (t4 - t1) - (t3 - t2)          (>= 0, smaller is better)
type sample struct {
	offset int64
	rtt    int64
}

// newSample computes offset and rtt from the four NTP timestamps (all ns).
func newSample(t1, t2, t3, t4 int64) sample

// estimator keeps the last windowSize samples and reports the median offset of
// the bestN with smallest RTT. Not safe for concurrent use; the Follower's
// mutex guards it.
type estimator struct {
	ring  []sample // up to windowSize, oldest-first (append + drop front)
	count uint64   // total samples ever added (for stats / debugging)
}

const (
	windowSize = 30 // "last 30" (§7)
	bestN      = 5  // "5 best-RTT samples" (§7)
)

// add inserts a sample, evicting the oldest when the window is full.
func (e *estimator) add(s sample)

// offset returns the current estimate and whether it is usable.
// ok is false until at least one sample exists (the *unsynced* gate, §7).
// With 1..bestN samples it medians whatever it has; with more, the bestN by RTT.
func (e *estimator) offset() (offsetNanos int64, ok bool)

// reset discards all samples (resync on generation / master change, §7/§8.4).
func (e *estimator) reset()

// len reports how many samples are currently held (tests / stats).
func (e *estimator) len() int
```

Median rule: copy the ring, `sort.Slice` by `rtt` ascending, take the first
`min(bestN, len)` offsets, sort *those* offsets ascending, return the middle
element (for an even count, the lower-middle — deterministic, no averaging of
int64 to avoid overflow surprises; spec says "median", lower-middle is a valid
median for our purposes and keeps it integer).

### 2.3 `clock.go` — Server

The master answers `0x10` with `0x11`, echoing t1, stamping t2 (receive) and t3
(send). Registered on the mux; replies are written back via `Mux.WriteTo` to the
datagram's `from` address. Stateless except the mux handle and `now`.

```go
package clock

import (
	"log/slog"
	"net/netip"

	"ensemble/internal/stream"
)

// nowFunc returns monotonic-derived nanoseconds. Production uses monoNow
// (below); tests inject a fake. It must be monotonic and is the SAME clock the
// Follower uses for t4 when following localhost (so master-vs-self offset ~ 0).
type nowFunc func() int64

// Server answers clock requests (type 0x10) with replies (type 0x11) on the
// shared UDP mux (§7). One per node; runs entirely on the mux read goroutine
// (the handler is cheap and non-blocking, honoring the Mux contract).
type Server struct {
	mux *stream.Mux
	now nowFunc
	log *slog.Logger
}

// NewServer creates the server bound to mux. It does NOT register yet.
func NewServer(mux *stream.Mux, log *slog.Logger) *Server

// Start registers the 0x10 handler on the mux. Idempotent; safe before or
// after mux.Run (S allows Register any time).
func (s *Server) Start()

// handle is the registered Handler. It decodes the request, stamps t2 on entry
// and t3 just before send, and writes a 0x11 reply to `from`. Malformed packets
// are dropped (counter via log at debug). Echoes Gen and Seq unchanged.
```

The server does **not** filter by generation: it answers every well-formed
request regardless of `Gen` (the master is the time source; gen is the
*follower's* concern for discarding stale samples). It echoes the request's Gen
and Seq verbatim so the follower can gate replies itself.

### 2.4 `clock.go` — Follower

Every member, **including the master against localhost** (§7), runs a follower:
one probe per second, computes offset, keeps 5-best-of-30, exposes `MasterNow`.

```go
// Follower probes a master's clock once per second and maintains the offset
// estimate (§7). Implements contracts.Clock.
type Follower struct {
	mux *stream.Mux
	now nowFunc
	log *slog.Logger

	mu   sync.Mutex     // the one mutex: guards everything below
	est  estimator      // offset estimator (last 30, best 5)
	gen  uint32         // current session generation; replies with other gen ignored
	seq  uint64         // next probe sequence
	dst  netip.AddrPort // master clock endpoint (mux UDP addr)
	have bool           // dst has been set at least once

	pending map[uint64]int64 // probe seq -> t1 (local send time); pruned on reply/timeout

	done    chan struct{}
	wg      sync.WaitGroup
	started bool
}

// NewFollower creates a follower bound to mux. now MUST be the same monotonic
// clock the local Server uses, so a master following itself sees ~0 offset.
func NewFollower(mux *stream.Mux, log *slog.Logger) *Follower

// Start registers the 0x11 reply handler and launches the 1 Hz probe loop.
// Call SetMaster (and ideally Retarget) before relying on MasterNow.
func (f *Follower) Start()

// SetMaster points the follower at a master clock endpoint and resyncs:
// it sets dst, bumps to the given generation, resets the estimator and pending
// map. Used on every mastership change (§7) and at startup. Calling it with the
// same (dst, gen) is a no-op (no spurious resync). The master follows ITSELF by
// passing mux.LocalAddr() here.
func (f *Follower) SetMaster(dst netip.AddrPort, gen uint32)

// MasterNow returns master-clock ns and whether synced (contracts.Clock).
func (f *Follower) MasterNow() (masterNanos int64, ok bool)

// MasterToLocal converts a master-clock instant to local ns (contracts.Clock).
func (f *Follower) MasterToLocal(masterNanos int64) (localNanos int64, ok bool)

// LocalToMaster converts a local instant to master-clock ns. (Concrete method;
// not on contracts.Clock — see contract concerns. Source/playout-side helper.)
func (f *Follower) LocalToMaster(localNanos int64) (masterNanos int64, ok bool)

// Stats reports follower state for /api/status.
func (f *Follower) Stats() FollowerStats

// Close stops the probe loop. Idempotent.
func (f *Follower) Close() error

// FollowerStats is a snapshot for diagnostics / /api/status.
type FollowerStats struct {
	Synced    bool   // MasterNow ok
	OffsetNs  int64  // current estimate (0 if unsynced)
	Samples   int    // samples currently in window
	Gen       uint32 // current generation
	Master    string // dst.String()
	Probes    uint64 // probes sent
	Replies   uint64 // replies accepted
}
```

`MasterNow` = `now() + offset` when synced. `MasterToLocal(m)` = `m - offset`;
`LocalToMaster(l)` = `l + offset`. All take `mu`, read `est.offset()`, and
return its `ok`.

---

## 3. Control flow

### Startup (wired by K / group H)

1. `NewServer(mux, log).Start()` — registers `0x10`. The master *is* whichever
   node has `following == ""`; but the server is harmless to run on every node
   (it only answers requests; non-masters simply receive none). **We run the
   server unconditionally on every node** — simplest, no role-gating, matches
   "one code path". A follower only ever sends probes to its group's master, so
   a non-master's server is dormant.
2. `NewFollower(mux, log).Start()` — registers `0x11`, launches the probe loop.
3. The **group engine (H)** calls `Follower.SetMaster(endpoint, gen)` whenever:
   - the node first joins/derives its group (initial target = master's stream
     UDP endpoint, resolved per §3.1 by H/cluster; the master passes
     `mux.LocalAddr()` for itself),
   - mastership changes (§5.2) — new master endpoint, **and** H bumps the
     generation,
   - the session generation bumps on a new play/stop (§8.4),
   - the resolved master endpoint changes even with the same master id (address
     re-resolution per §3.1; see commit `4d384ba` rationale).

   F does not know about cluster derivation; it just obeys `SetMaster`.

### Steady state — probe loop (one goroutine)

```
ticker := 1 Hz
for {
  select {
  case <-done: return
  case <-ticker.C:
    lock:
      if !have { unlock; continue }        // no master yet
      seq := f.seq; f.seq++
      gen := f.gen; dst := f.dst
      t1 := now()
      pending[seq] = t1
      prunePending(now())                  // drop probes older than 5s (lost replies)
    unlock
    encodeClock(buf, TypeClockReq, gen, seq, t1, 0, 0)
    mux.WriteTo(buf, dst)                   // outside the lock
    Probes++
  }
}
```

### Steady state — reply handler (runs on mux read goroutine)

```
handleReply(pkt, from):
  t4 := now()                              // stamp arrival ASAP
  h, t1w, t2, t3, err := decodeClock(pkt)
  if err || h.Type != TypeClockRsp: return
  lock:
    if h.Gen != f.gen: unlock; return      // stale generation, drop (resync gate §8.4)
    t1, ok := pending[h.Seq]
    if !ok: unlock; return                 // unknown/duplicate/late, drop
    delete(pending, h.Seq)
    est.add(newSample(t1, t2, t3, t4))      // t1 is OUR recorded send time, not echoed t1w
    Replies++
  unlock
```

We trust our **locally recorded** `t1` (from `pending[seq]`) rather than the
echoed `t1w`; the echo is belt-and-suspenders / debugging only. This makes the
math immune to a misbehaving echo and means the request payload's t1 is
advisory. (We still send t1 in the request so a future stateless follower could
work, and so packet captures are readable.)

The reply handler must not block (mux contract): it only takes `mu` briefly,
does O(1) map ops + an O(1) estimator append. The estimator's median is computed
lazily in `offset()` (called by `MasterNow`), so the hot path stays cheap.

### Shutdown

`Close()` closes `done`, `wg.Wait()`s the probe loop. The reply handler stays
registered on the mux but the mux itself is closed by K; a late reply after
`Close` finds `done` closed is harmless (handler just updates state nobody
reads). We do **not** unregister from the mux (S has no Unregister; not needed).

### Locking

**One mutex** (`f.mu`) per Follower guards `est`, `gen`, `seq`, `dst`, `have`,
`pending`. The Server has **no mutex** (stateless; `mux` and `now` are
read-only after construction). `mux.WriteTo` is called outside `f.mu` (it's
concurrency-safe per S, and we must not hold our lock across a syscall). The two
goroutines touching `f.mu` are the probe ticker and the mux read goroutine
(reply handler); `MasterNow`/`MasterToLocal`/`Stats` are called from playout (E)
and API (I) goroutines — all serialize on `f.mu`. No struct is shared across two
mutexes (S convention).

---

## 4. Edge cases & failure handling

- **Unsynced gate (§7).** `est.offset()` returns `ok=false` with zero samples,
  so `MasterNow`/`MasterToLocal`/`LocalToMaster` return `ok=false`. Playout (E)
  must not start until `ok` (it gates on `SinkStats.Synced`, fed from here).
- **Master following itself (§7).** The master calls `SetMaster(mux.LocalAddr(),
  gen)`. Probes loop back through the same UDP socket to its own server; t1..t4
  share one monotonic clock so offset ≈ 0 (only the tiny localhost RTT). This is
  the same code path as a real follower — no special case. `LocalAddr` must be a
  dialable unicast addr; if the mux bound `0.0.0.0:P`, the follower rewrites a
  wildcard/unspecified host to loopback (`127.0.0.1` / `::1`) before dialing —
  handled in `SetMaster`: if `dst.Addr().IsUnspecified()`, substitute loopback.
- **Lost request or reply (UDP).** No reply ⇒ the `pending[seq]` entry lingers;
  `prunePending` drops entries older than 5 s each tick so the map can't grow
  unbounded under sustained loss. A missing sample just means the window fills
  slower; once ≥1 sample exists we stay synced using the last good estimate.
- **Reply for an old generation (§8.4).** Dropped in the handler (`h.Gen !=
  f.gen`). After `SetMaster` bumps gen + resets, in-flight replies from the old
  gen are ignored and `pending` was cleared, so their seqs are unknown too —
  double-guarded.
- **Duplicate / reordered replies.** First reply consumes `pending[seq]`;
  duplicates find no entry and are dropped. Reordering is fine — samples are
  unordered in the estimator (selected by RTT, not arrival).
- **Resync discards samples (§7).** `SetMaster` with a new (dst,gen) calls
  `est.reset()` and clears `pending`, so the node goes *unsynced* until the next
  reply — correct: a new master means old offsets are meaningless.
- **No-op SetMaster.** Same (dst,gen) ⇒ return early, do **not** reset. Prevents
  a steady stream of redundant SetMaster calls (e.g. H re-resolving to the same
  endpoint each tick) from perpetually wiping the window and keeping the node
  unsynced.
- **Endpoint change, same master id (§3.1, commit 4d384ba).** H passes a new
  `dst` with the *same or bumped* gen; since `dst` differs it's not a no-op, so
  we resync to the new address. We resync on ANY dst change even if gen is
  unchanged.
- **Clock packet from a stranger / wrong type on the reply handler.** The mux
  only routes `0x11` to the follower and `0x10` to the server, but we still
  re-check `h.Type` after decode and drop mismatches (defensive against a
  mis-registered handler in tests).
- **Negative RTT.** Clock granularity or a reply that "arrives before it was
  sent" in pathological fakes can yield rtt < 0. We keep the sample (offset is
  still meaningful) but `sort` by raw rtt; a negative rtt just sorts as "best".
  In practice monotonic `now()` prevents this for real traffic.
- **Pending map under churn.** Bounded by `prunePending` (5 s window at 1 Hz ⇒
  ≤ ~5 entries) plus deletion on every accepted reply.
- **Server stamping order.** t2 is read at handler entry (closest to receive),
  t3 immediately before `WriteTo` (closest to send), to minimize the master's
  processing time leaking into the follower's RTT/offset.
- **PTS field unused.** Clock packets set `PTS=0`; we never read it. (S reserves
  PTS for audio frames.)
- **int64 ns, no float.** Offset/RTT/median are all int64; `MasterToLocal`
  subtracts. We never convert ns to float (§ S edge cases; avoids 2^53 loss).

---

## 5. Test plan

`internal/clock/payload_test.go`
- `TestEncodeDecodeRequestRoundTrip` — request (t2=t3=0) round-trips; header
  Type=0x10, Gen/Seq preserved, payload t1 read back.
- `TestEncodeDecodeReplyRoundTrip` — reply with all three timestamps round-trips.
- `TestDecodeClockShortBuffer` — buffer < packetSize → ErrShort.
- `TestDecodeClockBadMagic` — corrupted magic byte → ErrBadMagic.
- `TestDecodeClockBigEndianOffsets` — hand-checked t1/t2/t3 bytes at 24/32/40.

`internal/clock/sample_test.go`
- `TestNewSampleMath` — known t1..t4 give expected offset & rtt.
- `TestEstimatorUnsyncedUntilFirstSample` — `offset()` ok=false at 0 samples,
  true at 1.
- `TestEstimatorMedianOfBestFive` — 30 samples with crafted rtt/offset: result
  is the median offset of the 5 smallest-rtt samples, ignoring the rest.
- `TestEstimatorFewerThanFiveSamples` — with 1..4 samples medians what it has.
- `TestEstimatorRingEviction` — >30 samples: only the last 30 are considered;
  oldest dropped.
- `TestEstimatorResetClears` — `reset()` → ok=false, len 0.
- `TestEstimatorIgnoresHighRTTOutlier` — a huge-rtt sample with a wild offset is
  excluded from the best-5, doesn't move the estimate.

`internal/clock/clock_test.go` (loopback mux, fake monotonic clock)
- `TestServerReplyEchoesAndStamps` — feed a 0x10 packet through a real mux;
  assert the 0x11 reply echoes Gen/Seq/t1, has t2≤t3 from the fake clock, sent
  to `from`.
- `TestServerDropsMalformed` — short / bad-magic / wrong-type request → no reply.
- `TestFollowerSyncsOverLoopback` — Server+Follower share one mux; advance the
  fake clock with a fixed master↔local skew; after probes, `MasterNow` ok and
  offset within tolerance of the injected skew.
- `TestMasterFollowsSelfZeroOffset` — follower targets `mux.LocalAddr()`, single
  monotonic clock ⇒ |offset| ≈ localhost RTT (≈0), Synced true.
- `TestFollowerUnspecifiedAddrRewritten` — `SetMaster` with `0.0.0.0:P` dials
  loopback and still syncs.
- `TestResyncOnGenerationChange` — sync, then `SetMaster(dst, gen+1)`:
  immediately unsynced (samples cleared); a reply with the OLD gen is ignored;
  new-gen replies re-sync.
- `TestResyncOnEndpointChange` — `SetMaster(dst2, sameGen)` with a different addr
  resets samples and re-targets dst2.
- `TestSetMasterNoOpSameTarget` — `SetMaster(sameDst, sameGen)` does not reset a
  synced estimator (stays synced, sample count unchanged).
- `TestStaleGenReplyDropped` — inject a 0x11 with `Gen != f.gen` → not added.
- `TestUnknownSeqReplyDropped` — 0x11 with a seq never sent → ignored, no panic.
- `TestPendingPrunedOnLoss` — drop all replies (server off); pending map stays
  bounded (≤ small N) after many ticks.
- `TestMasterToLocalRoundTrip` — synced: `MasterToLocal(LocalToMaster(x)) == x`.
- `TestCloseStopsProbeLoop` — `Close` returns, probe goroutine exits (`-race`,
  no leak); second `Close` is safe.
- `TestUnsyncedBeforeFirstReply` — fresh follower: `MasterNow` ok=false until the
  first reply lands.

All tests run over `127.0.0.1` loopback via a real `stream.Mux`, with an
injectable `now func() int64` fake clock — no root, no hardware, no real time
dependence (the probe ticker is driven by a test-controllable interval, see
below).

### Test seam for the ticker

To keep `TestFollowerSyncsOverLoopback` fast and deterministic, `Start` uses an
internal `probeInterval` field (default `time.Second`) that tests set to a few
ms via an unexported `newFollowerInterval(mux, log, d)` constructor (or a
`WithInterval` option). The 1 Hz figure is the production default per §7; the
seam is test-only and not part of the exported API.

---

## 6. Contract summary

| Depends on (S) | Use |
|---|---|
| `stream.Mux.Register` | server registers 0x10, follower registers 0x11 |
| `stream.Mux.WriteTo` | send request (follower) / reply (server) |
| `stream.Mux.LocalAddr` | master-follows-self target |
| `stream.Header` + `Encode/Decode` | clock framing (Magic/Type/Gen/Seq) |
| `stream.TypeClockReq/Rsp`, `Magic`, `HeaderSize` | packet typing |
| clock payload layout (S §2.2) | `t1|t2|t3` big-endian int64 |

| Consumed by | Via |
|---|---|
| sink/playout (E) | `contracts.Clock` (MasterNow/MasterToLocal) → Synced gate |
| group (H) | `SetMaster(dst, gen)` on derivation / takeover / play / endpoint change |
| api (I) / K | `Stats()` for `/api/status` |
