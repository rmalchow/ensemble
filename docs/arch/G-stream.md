# G — stream transport

Source of truth: [docs/README.md](../README.md) §8.4. Contracts I code against:
[S-skeleton.md](S-skeleton.md) — `internal/stream/wire.go` (Header, Encode,
AppendFrame, Decode, DecodeFrame, XORInto, type/PCM consts) and
`internal/stream/mux.go` (Mux: Register, WriteTo, LocalAddr). Both are
**read-only** to me; I add new files in the same package `stream`.

This piece is the wire transport between the group master and its members: a
**sender** (master side, fan-out to N endpoints) and a **receiver** (member
side, one per node). Two transports, selected by the group setting
`transport: udp | tcp` (§8.4): UDP datagrams + XOR FEC, or one persistent
length-prefixed TCP connection per member. Both deliver `(Header, payload)` to
a caller-supplied callback. Pure transport: no decode, no clock, no jitter
buffer (that is E/H). Frames in, frames out, plus loss/recovery counters.

---

## 1. Package / file layout

All in package `stream`, alongside the read-only `wire.go` / `mux.go`.

```
internal/stream/sender.go        Sender: transport-agnostic fan-out API; owns Endpoint set, gen, seq, FEC block accumulator; routes to udp/tcp impl
internal/stream/sender_udp.go    udpTransport: per-datagram WriteTo via Mux + parity datagram every 4 audio frames
internal/stream/sender_tcp.go    tcpTransport: one tcpConn per endpoint, length-prefixed writes, dial + backoff reconnect goroutine
internal/stream/receiver.go      Receiver: registers a Deliver callback; owns UDP path (mux 0x01/0x02 → reorder/FEC window) and TCP listener path; Counters()
internal/stream/fec.go           fecBlock encode helper (sender) + recoveryWindow (receiver): per-block buffering, single-loss XOR recovery
internal/stream/recvwindow.go    reorderBuffer: small ordered window, in-order/at-most-once delivery, gap accounting, flush on gen change
internal/stream/sender_test.go   sender fan-out, FEC parity emission cadence, gen/seq monotonicity, tcp reconnect, null-endpoint
internal/stream/receiver_test.go udp loopback deliver, FEC single-loss recovery, reorder, stale-gen drop, tcp listener deliver, counters
internal/stream/fec_test.go      block parity build, recover missing payload from parity+3, double-loss unrecoverable
internal/stream/recvwindow_test.go in-order, out-of-order reorder, duplicate drop, window overflow eviction, gen reset
```

No new exported types beyond `Sender`, `Receiver`, their constructors, the
small `Endpoint`/`Counters`/options structs, and the `DeliverFunc` callback
type. Everything else is unexported.

---

## 2. Concrete Go API

### 2.1 Common: endpoints, callback, transport selector

```go
package stream

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// Transport selects the wire transport for a session (group setting §8.4).
// Mirrors contracts.GroupSettings.Transport ("udp" | "tcp").
type Transport int

const (
	TransportUDP Transport = iota // 0: datagrams + XOR FEC (default)
	TransportTCP                  // 1: persistent length-prefixed conn
)

// ParseTransport maps the group-setting string to a Transport. Unknown ->
// TransportUDP (the spec default §8.4), so a malformed record never wedges play.
func ParseTransport(s string) Transport

// Endpoint is one stream destination: a group member's STREAM_PORT, already
// resolved to an address by the group/cluster piece (H/C, §3.1 candidate
// selection happens upstream — G dials exactly what it's handed). The master's
// own sink is just another Endpoint pointing at localhost:STREAM_PORT (§8.2,
// "including itself … one code path").
type Endpoint struct {
	Node id.ID          // member node id (for logs / per-endpoint state keys)
	Addr netip.AddrPort // member STREAM_PORT (same number for UDP and TCP, §2)
}
```

### 2.2 Sender (master side)

One `Sender` per active play session. The group piece (H) creates it on `Play`,
calls `SetEndpoints` once (and again if membership changes mid-session),
`SendFrame` per 20 ms frame from the source ticker, and `Stop`/`Close` at
end-of-file or stop (§8.6).

```go
// Sender fans audio frames out to a set of Endpoints over UDP+FEC or TCP.
// Transport-agnostic: it owns the header bookkeeping (gen, seq, FEC blocking)
// and delegates the actual write to a transport impl. One Sender per session.
//
// Concurrency: SendFrame is called from a single goroutine (the source
// ticker). SetEndpoints / Stop / Close may be called from another (the API/
// group goroutine). All exported methods are safe under one mutex.
type Sender struct {
	mu        sync.Mutex
	gen       uint32
	seq       uint64
	endpoints []Endpoint
	tr        senderTransport // udpTransport or tcpTransport
	fec       fecBlock        // UDP only; zero-value/unused for TCP
	closed    bool
	log       *slog.Logger
}

// SenderConfig wires a Sender to its transport. mux is required for
// TransportUDP (it is the WriteTo side of the STREAM_PORT UDP socket, owned by
// S); ignored for TransportTCP. gen is the session generation (§8.4); the first
// SendFrame uses Seq 0.
type SenderConfig struct {
	Transport Transport
	Gen       uint32
	Mux       *Mux         // UDP write side (nil ok for TCP)
	Log       *slog.Logger // nil -> slog.Default with comp=stream/sender
}

// NewSender builds a Sender. For TransportTCP it starts no goroutines until
// SetEndpoints adds endpoints (each endpoint gets its own dial/reconnect
// goroutine). For TransportUDP it is purely synchronous (writes via Mux).
func NewSender(cfg SenderConfig) *Sender

// SetEndpoints replaces the destination set. For TCP, new endpoints get a
// dial+backoff goroutine; dropped endpoints have their conn closed. Idempotent
// for unchanged endpoints (keyed by Node id). Safe mid-session on membership
// change (§5).
func (s *Sender) SetEndpoints(eps []Endpoint)

// SendFrame encodes one audio frame (TypeAudio) with the next Seq and the given
// PTS (master-clock ns, §8.2) and the session Gen, then transmits it to every
// endpoint. For UDP it also folds the payload into the current FEC block and,
// after every 4th audio frame, emits the parity datagram (TypeFEC) to every
// endpoint (§8.4). payload must be canonical PCM (FrameBytes) for pcm, or an
// Opus packet for opus — G is codec-agnostic, it just carries bytes. Returns
// the Seq used. No error: per-endpoint write failures are counted+logged, never
// propagated (a dead UDP path or a reconnecting TCP path must not stall the
// source ticker, §8.2/§8.6).
func (s *Sender) SendFrame(pts int64, payload []byte) uint64

// Stop sends a control "stop" by bumping the generation contract is NOT here —
// generation bumping and the stop-control record live in H (§8.6). G's Stop
// just flushes any partial FEC block (emits an early parity if 1..3 frames are
// pending) and stops; the receiver's watchdog (E) handles cessation. Kept for
// symmetry; callers normally just Close.
func (s *Sender) Stop()

// Close stops all transport goroutines (TCP dialers/conns) and releases
// resources. The UDP socket itself is owned by the Mux (S), not closed here.
// Idempotent.
func (s *Sender) Close() error

// senderTransport is the private seam between Sender and its two impls.
type senderTransport interface {
	send(pkt []byte, eps []Endpoint)            // unicast pkt to every endpoint
	setEndpoints(eps []Endpoint)                // (re)dial for TCP; no-op for UDP
	close() error
}
```

#### UDP transport (`sender_udp.go`)

```go
// udpTransport writes each pre-encoded packet to every endpoint via the shared
// Mux (the STREAM_PORT UDP write side, S). Stateless beyond the Mux handle:
// FEC blocking lives in Sender.fec, parity packets arrive here as ordinary
// send() calls. Per-endpoint write errors are counted, not returned.
type udpTransport struct {
	mux     *Mux
	log     *slog.Logger
	writeErr atomic.Uint64
}

func (u *udpTransport) send(pkt []byte, eps []Endpoint) // mux.WriteTo per ep
func (u *udpTransport) setEndpoints([]Endpoint)         // no-op
func (u *udpTransport) close() error                    // no-op (mux owned by S)
```

#### TCP transport (`sender_tcp.go`)

```go
// tcpTransport keeps one persistent outbound connection per endpoint and writes
// length-prefixed frames (uint32 big-endian length + header+payload bytes,
// §8.4). A per-endpoint dialer goroutine reconnects with capped exponential
// backoff (250ms → 4s) whenever the conn is absent or errors. send() is
// best-effort: if an endpoint has no live conn, the packet is dropped for it
// (counted); TCP retransmit handles loss for connected endpoints, so there is
// NO FEC on this path (§8.4).
type tcpTransport struct {
	mu    sync.Mutex
	conns map[id.ID]*tcpConn // keyed by Endpoint.Node
	log   *slog.Logger
}

// tcpConn is the per-endpoint connection state + dialer loop.
type tcpConn struct {
	node    id.ID
	addr    netip.AddrPort
	mu      sync.Mutex     // guards c
	c       net.Conn       // nil while (re)connecting
	done    chan struct{}  // closed on removal -> dialer exits
	drops   atomic.Uint64  // frames dropped while disconnected
	log     *slog.Logger
}

func newTCPTransport(log *slog.Logger) *tcpTransport
func (t *tcpTransport) send(pkt []byte, eps []Endpoint)
func (t *tcpTransport) setEndpoints(eps []Endpoint) // diff: dial new, close gone
func (t *tcpTransport) close() error
```

### 2.3 Receiver (member side)

One `Receiver` per node, long-lived (created at startup, not per session). It
handles **both** transports concurrently: it registers UDP handlers on the Mux
for `TypeAudio`/`TypeFEC`, and owns the STREAM_PORT **TCP listener** for the TCP
path. Frames from either path go through the same reorder/dedup logic and out
the same `DeliverFunc`. The receiver does not know the active group transport —
it accepts whatever arrives (a member that switched udp↔tcp mid-cluster just
sees frames on the new path).

```go
// DeliverFunc receives one decoded frame: the parsed Header and its payload.
// payload aliases the receiver's buffer and is ONLY valid for the duration of
// the call — the Sink (E) copies on Push. Called from the receiver's own
// goroutine(s), serialized per source path; the callback must not block long.
type DeliverFunc func(h Header, payload []byte)

// Receiver terminates both stream transports on a member and delivers ordered,
// de-duplicated, FEC-recovered frames to a callback (§8.4/§8.5). One per node.
//
// Concurrency: the UDP path runs on the Mux read goroutine (S) calling onUDP;
// the TCP path runs one goroutine per accepted connection. Both funnel into the
// reorder window under a single mutex. Counters are atomic.
type Receiver struct {
	mu       sync.Mutex
	window   reorderBuffer  // ordered delivery + gap accounting
	fecwin   recoveryWindow // pending FEC blocks awaiting a single missing frame
	deliver  DeliverFunc
	listener *net.TCPListener
	ctr      receiverCounters
	done     chan struct{}
	wg       sync.WaitGroup
	log      *slog.Logger
}

// ReceiverConfig wires a Receiver. Mux + Deliver are required. TCPListener is
// the STREAM_PORT TCP listener from netx.BindTCPUDP (S/K); nil disables the TCP
// path (UDP-only nodes / tests). The receiver does NOT register UDP handlers or
// start accepting until Run.
type ReceiverConfig struct {
	Mux         *Mux
	TCPListener *net.TCPListener
	Deliver     DeliverFunc
	Log         *slog.Logger
}

// NewReceiver builds a Receiver; no goroutines, no Mux registration yet.
func NewReceiver(cfg ReceiverConfig) *Receiver

// Run registers the UDP handlers (TypeAudio, TypeFEC) on the Mux and starts the
// TCP accept loop (if a listener was given). Non-blocking. Call once.
func (r *Receiver) Run()

// Counters returns a snapshot of loss/recovery/drop counters for /api/status
// (§9.1) and the e2e smoke test (K).
func (r *Receiver) Counters() Counters

// Close stops the TCP accept loop and open conns and unregisters nothing (the
// Mux is owned by S; handlers simply stop being fed after Close drains). Safe
// once.
func (r *Receiver) Close() error

// Counters are monotonic per receiver (NOT per session — the sink resets its
// own per-session stats; these are lifetime transport health).
type Counters struct {
	Delivered  uint64 // frames handed to DeliverFunc
	Recovered  uint64 // frames reconstructed by FEC
	Lost       uint64 // gaps the window gave up on (delivered as nothing; E plays silence)
	Duplicate  uint64 // frames dropped as already-delivered (reorder dup / FEC+real)
	StaleGen   uint64 // frames dropped: gen older than the highest seen
	Malformed  uint64 // datagrams that failed Decode/DecodeFrame (UDP garbage)
	FECParity  uint64 // parity datagrams received (type 0x02)
}

type receiverCounters struct { // atomic mirror of Counters
	delivered, recovered, lost, duplicate, staleGen, malformed, fecParity atomic.Uint64
}
```

### 2.4 FEC (`fec.go`)

XOR FEC over blocks of 4 audio frames (§8.4): one parity datagram per block,
parity = XOR of the 4 payloads (zero-padded to the longest). Any **single** loss
in the 5-packet block (4 data + 1 parity) is recoverable.

```go
// --- sender side ---

// fecBlock accumulates the payloads of up to 4 audio frames and the header of
// the FIRST frame of the block (parity reuses that gen and the block's base
// seq), then produces a parity packet. UDP only.
type fecBlock struct {
	count    int               // 0..4 frames folded so far
	baseSeq  uint64            // Seq of the first frame in the block
	gen      uint32
	parity   [FrameBytes]byte  // running XOR (payloads are <= FrameBytes)
	maxLen   int               // longest payload folded (parity PayloadLen)
}

// fold XORs one frame's payload into the running parity and bumps count.
func (b *fecBlock) fold(gen uint32, seq uint64, payload []byte)

// ready reports count == 4 (full block).
func (b *fecBlock) ready() bool

// parityPacket encodes the parity datagram (TypeFEC). Its Header carries:
// Gen=block gen, Seq=baseSeq (identifies the block as [baseSeq, baseSeq+3]),
// PTS=0 (unused for parity), PayloadLen=maxLen. Returns nil if count==0.
// Resets the block for the next 4 frames.
func (b *fecBlock) parityPacket(buf []byte) []byte

// flushPartial emits parity for a 1..3-frame tail at Stop (so a final short
// block is still protected); returns nil if count==0. Also resets.
func (b *fecBlock) flushPartial(buf []byte) []byte

// --- receiver side ---

// recoveryWindow tracks, per FEC block (keyed by baseSeq within the current
// gen), which of the 4 data frames have been seen and the parity. When exactly
// one data frame is missing AND parity is present, it reconstructs the missing
// payload by XORing the parity with the 3 present payloads.
type recoveryWindow struct {
	gen    uint32
	blocks map[uint64]*fecState // keyed by baseSeq; small, evicted by seq age
}

type fecState struct {
	baseSeq uint64
	have    [4]bool
	payload [4][]byte // copies of present data payloads (nil if missing)
	pts     [4]int64  // PTS of present frames (to reconstruct a recovered frame's header)
	parity  []byte    // copy of parity payload, nil until seen
	parLen  int
}

// observeData records a data frame in its block. Returns (recoveredSeq,
// recoveredPTS, recoveredPayload, true) if THIS arrival completed a block that
// was missing exactly one OTHER frame — i.e. a recovery is now possible for a
// frame still missing. (In practice recovery fires when parity OR the 3rd-of-3
// data arrives; see control flow §3.)
func (w *recoveryWindow) observeData(gen uint32, seq uint64, pts int64, payload []byte) (rseq uint64, rpts int64, rpay []byte, ok bool)

// observeParity records the parity for a block; returns a recovered frame if the
// block now has parity + exactly 3 data (one missing). Same return shape.
func (w *recoveryWindow) observeParity(gen uint32, baseSeq uint64, parity []byte) (rseq uint64, rpts int64, rpay []byte, ok bool)

// reset clears all blocks for a new generation.
func (w *recoveryWindow) reset(gen uint32)
```

### 2.5 Reorder window (`recvwindow.go`)

```go
// reorderBuffer delivers frames in Seq order, at most once, tolerating small
// out-of-order arrival and bounded gaps. It does NOT buffer for the jitter
// deadline — that is the Sink's job (E). It only fixes ordering/dedup over the
// short FEC/network reorder horizon, then hands frames downstream as soon as
// they are in order or the window slides past a gap (gap => Lost counter; E
// plays silence).
type reorderBuffer struct {
	gen      uint32
	next     uint64            // next Seq to deliver (in order)
	started  bool              // have we anchored 'next' to the first frame?
	pend     map[uint64]frameRec // seq -> buffered frame ahead of 'next'
	maxAhead uint64            // window depth (frames); slide+gap if exceeded
}

type frameRec struct {
	pts     int64
	payload []byte // owned copy
}

// admit takes a frame (real or FEC-recovered). It returns the ordered run of
// frames now deliverable (could be 0..many as a gap fills), plus how many seqs
// were skipped as lost (gaps the window slid past). Duplicates (seq < next, or
// already pending) return ok=false via the dup count.
func (b *reorderBuffer) admit(gen uint32, seq uint64, pts int64, payload []byte) (deliver []frameRec, lost int, dup bool, stale bool)

// reset re-anchors for a new generation, dropping buffered frames.
func (b *reorderBuffer) reset(gen uint32)
```

The window is intentionally tiny: `maxAhead = 32` frames (~640 ms) — comfortably
larger than any FEC block (4) plus realistic LAN reorder, far smaller than the
150 ms+ jitter buffer in E so we never double-buffer.

---

## 3. Control flow, goroutines, locking

### Sender — startup / steady / shutdown

- **Startup** (`NewSender`): pure construction. UDP: store `mux`, zero `fec`.
  TCP: build `tcpTransport` with empty conn map.
- **SetEndpoints**: under `Sender.mu`, replace the slice; call
  `tr.setEndpoints(eps)`. TCP diff: for each new `Node` id start a `tcpConn`
  dialer goroutine; for each removed one, close its `done` channel (dialer exits,
  conn closed). UDP: no-op.
- **Steady (SendFrame)**, called every 20 ms from the source ticker (H):
  1. Lock `Sender.mu`. If `closed`, unlock and return current seq.
  2. `seq := s.seq; s.seq++`. Build `Header{Magic, TypeAudio, gen, seq, pts,
     len(payload)}`; `pkt := h.AppendFrame(buf[:0], payload)` (reused scratch
     buffer on the Sender, no per-frame alloc).
  3. `tr.send(pkt, endpoints)`.
  4. UDP only: `fec.fold(gen, seq, payload)`; if `fec.ready()`,
     `par := fec.parityPacket(parityBuf[:0])`; `tr.send(par, endpoints)`.
  5. Unlock; return seq.
  The lock is held across the writes. UDP `mux.WriteTo` is a non-blocking
  sendto; TCP `send` does a non-blocking-ish `conn.Write` under each conn's own
  mutex and drops if no conn — neither blocks on the network meaningfully, so
  holding `Sender.mu` for the fan-out is fine and keeps seq/gen/FEC consistent.
- **TCP dialer goroutine** (one per endpoint): loop until `done`: if `c == nil`,
  `net.DialTimeout("tcp", addr, 2s)`; on success store `c`, reset backoff; on
  failure sleep `backoff` (250ms, ×2 up to 4s) and retry. Writes happen in
  `send` on the caller's goroutine, guarded by `tcpConn.mu`; a write error nils
  `c` and signals the dialer (the dialer also wakes on a `reconnect` channel /
  simply polls c==nil each loop with a short select on `done`+timer). Keep it
  simple: dialer loops on a timer, checks `c==nil`, redials.
- **Shutdown (Close)**: set `closed`; `tr.close()` (TCP: close all `done`,
  Wait dialers, close conns). UDP socket untouched (S owns it).

Locking: **one** `Sender.mu` for gen/seq/endpoints/fec/closed. The TCP transport
has its own `tcpTransport.mu` (conn map) and per-`tcpConn.mu` (the `net.Conn`) —
these are leaf locks never held while `Sender.mu` is held in a way that nests the
other direction (Sender calls into transport, never transport back into Sender),
so no lock-order cycle.

### Receiver — startup / steady / shutdown

- **Startup** (`NewReceiver` then `Run`): `Run` registers two Mux handlers:
  `mux.Register(TypeAudio, r.onUDP)` and `mux.Register(TypeFEC, r.onUDP)` (same
  handler, branches on `pkt[1]`). If `TCPListener != nil`, start the accept
  goroutine.
- **UDP steady (`onUDP`, runs on the Mux read goroutine, must not block — S
  contract)**:
  1. `DecodeFrame(pkt)` → `(h, payload, err)`. On err: `malformed++`, return.
  2. Branch on `h.Type`:
     - `TypeAudio`: `ingest(h, payload, true)`.
     - `TypeFEC`: `fecParity++`; under `mu`, `fecwin.observeParity(h.Gen,
       h.Seq, payload)`; if it yields a recovered frame, `recovered++` and feed
       it into `ingest(recoveredHeader, recoveredPayload, false)`.
  Because `onUDP` runs on the single Mux read goroutine and the TCP path runs on
  its own goroutine(s), both take `Receiver.mu` around the window/fec mutation;
  the callback `deliver` is invoked **while holding `mu`** (serialized, simplest)
  — acceptable because the Sink's `Push` is contractually non-blocking (S). If
  profiling ever shows contention we'd collect `deliver` frames into a local
  slice and call after unlock; v1 keeps it under the lock for simplicity.
- **`ingest(h, payload, real)`** (caller holds nothing; takes `mu`):
  1. Gen gate: if `h.Gen < window.gen` → `staleGen++`, return. If
     `h.Gen > window.gen` → `window.reset(h.Gen)`, `fecwin.reset(h.Gen)` (new
     session §8.4/§8.6).
  2. If `real`, also feed `fecwin.observeData(h.Gen, h.Seq, h.PTS, payload)`;
     if that completes a recoverable block, recover that frame too (recurse once
     via the same ingest path with `real=false`; recovery can only fill one hole
     per block so no unbounded recursion).
  3. `window.admit(h.Gen, h.Seq, h.PTS, payload)` → `(deliver, lost, dup,
     stale)`. Update counters (`lost += lost`, `duplicate++` if dup). For each
     `frameRec` in `deliver`: reconstruct a `Header` (Magic, TypeAudio, gen,
     running seq, pts, len) and call `r.deliver(h, payload)`, `delivered++`.
- **TCP accept goroutine**: `Accept()` in a loop until `done`; per accepted
  conn, `wg.Add(1)` and start a conn-reader goroutine.
- **TCP conn-reader goroutine**: read a `uint32` big-endian length prefix, then
  that many bytes into a buffer, `DecodeFrame`, `ingest(h, payload, true)`
  (TCP carries no parity → always `real`, no FEC). On EOF/error close and exit;
  the master's `tcpTransport` reconnects, the watchdog (E) covers a long gap.
- **Shutdown (Close)**: close `done`, close `listener` (unblocks Accept), close
  any tracked conns, `wg.Wait()`. UDP handlers remain registered on the Mux but
  are never called after the node shuts the Mux (K); we don't have an
  Unregister in the contract and don't need one — a closed receiver simply
  returns early (guard a `closed` flag in `ingest`).

Locking: **one** `Receiver.mu` guarding `window` + `fecwin` + the running
delivery seq. Counters are `atomic.Uint64` (read by `Counters()` from the API
goroutine without the lock). The TCP listener/conn lifecycle uses `done` + `wg`,
no extra mutex beyond `mu` for the window.

### FEC recovery timing (the subtle part)

A block is `[baseSeq, baseSeq+3]`. We can recover **one** missing data frame
once we hold parity + the other three data frames. Recovery therefore fires from
whichever arrival is the *last* of those four (the 3rd data frame if parity came
first, or the parity if it came last). `observeData`/`observeParity` both check
the same completion condition: `parity != nil && countHave == 3` → reconstruct
the single missing data payload (`XORInto(recovered, parity)` then `XORInto`
each present payload). The recovered frame's `PTS` is computed as
`block.pts[present] ± k·FrameNanos` from a present neighbor's PTS and seq delta
(PTS is linear in seq within a session, §8.2), so the recovered frame carries a
correct timestamp for E to schedule. The recovered frame then flows through
`ingest(real=false)` → reorder window → deliver. Double loss in a block is
unrecoverable: those seqs fall out as `Lost` when the reorder window slides past
them (E plays silence, §8.5).

---

## 4. Edge cases & failure handling (spec-referenced)

- **Stale generation (§8.4)**: any frame/parity with `Gen` below the receiver's
  current gen is dropped (`StaleGen++`) before touching the window; a higher gen
  resets both window and FEC state. This is how a new play / master change
  cleanly supersedes the old session even if late datagrams from the old gen are
  still in flight.
- **Master streams to itself over localhost (§8.2)**: the master's sink is just
  another `Endpoint{Addr: 127.0.0.1:STREAM_PORT}`; its own `Receiver` gets the
  frames through the identical UDP/TCP path. No special "self" branch in G.
- **UDP single loss (§8.4)**: recovered via FEC parity; `Recovered++`,
  `Delivered++`. The recovered payload is byte-identical to the original (XOR is
  exact); zero-padding handles the (rare, non-pcm) short-payload case via
  `XORInto`'s clamp, and `parLen`/per-frame `PayloadLen` restores the true length
  on recovery.
- **UDP double loss in a block (§8.4)**: unrecoverable; the two seqs become
  `Lost`, the reorder window slides past them, E inserts silence. We do NOT block
  waiting — the window's `maxAhead` bound forces forward progress.
- **Reorder beyond the window**: a frame arriving more than `maxAhead` ahead of
  `next` forces the window to slide (delivering/abandoning the oldest), counting
  the skipped seqs as `Lost`. Prevents unbounded buffering on a stuck low seq.
- **Duplicate frames (§8.4 FEC + reorder)**: a frame delivered by FEC recovery
  and then its real datagram also arriving (or a UDP dup) is dropped at the
  reorder window (`seq < next` or already-delivered) → `Duplicate++`, never
  double-delivered. At-most-once delivery is the window's invariant.
- **Truncated / garbage datagram on the open UDP port (§8.4, S §4)**:
  `DecodeFrame` returns `ErrShort`/`ErrBadMagic`; counted `Malformed`, dropped.
  The Mux already filters bad magic / short before dispatch, so this is
  defense-in-depth for `PayloadLen` truncation specifically.
- **TCP reconnect (§8.4)**: master-side dialer reconnects with capped backoff;
  while disconnected, `SendFrame` drops that endpoint's frames (counted). On
  reconnect, streaming resumes mid-session — the receiver's reorder window slid
  forward during the gap, so resumed frames with a higher seq just re-anchor
  `next` forward and deliver; the gap is `Lost` (silence), exactly as spec wants.
- **TCP listener absent (tests / UDP-only)**: `TCPListener == nil` disables the
  accept loop cleanly; UDP path unaffected.
- **Membership change mid-session (§5/§5.2)**: `SetEndpoints` adds/removes
  destinations without bumping gen or seq — the stream continues to the new set.
  A member that joined late just starts receiving from the current seq and
  re-anchors its window; missed earlier frames are simply silence on its side.
- **Source faster/slower than 20 ms**: G imposes no rate; it sends exactly when
  `SendFrame` is called. Seq/PTS come from the caller (H) and the FEC block
  cadence is purely per-4-audio-frames, independent of wall time.
- **Empty endpoint set**: `SendFrame` with zero endpoints still advances
  seq/gen/FEC bookkeeping (so a member that joins next frame sees a consistent
  stream) but writes nowhere. No error.
- **Payload length**: pcm payloads are always `FrameBytes`; opus payloads vary.
  G never assumes `FrameBytes` for transport — it uses `Header.PayloadLen`.
  Only the FEC parity buffer is sized `FrameBytes` (the max), and `XORInto`
  zero-pads shorter payloads, matching the spec's "XOR … padded" (§8.4).
- **Receiver close vs in-flight Mux callback**: a `closed` guard at the top of
  `ingest` makes a late `onUDP` call after `Close` a no-op (it still holds `mu`
  briefly); the Mux read goroutine is stopped by S's `Mux.Close` in K's shutdown
  order, so this is belt-and-suspenders.

---

## 5. Test plan (all loopback / in-process, no hardware, no root)

### `internal/stream/fec_test.go`
- `TestFECBlockParityXOR` — fold 4 known payloads; parity == XOR of all four.
- `TestFECRecoverMissingFromParity` — drop one of four data, supply parity+3 →
  `observeParity`/`observeData` reconstructs the exact missing payload + PTS.
- `TestFECRecoverWhenParityArrivesLast` — 3 data then parity triggers recovery.
- `TestFECRecoverWhenDataArrivesLast` — parity then 3rd data triggers recovery.
- `TestFECDoubleLossUnrecoverable` — drop two of four → no recovery emitted.
- `TestFECShortPayloadPadding` — mixed/short payload lengths zero-pad correctly;
  recovered payload trimmed to its `PayloadLen`.
- `TestFECPartialFlush` — `flushPartial` on a 2-frame tail emits usable parity.
- `TestFECResetOnGen` — `reset` drops old-gen blocks; new gen starts clean.

### `internal/stream/recvwindow_test.go`
- `TestWindowInOrderDelivery` — seqs 0,1,2,3 deliver immediately in order.
- `TestWindowReordersWithinWindow` — 0,2,1,3 → delivered as 0,1,2,3.
- `TestWindowGapBecomesLost` — 0,1,(skip 2),3,4 with no recovery → seq 2 counted
  Lost, 3 and 4 delivered after the slide.
- `TestWindowDuplicateDropped` — re-admit an already-delivered seq → `dup`, not
  re-delivered.
- `TestWindowOverflowEvicts` — admit `next+maxAhead+1` → window slides, oldest
  gap counted Lost, forward progress.
- `TestWindowResetReanchors` — `reset(newGen)` then admit re-anchors `next` to
  the first new-gen seq.
- `TestWindowFirstFrameAnchors` — first admitted seq (nonzero) anchors `next` to
  it (joining mid-stream).

### `internal/stream/sender_test.go`
- `TestSenderUDPFanOut` — two fake endpoints (two loopback Muxes); one SendFrame
  arrives at both with identical header+payload.
- `TestSenderUDPParityCadence` — 4 SendFrames → exactly one TypeFEC datagram per
  endpoint after the 4th; none after frames 1–3.
- `TestSenderSeqMonotonic` — N SendFrames produce Seq 0..N-1; Gen constant.
- `TestSenderEmptyEndpoints` — SendFrame with no endpoints advances seq, no write,
  no panic.
- `TestSenderTCPLengthPrefixed` — TCP sender to a loopback listener; received
  bytes are `uint32 len | header | payload`, decode matches.
- `TestSenderTCPReconnect` — start sender before listener up; dialer backs off,
  then connects when listener opens; subsequent frames arrive.
- `TestSenderTCPNoParity` — TCP path emits no TypeFEC frames.
- `TestSenderClose` — Close stops TCP dialers (no goroutine leak via
  `goleak`/manual wg) and is idempotent.
- `TestParseTransport` — "tcp"→TCP, "udp"/""/junk→UDP.

### `internal/stream/receiver_test.go`
- `TestReceiverUDPDeliver` — sender→receiver over two loopback Muxes; frames
  delivered in order via the callback; `Delivered` counter matches.
- `TestReceiverFECRecovery` — sender emits 4+parity; drop the 2nd data datagram
  in a filtering Mux shim → callback still receives all 4; `Recovered==1`,
  `Lost==0`.
- `TestReceiverDoubleLossSilence` — drop 2 of 4 → 2 frames delivered, `Lost==2`,
  no recovery, no block.
- `TestReceiverStaleGenDropped` — feed gen=5 then a gen=4 frame → `StaleGen++`,
  not delivered; a gen=6 frame resets and delivers.
- `TestReceiverReorderThenDeliver` — out-of-order UDP arrival delivered ordered.
- `TestReceiverDuplicateCounted` — same datagram twice → one delivery,
  `Duplicate==1`.
- `TestReceiverTCPDeliver` — real `net.TCPListener` on 127.0.0.1:0; a TCP sender
  writes 3 length-prefixed frames → all delivered; counters correct.
- `TestReceiverMalformedDropped` — hand a too-short / bad-`PayloadLen` datagram
  to `onUDP` → `Malformed++`, no delivery, no panic.
- `TestReceiverClose` — Close stops the accept loop and conn readers (no leak),
  idempotent; post-close ingest is a no-op.

### End-to-end within the package (sender↔receiver, no other pieces)
- `TestRoundTripUDPLossy` — wire a Sender to a Receiver through a loopback Mux
  pair with a deterministic 1-in-5 drop filter; stream 100 frames; assert every
  in-block single loss recovered, double losses (none injected) zero, and the
  delivered seq set is contiguous where recoverable. Uses a fake DeliverFunc
  collecting `(Header, payload-copy)`.
- `TestRoundTripTCPClean` — Sender(TCP)→Receiver(TCP listener); 100 frames; all
  delivered in order, zero Lost, zero Recovered, zero FECParity.

All UDP tests build two `*Mux` instances on `127.0.0.1:0` (via
`netx.BindTCPUDP`, or a raw `net.ListenUDP` helper in `_test.go` if K hasn't
landed netx), register the receiver, and use `Mux.WriteTo` / a drop-filtering
wrapper for loss injection. No multicast, no real audio device, no root.
