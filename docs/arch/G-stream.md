# G — audio source server & subscriber client

Source of truth: [docs/README.md](../README.md) §8.2, §8.4, §8.6, §8.7; integrator
decisions [DECISIONS.md](DECISIONS.md) **D12, D13, D18 (→D22), D22, D23, D24, D28**.
Contracts I code against from [S-skeleton.md](S-skeleton.md), all **read-only** to me:

- `internal/stream/wire.go` — `Header`, `Encode`, `AppendFrame`, `Decode`,
  `DecodeFrame`, `XORInto`; packet-type consts `TypeAudio 0x01`, `TypeFEC 0x02`,
  control `TypeHello 0x20`, `TypeBye 0x21`, `TypeRestart 0x22`, `TypeReconfig
  0x23`; PCM consts (`FrameBytes`, `FrameNanos`, …); `Magic`, `HeaderSize`.
- `internal/stream/mux.go` — `Mux`: `Register(type,h)`, `WriteTo(pkt,addr)`,
  `LocalAddr()`. The member-side STREAM_PORT UDP socket.
- `internal/contracts` — `SourceStats` (D28); I do **not** implement any contract
  interface, I expose concrete types H consumes via a small Go-style interface H
  declares on its side.

This piece is the **subscribe-based** stream layer (D22, supersedes the old
push/`SetEndpoints` model — there is no `Resolver`, no `SetEndpoints`, no
master-dials-members anywhere here):

- **Source server** (`internal/source/*`, master side, §8.2/§8.7): listens on
  `SOURCE_PORT` (TCP+UDP), keeps a **subscriber registry** keyed by observed
  addr with HELLO keepalive (5 s) / expiry (15 s), a **ring buffer** of recently
  released frames sized `max(2×bufferMs, 1 s)`, **burst-primes** new/restarting
  subscribers (D24), broadcasts **RECONFIG** (incl. the stop flag, §8.6), and
  **fans out** every released frame to all live subscribers — UDP datagrams +
  XOR FEC parity every 4 frames, or length-prefixed frames down each TCP
  subscriber connection. Surfaces `SourceStats` (D28).
- **Subscriber client** (`internal/stream/*` minus the S files, member side):
  dials/HELLOs the master's `SOURCE_PORT` (UDP from the STREAM_PORT mux socket,
  or TCP to SOURCE_PORT), sends keepalive HELLO / BYE / RESTART, receives audio
  on the member-side mux (`0x01`/`0x02`) with reorder + FEC recovery, or on the
  TCP subscription connection, delivers `(Header, payload)` to a callback, counts
  loss/recovery, and runs the **starvation watchdog** that issues RESTART (§8.6).

The master's own sink subscribes over loopback like any other client — no special
self path. Pure transport/control: no decode, no clock, no jitter buffer (D/E/F/H).

Preserved internals from the previous round (re-presented in this structure):
`fecBlock` (sender parity build), `recoveryWindow` (receiver single-loss XOR
recovery), `reorderBuffer` (ordered/at-most-once delivery), and the **uint32-BE
TCP length prefix per D13**.

---

## 1. Package / file layout

Two packages. `internal/source` is **new and owned entirely by G**.
`internal/stream` adds the subscriber-client files alongside the read-only S
files (`wire.go`, `mux.go` and their tests stay untouched).

```
internal/source/server.go        Server: SOURCE_PORT TCP+UDP listeners, control intake (HELLO/BYE/RESTART),
                                  RECONFIG broadcast, ReleaseFrame fan-out, gen, SourceStats. One per node.
internal/source/registry.go      subscriber registry: addr-keyed entries, transport (udp|tcp), keepalive/expiry,
                                  per-tcp conn handle; add/refresh/remove/expire under the server mutex.
internal/source/ring.go          ringBuffer: fixed-capacity ring of released (Header,payload) frames; Prime() selects
                                  frames whose pts+bufferMs deadline is still future (D24); resize on bufferMs change.
internal/source/prime.go         primer: paces a burst to one subscriber — UDP ~4× realtime (one frame/~5 ms),
                                  TCP back-to-back; counts primes.
internal/source/fanout.go        per-frame fan-out: UDP datagram + FEC parity every 4 frames (fecBlock), TCP length-prefixed.
internal/source/server_test.go   subscribe→prime→live, keepalive/expiry, RESTART re-prime, RECONFIG+stop, stats, udp+tcp.
internal/source/registry_test.go add/refresh/expire, addr-key identity, transport routing, count.
internal/source/ring_test.go     capacity wrap, Prime deadline cutoff, resize, empty.
internal/source/prime_test.go    udp pacing cadence, tcp back-to-back, prime count.

internal/stream/client.go        Client: subscriber client. Subscribe(master,gen,settings)→HELLO loop, BYE/RESTART,
                                  Deliver callback, starvation watchdog→RESTART, Counters. One per node.
internal/stream/client_udp.go    udp subscription: HELLO from the mux socket to SOURCE_PORT; receive 0x01/0x02 via the mux.
internal/stream/client_tcp.go    tcp subscription: dial SOURCE_PORT, send control frames, read length-prefixed audio.
internal/stream/fec.go           fecBlock (parity build, source side) + recoveryWindow (single-loss XOR recovery, client side).
internal/stream/recvwindow.go    reorderBuffer: ordered, at-most-once delivery; gap accounting; flush on gen change.
internal/stream/client_test.go   udp deliver, FEC recovery, reorder, stale-gen drop, tcp deliver, watchdog→RESTART, counters.
internal/stream/fec_test.go      block parity build, recover-from-parity+3, parity-last/data-last, double-loss, padding, reset.
internal/stream/recvwindow_test.go in-order, reorder, dup drop, overflow evict, gen reset, mid-stream anchor.
```

Exported surface is intentionally tiny: `source.Server` (+ `Config`,
`Frame`), `stream.Client` (+ `Config`, `Subscription`, `DeliverFunc`,
`Counters`), `stream.ParseTransport`/`Transport`. Everything else is unexported.

---

## 2. Concrete Go API

### 2.1 Common: transport selector & frame

```go
package stream

// Transport selects the wire transport for a subscription (group setting §8.4),
// mirroring contracts.GroupSettings.Transport ("udp" | "tcp").
type Transport int

const (
	TransportUDP Transport = iota // 0: datagrams + XOR FEC (default §8.4)
	TransportTCP                  // 1: persistent length-prefixed conn on SOURCE_PORT
)

// ParseTransport maps the group-setting string to a Transport. Unknown -> UDP,
// so a malformed record never wedges a subscription.
func ParseTransport(s string) Transport
```

```go
package source

// Frame is one released audio frame handed to the source for fan-out. The
// source owns header bookkeeping (Seq, Gen) — H supplies only pts + payload via
// ReleaseFrame, and the server stamps Seq/Gen. (Frame is the ring's element.)
type Frame struct {
	Seq     uint64
	PTS     int64
	Payload []byte // canonical PCM (FrameBytes) for pcm; opus bytes for opus — opaque here
}
```

### 2.2 Source server (master side) — `internal/source`

**One `Server` per node** (every node binds SOURCE_PORT per D22; the listeners
matter only while this node is a master). The group engine (H) calls
`StartSession` on `Play`, `ReleaseFrame` per 20 ms frame from its release ticker,
`Reconfig` on a live settings change (D23), and `StopSession`/`Close` at
end-of-source or stop (§8.6).

```go
package source

import (
	"log/slog"
	"net"
	"sync"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// Server is the audio source: SOURCE_PORT control intake + per-frame fan-out to
// the subscriber registry, with a ring buffer for burst priming (§8.2/§8.7/D24).
//
// Concurrency: ONE mutex guards the registry, ring, current session (gen,
// transport, bufferMs, active), and the FEC accumulator. ReleaseFrame is called
// from H's single release goroutine; control packets arrive on the UDP read
// goroutine and per-TCP-conn goroutines; StartSession/Reconfig/StopSession/Close
// come from H's API goroutine. All take the one mutex. Stats are atomics read
// lock-free for /api/status.
type Server struct {
	mu       sync.Mutex
	self     id.ID
	udp      *net.UDPConn      // SOURCE_PORT UDP (control in, audio+fec out)
	tcpLn    *net.TCPListener  // SOURCE_PORT TCP (control + audio per conn)
	reg      registry          // addr-keyed subscribers
	ring     ringBuffer        // recently released frames (prime source)
	fec      fecBlock          // UDP parity accumulator (per session)

	active   bool              // a session is running
	gen      uint32            // current session generation
	transport stream.Transport // current session transport
	bufferMs int               // current session bufferMs (ring sizing + prime cutoff)
	seq      uint64            // next Seq to stamp

	stats    sourceCounters    // atomics: connects, restarts, primes
	done     chan struct{}
	wg       sync.WaitGroup
	log      *slog.Logger
}

// Config wires a Server to its already-bound SOURCE_PORT sockets (from
// netx.BindTCPUDP, owned by K). Both are required: any node can become master.
type Config struct {
	Self id.ID
	UDP  *net.UDPConn      // SOURCE_PORT UDP socket
	TCP  *net.TCPListener  // SOURCE_PORT TCP listener
	Log  *slog.Logger      // nil -> slog.Default with comp=source
}

// NewServer builds a Server; no goroutines yet.
func NewServer(cfg Config) *Server

// Run starts the UDP control read loop, the TCP accept loop, and the expiry
// sweeper (1 s tick → drop subscribers unseen for 15 s). Non-blocking; call once.
func (s *Server) Run()

// StartSession arms a new play session: bumps to the given gen, sets transport
// and bufferMs, resizes/clears the ring to max(2*bufferMs,1s), resets the FEC
// accumulator and Seq=0, marks active. Existing subscribers persist across the
// StartSession boundary (they get RECONFIG and a fresh prime on their next
// HELLO/RESTART under the new gen); a RECONFIG (non-stop) is broadcast so they
// resubscribe promptly (D23). Called by H on Play and on settings change.
func (s *Server) StartSession(gen uint32, t stream.Transport, bufferMs int)

// ReleaseFrame fans out one frame. The server stamps Seq (next) and uses the
// session Gen; pts is master-clock ns (§8.2). It appends the frame to the ring,
// sends it to every live subscriber on the session transport, and (UDP) folds it
// into the FEC block — emitting a parity datagram to all UDP subscribers after
// every 4th frame (§8.4). No error: per-subscriber write failures are counted,
// never propagated (a dead path must not stall H's ticker, §8.2). Returns the
// Seq used. No-op (returns 0) if no session is active.
func (s *Server) ReleaseFrame(pts int64, payload []byte) uint64

// Reconfig broadcasts a RECONFIG control (non-stop) to every subscriber: "gen/
// settings changed, re-read group settings and resubscribe" (§8.7/D23). H calls
// it right after StartSession on a live settings change so subscribers reconnect
// without waiting for their watchdog.
func (s *Server) Reconfig()

// StopSession ends the current session: flushes any partial FEC tail block
// (parity for a 1..3-frame remainder, D13), broadcasts RECONFIG with the STOP
// flag (the explicit end-of-session notice, §8.6), marks inactive, and clears
// the ring. Subscribers are NOT removed (they keep their entry and re-prime on
// the next play's RECONFIG/HELLO). Idempotent.
func (s *Server) StopSession()

// Stats returns a snapshot of source stats for /api/status and the replicated
// playback record (D28). Clients = current live subscriber count.
func (s *Server) Stats() contracts.SourceStats

// Close stops all goroutines (read loops, accept loop, sweeper) and closes
// tracked TCP subscriber conns. It does NOT close the SOURCE_PORT sockets — K
// owns them (symmetry with the Mux). Idempotent.
func (s *Server) Close() error
```

`SourceStats` (D28): `Clients` = live registry size at call time; `Connects` =
total HELLO-subscribes accepted (a brand-new addr, not a keepalive); `Restarts` =
RESTART re-prime requests served; `Primes` = burst primes sent (connect +
restart). `Clients` is computed under the mutex; the other three are atomics.

### 2.3 Subscriber registry — `internal/source/registry.go`

```go
package source

import (
	"net"
	"net/netip"
	"time"

	"ensemble/internal/stream"
)

// registry holds the live subscribers, keyed by their OBSERVED source address
// (§8.7: UDP audio flows back to the addr the HELLO came from; TCP audio rides
// the accepted conn). Guarded by Server.mu (no own lock).
type registry struct {
	subs map[netip.AddrPort]*subscriber
}

// subscriber is one live destination.
type subscriber struct {
	addr     netip.AddrPort // UDP: HELLO source addr (= STREAM_PORT mux socket); TCP: RemoteAddr
	tr       stream.Transport
	conn     net.Conn       // TCP only (nil for UDP); writes go here length-prefixed
	lastSeen time.Time      // last HELLO/RESTART; expiry at +15 s
	wmu      sync.Mutex     // TCP: serializes writes to conn (fan-out + prime)
}

// upsert records a HELLO from addr. Returns (sub, isNew): isNew true on a
// previously-unknown addr (Connects++). Refreshes lastSeen otherwise (keepalive).
func (r *registry) upsert(addr netip.AddrPort, t stream.Transport, conn net.Conn, now time.Time) (sub *subscriber, isNew bool)

// get returns the subscriber for addr (RESTART/BYE lookups), or nil.
func (r *registry) get(addr netip.AddrPort) *subscriber

// remove drops a subscriber (BYE, or TCP conn error/close).
func (r *registry) remove(addr netip.AddrPort)

// expire removes subscribers whose lastSeen < now-ttl (15 s, §8.7); returns the
// removed TCP conns so the caller can close them outside the map mutation.
func (r *registry) expire(now time.Time, ttl time.Duration) []net.Conn

// live returns the current subscriber slice (snapshot for fan-out) and count.
func (r *registry) live() []*subscriber
```

### 2.4 Ring buffer & primer — `internal/source/ring.go`, `prime.go`

```go
package source

// ringBuffer is a fixed-capacity ring of recently released frames, the source
// for burst priming (§8.2/D24). Sized max(2*bufferMs/FrameDuration, 1s worth) of
// frames; oldest frames overwrite. Guarded by Server.mu.
type ringBuffer struct {
	frames []ringSlot // capacity slots; circular
	head   int        // index of the next write
	count  int        // valid slots (<= cap)
	bufMs  int        // current bufferMs (prime deadline = pts + bufMs)
}

type ringSlot struct {
	seq     uint64
	pts     int64
	payload []byte // owned copy
}

// resize (re)allocates the ring to hold max(2*bufferMs, 1000) ms of 20 ms
// frames and clears it. Called from StartSession (new session / settings change).
func (b *ringBuffer) resize(bufferMs int)

// push appends a released frame (copying payload), overwriting the oldest when full.
func (b *ringBuffer) push(seq uint64, pts int64, payload []byte)

// prime returns the frames to burst to a (re)joining subscriber, oldest→newest:
// every ring frame whose playout deadline pts+bufferMs is STILL IN THE FUTURE
// relative to nowMaster — older frames are useless to the newcomer and skipped
// (D24). nowMaster is the source's current master-clock ns (H passes it; the
// source has no clock of its own).
func (b *ringBuffer) prime(nowMaster int64) []ringSlot

// clear empties the ring (StopSession).
func (b *ringBuffer) clear()
```

The prime cutoff needs `nowMaster`. The source does not own a `Clock`; rather
than thread one in, **H stamps every `ReleaseFrame` pts itself and the most-recent
released pts is monotone**, so the server tracks `lastPTS` (the pts of the newest
ring frame) and computes the cutoff as `lastPTS - bufferMs` worth of frames:
`prime` keeps slots with `slot.pts >= lastPTS - bufferMs*1e6` (i.e. only the most
recent `bufferMs` of audio — exactly the frames whose deadline hasn't passed
relative to the live edge). This is clock-free and equivalent for a continuously
releasing source. (Edge: §4 "prime when paused/idle".)

```go
package source

// primeUDP bursts the selected frames to a UDP subscriber via the SOURCE_PORT
// UDP socket, paced ~4× realtime: one frame per ~5 ms (D24), so it outruns the
// live stream without flooding. Each frame is sent as a TypeAudio datagram with
// its original Seq/PTS/Gen; NO FEC during a prime (the live FEC cadence continues
// independently). Runs in its own goroutine; counts primes++ when done.
func (s *Server) primeUDP(sub *subscriber, frames []ringSlot, gen uint32)

// primeTCP writes the selected frames back-to-back (length-prefixed) on the
// subscriber's conn (TCP flow control paces it, D24). Holds sub.wmu so it never
// interleaves mid-frame with live fan-out writes. Counts primes++.
func (s *Server) primeTCP(sub *subscriber, frames []ringSlot, gen uint32)
```

### 2.5 Subscriber client (member side) — `internal/stream/client.go`

**One `Client` per node**, long-lived. It owns exactly one active
`Subscription` at a time (a node follows exactly one master). H calls
`Subscribe` when the current master/gen/settings change (incl. the master itself
subscribing to its own loopback source), and `Unsubscribe` on stop / leaving.

```go
package stream

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
)

// DeliverFunc receives one ordered, de-duplicated, FEC-recovered frame: the
// parsed Header and its payload. payload aliases the client's buffer and is ONLY
// valid for the duration of the call — the Sink (E) copies on Push. Called
// serialized per subscription; must not block long (Sink.Push is non-blocking, S).
type DeliverFunc func(h Header, payload []byte)

// Client is the member-side subscriber: it HELLOs a master's SOURCE_PORT, keeps
// the subscription alive, receives audio (UDP via the mux, or TCP), recovers/
// reorders, and delivers frames. It also runs the starvation watchdog that
// issues RESTART, then gives up (§8.6).
//
// Concurrency: ONE mutex guards the active subscription pointer + lifecycle.
// The receive paths (mux callback on the S read goroutine for UDP; a conn-reader
// goroutine for TCP) funnel into the subscription's reorder/FEC state under the
// subscription's own mutex. Counters are atomics. Subscribe/Unsubscribe come
// from H's goroutine.
type Client struct {
	mu   sync.Mutex
	mux  *Mux              // member STREAM_PORT UDP socket (HELLO out, 0x01/0x02 in)
	sub  *subscription     // current active subscription (nil if none)
	ctr  clientCounters    // atomic lifetime counters
	log  *slog.Logger
}

// ClientConfig wires a Client. Mux + Deliver are required.
type ClientConfig struct {
	Mux     *Mux
	Deliver DeliverFunc
	Log     *slog.Logger
}

// NewClient builds a Client and registers the mux handlers for TypeAudio /
// TypeFEC (they dispatch to whatever subscription is active, or drop). No
// subscription yet.
func NewClient(cfg ClientConfig) *Client

// Subscribe starts (or replaces) the active subscription to master at sourceAddr
// with the given session generation and transport. It tears down any prior
// subscription (BYE if reachable), then: sends an initial HELLO with the prime-me
// flag, starts the 5 s keepalive-HELLO loop and the starvation watchdog, and (TCP)
// dials sourceAddr and starts the conn-reader. Idempotent for an unchanged
// (addr,gen,transport). Returns an error only on an immediate TCP dial failure;
// UDP never errors here (fire-and-forget HELLO).
func (c *Client) Subscribe(sourceAddr netip.AddrPort, gen uint32, t Transport) error

// Unsubscribe sends BYE, stops the keepalive/watchdog/reader, and clears the
// active subscription. Idempotent; safe if no subscription is active.
func (c *Client) Unsubscribe()

// OnReconfig is the hook H wires to the RECONFIG control. The Client does NOT
// parse RECONFIG itself (RECONFIG arrives on the SOURCE_PORT control path which,
// for UDP, the client receives as... see §3): instead H, on observing the
// session gen/settings change (it re-reads replicated group settings, D23),
// calls Subscribe again with the new gen/transport. Provided so the TCP reader,
// which DOES see RECONFIG inline on its conn, can notify H. Signature:
//   c.onReconfig(stop bool)  // internal; H injects via ClientConfig if needed
// (kept internal in v1; see §3 "RECONFIG handling".)

// Counters returns lifetime transport-health counters for /api/status (§9.1, K).
func (c *Client) Counters() Counters

// Close unsubscribes and stops everything. Idempotent.
func (c *Client) Close() error

// Counters are monotonic per client (NOT per session — the sink resets its own
// per-session stats; these are lifetime transport health).
type Counters struct {
	Delivered uint64 // frames handed to DeliverFunc
	Recovered uint64 // frames reconstructed by FEC
	Lost      uint64 // gaps the reorder window gave up on (E plays silence)
	Duplicate uint64 // frames dropped as already-delivered
	StaleGen  uint64 // frames dropped: gen below the active subscription gen
	Malformed uint64 // datagrams/chunks that failed Decode (UDP garbage)
	FECParity uint64 // parity datagrams received (type 0x02)
	Restarts  uint64 // RESTART controls this client issued (starvation, §8.6)
}

type clientCounters struct { // atomic mirror
	delivered, recovered, lost, duplicate, staleGen, malformed, fecParity, restarts atomic.Uint64
}
```

```go
// subscription is one active link to a master's source. It holds the wire state
// (reorder window + FEC recovery), the keepalive/watchdog timers, and (TCP) the
// conn. Guarded by its own mutex; the Client.mu only guards the *subscription
// pointer.
type subscription struct {
	mu       sync.Mutex
	addr     netip.AddrPort // master SOURCE_PORT
	gen      uint32         // active generation; frames below are stale-dropped
	tr       Transport
	mux      *Mux           // for UDP HELLO/BYE/RESTART out + (no-op) receive
	conn     net.Conn       // TCP only

	window   reorderBuffer  // ordered, at-most-once delivery
	fecwin   recoveryWindow // pending FEC blocks awaiting a single missing frame
	lastRecv int64          // local-ns of the most recent accepted frame (watchdog)

	deliver  DeliverFunc
	ctr      *clientCounters
	done     chan struct{}
	wg       sync.WaitGroup
	log      *slog.Logger
}
```

### 2.6 UDP & TCP subscription paths — `client_udp.go`, `client_tcp.go`

```go
// --- UDP (client_udp.go) ---

// helloUDP sends a control datagram (TypeHello/Bye/Restart) from the member's
// STREAM_PORT mux socket to the master's SOURCE_PORT (§8.7). Sending from the mux
// socket is what makes the source stream back to the observed addr (D22). The
// 1-byte payload flag: bit0 = prime-me (Hello) — set on the initial Hello and on
// Restart; clear on keepalive Hello and on Bye.
func (s *subscription) helloUDP(typ byte, primeMe bool)

// onAudioUDP / onFECUDP are dispatched by the Client's mux handlers for the
// active subscription (the Client routes 0x01/0x02 to s). They run on the S mux
// read goroutine and must not block: Decode, then ingest under s.mu.

// --- TCP (client_tcp.go) ---

// dialTCP dials the master's SOURCE_PORT, sends the initial HELLO (control frame
// on the conn), and starts readTCP. Control out (keepalive HELLO / BYE / RESTART)
// also goes as frames on this conn.
func (s *subscription) dialTCP() error

// readTCP loops: read uint32-BE length prefix (D13), then that many bytes; Decode
// the chunk. TypeAudio → ingest(real). TypeReconfig → handle (resubscribe / stop,
// see §3). Other types ignored. On EOF/error: close, signal watchdog (the master
// may have died); H re-subscribes on the next cluster change.
func (s *subscription) readTCP()
```

### 2.7 FEC & reorder window — `internal/stream/fec.go`, `recvwindow.go`

Re-presented verbatim-in-spirit from the previous round; these are the preserved
good internals. Source side uses `fecBlock`; client side uses `recoveryWindow`
and `reorderBuffer`.

```go
// --- source side (fanout.go uses this) ---

// fecBlock accumulates up to 4 audio payloads (zero-padded to the longest) and
// the base Seq/Gen of the block, then produces one XOR parity packet. UDP only.
type fecBlock struct {
	count   int
	baseSeq uint64
	gen     uint32
	parity  [FrameBytes]byte // running XOR
	maxLen  int              // longest payload folded -> parity PayloadLen
}

func (b *fecBlock) fold(gen uint32, seq uint64, payload []byte) // XOR in, count++
func (b *fecBlock) ready() bool                                 // count == 4
// parityPacket encodes the parity datagram: Header{TypeFEC, Gen, Seq=baseSeq,
// PTS=0, PayloadLen=maxLen}; resets the block. nil if count==0.
func (b *fecBlock) parityPacket(buf []byte) []byte
// flushPartial emits parity for a 1..3-frame tail at StopSession (D13); resets.
func (b *fecBlock) flushPartial(buf []byte) []byte
func (b *fecBlock) reset(gen uint32)

// --- client side (subscription uses these) ---

// recoveryWindow tracks, per FEC block (keyed by baseSeq within gen), which of
// the 4 data frames + the parity have arrived. When exactly one data frame is
// missing AND parity is present, it reconstructs the missing payload.
type recoveryWindow struct {
	gen    uint32
	blocks map[uint64]*fecState
}
type fecState struct {
	baseSeq uint64
	have    [4]bool
	payload [4][]byte
	pts     [4]int64
	parity  []byte
	parLen  int
}
func (w *recoveryWindow) observeData(gen uint32, seq uint64, pts int64, payload []byte) (rseq uint64, rpts int64, rpay []byte, ok bool)
func (w *recoveryWindow) observeParity(gen uint32, baseSeq uint64, parity []byte) (rseq uint64, rpts int64, rpay []byte, ok bool)
func (w *recoveryWindow) reset(gen uint32)

// reorderBuffer delivers frames in Seq order, at most once, tolerating bounded
// out-of-order arrival and gaps. It does NOT buffer for the jitter deadline (E's
// job) — only fixes ordering/dedup over the FEC/reorder horizon (maxAhead = 32
// frames, ~640 ms; far smaller than E's 150 ms+ jitter buffer so no double-buffer).
type reorderBuffer struct {
	gen      uint32
	next     uint64
	started  bool
	pend     map[uint64]frameRec
	maxAhead uint64
}
type frameRec struct {
	pts     int64
	payload []byte
}
func (b *reorderBuffer) admit(gen uint32, seq uint64, pts int64, payload []byte) (deliver []frameRec, lost int, dup bool, stale bool)
func (b *reorderBuffer) reset(gen uint32)
```

---

## 3. Control flow, goroutines, locking

### Source server — startup / control / fan-out / shutdown

- **Startup** (`NewServer` then `Run`): `Run` starts three goroutines — the **UDP
  control reader** (`ReadFromUDPAddrPort` on the SOURCE_PORT socket), the **TCP
  accept loop**, and the **expiry sweeper** (1 s ticker). All stop on `done`.

- **UDP control intake** (read goroutine): read a datagram; `DecodeFrame`; branch
  on `Header.Type`:
  - `TypeHello` (0x20): `now := time.Now()`; under `mu`,
    `sub, isNew := reg.upsert(from, TransportUDP, nil, now)`; if `isNew`
    `connects++`. If the payload prime-me flag is set (initial HELLO or after the
    sub was absent), and a session is active, snapshot `frames := ring.prime(...)`
    and `gen` under the lock, then **outside the lock** launch `primeUDP(sub,
    frames, gen)` (a goroutine, `wg.Add`). Keepalive HELLOs (flag clear) just
    refresh `lastSeen`.
  - `TypeBye` (0x21): under `mu`, `reg.remove(from)`.
  - `TypeRestart` (0x22): under `mu`, `restarts++`, refresh `lastSeen`, snapshot
    prime frames + gen; launch `primeUDP` (re-prime + resume, §8.6). Treated as a
    HELLO that always primes.
  - Control packets never carry audio; `Seq/PTS` are ignored.

- **TCP accept loop**: `Accept()` until `done`; per conn, `wg.Add(1)` and start a
  **conn control-reader** goroutine. That goroutine reads length-prefixed control
  frames (D13): the first frame must be a HELLO → under `mu`,
  `reg.upsert(RemoteAddr, TransportTCP, conn, now)`, `connects++` if new; if
  prime-me + active session, `primeTCP(sub, frames, gen)` (back-to-back, under
  `sub.wmu`). Subsequent frames on the conn: keepalive HELLO → refresh; RESTART →
  `restarts++` + re-prime; BYE or EOF/error → `reg.remove`, close conn, exit.

- **ReleaseFrame** (H's release goroutine, every 20 ms):
  1. Lock `mu`. If `!active`, unlock, return 0.
  2. `seq := s.seq; s.seq++`. `ring.push(seq, pts, payload)` (copies). Track
     `lastPTS = pts`.
  3. Build the audio packet once: `h := Header{Magic, TypeAudio, gen, seq, pts,
     len(payload)}`; `pkt := h.AppendFrame(scratch[:0], payload)`.
  4. `subs := reg.live()`. Fan out: UDP subs → `mux/udp.WriteToUDPAddrPort(pkt,
     sub.addr)` (the SOURCE_PORT UDP socket, **not** the member mux — the source
     writes from its own socket); TCP subs → under `sub.wmu`, write `uint32-BE
     len | pkt`. Per-sub write errors counted; a TCP write error marks the conn
     dead (`reg.remove` deferred to the reader's error path).
  5. UDP path: `fec.fold(gen, seq, payload)`; if `fec.ready()`, build parity and
     write it to every UDP sub.
  6. Unlock; return seq.
  The mutex is held across the fan-out. UDP `WriteTo` is non-blocking `sendto`;
  TCP writes go to kernel send buffers (a wedged TCP sub is the rare risk — see
  §4 "slow TCP subscriber"). This keeps seq/gen/ring/FEC consistent with one lock.

- **Reconfig**: under `mu`, snapshot subs + gen, build a RECONFIG packet
  (`Header{TypeReconfig, gen, 0, 0, 1}`, payload `[stop?1:0 byte]` = 0), and send
  it to every sub (UDP datagram to `sub.addr`; TCP length-prefixed on `sub.conn`).

- **StartSession**: under `mu` — `gen=newGen`, `transport=t`, `bufferMs=b`,
  `ring.resize(b)`, `fec.reset(gen)`, `seq=0`, `active=true`. Then (outside the
  lock) `Reconfig()` so existing subscribers resubscribe under the new gen (D23).

- **StopSession**: under `mu` — if active: `if par := fec.flushPartial(buf); par
  != nil` send it to UDP subs (D13 tail parity); build RECONFIG **with the stop
  flag** (payload `[1]`), send to all subs; `active=false`, `ring.clear()`.
  Subscribers keep their registry entry.

- **Expiry sweeper** (1 s tick): `conns := reg.expire(now, 15s)`; close the
  returned TCP conns. Under `mu` for the map mutation, conn `Close` outside it.

- **Locking**: **one** `Server.mu` for registry+ring+session+FEC+seq. `Connects/
  Restarts/Primes` are atomics (read lock-free by `Stats`); `Clients` snapshots
  `len(reg.subs)` under `mu`. Per-TCP-conn writes use `subscriber.wmu` (leaf lock,
  never nests `mu`). Prime goroutines read an immutable frame snapshot taken under
  `mu`, then write without `mu` (UDP `sendto` / TCP under `wmu`) — they never
  touch the registry.

### Subscriber client — subscribe / receive / watchdog / shutdown

- **NewClient**: registers `mux.Register(TypeAudio, c.onUDP)` and
  `mux.Register(TypeFEC, c.onUDP)`. The single handler routes by `pkt[1]` and by
  `from` to the active subscription (drops if none, or if `from != sub.addr`).

- **Subscribe(addr, gen, t)**:
  1. Lock `c.mu`. If an existing `sub` matches `(addr,gen,t)`, unlock, return
     (idempotent). Otherwise tear it down (`sub.shutdown()` → BYE, stop loops).
  2. Build a fresh `subscription{addr, gen, t, …}` with `window.reset(gen)` and
     `fecwin.reset(gen)`. Store as `c.sub`. Unlock.
  3. UDP: `sub.helloUDP(TypeHello, primeMe=true)`; start keepalive (5 s ticker →
     keepalive HELLO) and watchdog goroutines. TCP: `sub.dialTCP()` (initial HELLO
     on the conn) — on dial error, return it; start `readTCP` + keepalive +
     watchdog.

- **UDP receive** (`onUDP`, on the S mux read goroutine — must not block):
  1. `DecodeFrame(pkt)`; on err `malformed++`, return.
  2. Read `c.sub` (atomic-ish under a short `c.mu` RLock or an `atomic.Pointer`).
     If nil or `from != sub.addr`, drop.
  3. `TypeAudio` → `sub.ingest(h, payload, real=true)`.
     `TypeFEC` → `fecParity++`; under `sub.mu`, `fecwin.observeParity(...)`; on a
     recovered frame, `recovered++`, feed `ingest(..., real=false)`.

- **`subscription.ingest(h, payload, real)`** (takes `sub.mu`):
  1. Gen gate: `h.Gen < sub.gen` → `staleGen++`, return. (`h.Gen > sub.gen` should
     not happen — Subscribe re-creates the subscription per gen — but if it does,
     `window.reset` / `fecwin.reset` to the higher gen, defensively.)
  2. `sub.lastRecv = nowLocal()` (watchdog feed) on any accepted frame.
  3. If `real`: `fecwin.observeData(...)`; a completed block recovers one frame →
     feed it back through `ingest(real=false)` (single hole per block ⇒ bounded).
  4. `window.admit(...)` → `(deliver, lost, dup, stale)`; update counters; for
     each `frameRec`, build a `Header` and call `c.deliver(h, payload)`,
     `delivered++`. Delivery happens under `sub.mu` (serialized; Sink.Push is
     non-blocking per S — acceptable, matches the previous round).

- **TCP receive** (`readTCP`, one goroutine): `uint32-BE len | chunk`;
  `DecodeFrame`; `TypeAudio` → `ingest(real=true)` (TCP ⇒ no FEC, always real);
  `TypeReconfig` → see "RECONFIG handling". EOF/error → close, the watchdog/H
  re-subscribe.

- **Keepalive** (5 s ticker): UDP → `helloUDP(TypeHello, primeMe=false)`; TCP →
  write a HELLO control frame. Stops on `sub.done`.

- **Starvation watchdog** (§8.6) — this is the piece that moved here from the old
  push model. A timer-driven goroutine: if `nowLocal() - sub.lastRecv > 2 s` and
  a frame was ever received:
  - **First trip**: issue **RESTART** (UDP `helloUDP(TypeRestart, primeMe=true)`
    or a TCP RESTART frame), `restarts++`, and reset the deadline. "I got lost,
    re-prime me" — the source re-bursts the ring and resumes.
  - **Second consecutive trip** (still no frames ~2 s after RESTART): the source
    is gone (master died). Log, `Unsubscribe()` locally, and stop. Group
    self-healing (§5) re-points this node and H re-Subscribes to the new master.
  The watchdog does **not** close the sink — E has its own internal disarm; the
  sink re-arms on the next `Reset(gen)` when H re-subscribes.

- **RECONFIG handling**: RECONFIG is `src→sub`. On **TCP** it arrives inline on
  the conn and `readTCP` sees it directly: stop-flag set → treat like EOF (end of
  session; H clears playback). Non-stop → signal H to re-read settings and
  re-Subscribe. On **UDP**, control flows only sub→src on the SOURCE_PORT; the
  source's RECONFIG datagram is delivered to the member's STREAM_PORT mux as a
  `TypeReconfig` packet, so the Client also registers `mux.Register(TypeReconfig,
  c.onReconfig)`. Either way, the Client's response is the same: invoke the
  injected `OnReconfig(stop bool)` callback (wired by H via `ClientConfig`), and H
  decides — re-`Subscribe` under the new gen (re-reading replicated settings,
  D23) or stop. The Client never re-reads cluster state itself (no cluster import).
  *(Correction to §2.5's note: `OnReconfig` IS exported via `ClientConfig`; the
  Client routes both the TCP-inline and UDP-mux RECONFIG to it.)*

- **Shutdown** (`Close`): `Unsubscribe()` (BYE + stop loops + `wg.Wait`). Mux
  handlers stay registered (D12: no Unregister; a nil/stale `c.sub` makes late
  dispatch a no-op). Idempotent via a `closed` guard.

- **Locking**: `Client.mu` guards only the `c.sub` pointer + `closed`. Each
  `subscription` has its own `sub.mu` for `window`/`fecwin`/`lastRecv`/delivery.
  Counters are atomics (read lock-free by `Counters()`). No lock nesting:
  `onUDP` reads the sub pointer briefly, then takes `sub.mu`; Subscribe/Unsubscribe
  take `c.mu`, and call `sub.shutdown()` which takes `sub.mu` — `c.mu` is released
  before `wg.Wait` to avoid blocking the read path. The two mutexes are never held
  simultaneously in a cycle.

### FEC recovery timing (preserved, the subtle part)

A block is `[baseSeq, baseSeq+3]`. One missing data frame is recoverable once
parity + the other three data frames are held. Recovery fires from the **last**
of those four to arrive (the 3rd data if parity came first, or the parity if it
came last); `observeData`/`observeParity` both check `parity != nil && have == 3`
→ reconstruct via `XORInto(recovered, parity)` then `XORInto` each present
payload. The recovered `PTS` is `present.pts ± k·FrameNanos` from a present
neighbor (PTS linear in Seq within a session, §8.2). The recovered frame flows
through `ingest(real=false)` → reorder window → deliver. Double loss in a block
is unrecoverable: those seqs fall out as `Lost` when the window slides (E plays
silence, §8.5).

---

## 4. Edge cases & failure handling (spec-referenced)

- **Master streams to itself over loopback (§8.2, D22)**: the master's own sink
  runs a `Client` like everyone else; its HELLO comes from `127.0.0.1:STREAM_PORT`
  to `127.0.0.1:SOURCE_PORT`, so the registry keys it by the loopback addr and
  fans out to it identically. No "self" branch anywhere.

- **Observed-by-construction return path (§3.1/§8.7, D22)**: the source NEVER
  resolves an address. UDP subscribers HELLO **from their STREAM_PORT mux socket**
  and the source replies to that exact `from`; TCP subscribers' audio rides the
  accepted conn. No `Resolver`, no `DialCandidates` on the source side. (The
  *subscriber* resolves the master's SOURCE_PORT via H's `cluster.DialCandidates(
  master)` upstream — G dials exactly the `addr` it is handed.)

- **Burst prime cutoff (§8.2/D24)**: `ring.prime` keeps only frames whose
  deadline `pts + bufferMs` is still future (the most recent `bufferMs` of audio);
  older ring frames are skipped — a newcomer can't play already-past audio. UDP
  prime paced ~4× realtime (one frame/~5 ms) so it outruns live without flooding;
  TCP back-to-back (flow control paces). Primes counted (D28).

- **Prime when idle / paused (§4/§8.2)**: if no session is active, a HELLO is
  registered but **no prime** is sent (ring empty / `!active`). The subscriber
  starts receiving at the next `StartSession`+`ReleaseFrame`. (Pull-paced sources
  release continuously; there is no real "pause" in v1.)

- **Keepalive / expiry (§8.7)**: HELLO every 5 s; a subscriber unseen for 15 s is
  swept and (TCP) its conn closed. A subscriber that missed several keepalives but
  resumes within 15 s simply refreshes `lastSeen`; beyond that it is removed and
  must re-HELLO (prime-me), which its watchdog/H re-Subscribe drives.

- **RESTART re-prime (§8.6)**: a starved subscriber (>2 s no frames) sends
  RESTART; the source re-bursts the live edge of the ring and the subscriber
  resumes mid-session. `Restarts` counted on both ends. If still starved ~2 s
  later, the subscriber gives up (`Unsubscribe`) and group self-heal takes over.

- **Stale generation (§8.4)**: client-side, any frame/parity below the active
  subscription `gen` is dropped (`StaleGen++`) before touching the window. A new
  play / master change creates a **new subscription** (Subscribe with the new
  gen), so old-gen datagrams still in flight are cleanly ignored. Source-side,
  `StartSession` bumps gen and resets seq/FEC so no cross-gen mixing.

- **Live settings change (§8.7/D23)**: master `StartSession(newGen,…)` +
  `Reconfig()`. Subscribers get RECONFIG, H re-reads replicated settings and
  re-`Subscribe`s under the new gen/transport. A transport flip (udp↔tcp)
  tears down the old subscription path and builds the new one; the source's
  `transport` field selects the fan-out path for new releases.

- **UDP single loss (§8.4)**: FEC-recovered; `Recovered++`, `Delivered++`. The
  recovered payload is byte-identical (XOR exact); `XORInto`'s clamp + per-frame
  `PayloadLen` handle the (rare, opus) short-payload case.

- **UDP double loss in a block (§8.4)**: unrecoverable; the two seqs become
  `Lost`, the reorder window slides past them (`maxAhead` bound forces progress),
  E inserts silence. Never blocks.

- **Reorder / duplicate (§8.4)**: out-of-order within `maxAhead` (32) reorders to
  in-order; a frame delivered by FEC then also arriving real (or a UDP dup) is
  dropped at the window (`seq < next` / already-pending) → `Duplicate++`,
  at-most-once preserved.

- **Garbage on the open UDP ports (§8.4, S §4)**: both the SOURCE_PORT UDP socket
  (source) and the member mux (client) get internet/garbage datagrams.
  `DecodeFrame` returns `ErrShort`/`ErrBadMagic` → counted `Malformed`/dropped; an
  unknown control type on SOURCE_PORT is ignored. No panic on hostile input.

- **Slow / wedged TCP subscriber (§8.4, D13)**: a TCP subscriber whose receive
  window is full would block the source's `conn.Write` under `Server.mu`,
  stalling H's release ticker. Mitigation: source TCP writes use a short write
  deadline (`SetWriteDeadline(now+50ms)`); a timeout marks the conn dead
  (`reg.remove` on its reader's next error, conn closed), dropping that
  subscriber rather than the whole group. Accepted v1 limitation; UDP (the
  default) has no such risk (`sendto` never blocks).

- **TCP subscriber reconnect (§8.4)**: if a TCP subscriber's conn drops, its
  `readTCP` errors → the client tears down and H re-`Subscribe`s (re-dial), which
  re-HELLOs and re-primes from the ring. The gap is `Lost` (silence) exactly as
  the spec wants; no master-side reconnect logic (the subscriber drives, D22).

- **Empty subscriber set**: `ReleaseFrame` with zero subscribers still advances
  seq/gen/ring/FEC bookkeeping (so a subscriber that HELLOs next frame primes and
  joins a consistent stream) but writes nowhere. No error.

- **Source faster/slower than 20 ms**: G imposes no rate; it fans out exactly when
  `ReleaseFrame` is called. Seq is server-stamped, pts comes from H, FEC cadence
  is per-4-frames, independent of wall time.

- **Payload length**: pcm payloads are always `FrameBytes`; opus payloads vary. G
  never assumes `FrameBytes` for the wire — it uses `Header.PayloadLen`. Only the
  FEC parity buffer is sized `FrameBytes` (the max) and `XORInto` zero-pads
  shorter payloads, matching the spec's "padded" parity (§8.4).

- **Close vs in-flight callbacks (D12)**: a `closed` guard makes a late mux
  dispatch after `Close` a no-op; K stops the Mux in shutdown order, so this is
  belt-and-suspenders. No `Mux.Unregister` (D12). The source's read/accept/sweeper
  goroutines exit on `done`; tracked TCP conns are closed.

---

## 5. Test plan (all loopback / in-process, no hardware, no root)

UDP tests build two `*Mux`/raw `*net.UDPConn` instances on `127.0.0.1:0`; loss is
injected by a drop-filtering wrapper around `WriteTo`. TCP tests use a real
`net.TCPListener` on `127.0.0.1:0`. Fakes: a `fakeDeliver` collecting
`(Header, payload-copy)`, a manual time source for keepalive/expiry/watchdog.

### `internal/stream/fec_test.go`
- `TestFECBlockParityXOR` — fold 4 known payloads; parity == XOR of all four.
- `TestFECRecoverMissingFromParity` — drop one of four; parity+3 reconstructs the
  exact missing payload + PTS.
- `TestFECRecoverWhenParityArrivesLast` / `…DataArrivesLast` — both completion
  orders trigger recovery.
- `TestFECDoubleLossUnrecoverable` — drop two of four → no recovery.
- `TestFECShortPayloadPadding` — mixed/short lengths zero-pad; recovered payload
  trimmed to its `PayloadLen`.
- `TestFECPartialFlush` — `flushPartial` on a 2-frame tail emits usable parity.
- `TestFECResetOnGen` — `reset` drops old-gen blocks; new gen clean.

### `internal/stream/recvwindow_test.go`
- `TestWindowInOrderDelivery`, `TestWindowReordersWithinWindow`,
  `TestWindowGapBecomesLost`, `TestWindowDuplicateDropped`,
  `TestWindowOverflowEvicts`, `TestWindowResetReanchors`,
  `TestWindowFirstFrameAnchors` (joins mid-stream: first admitted seq anchors).

### `internal/stream/client_test.go`
- `TestClientUDPDeliver` — a fake source `WriteTo`s 4 audio frames to the client's
  mux; delivered in order; `Delivered==4`.
- `TestClientFECRecovery` — source emits 4+parity, drop the 2nd data via the
  filter → all 4 delivered; `Recovered==1`, `Lost==0`.
- `TestClientDoubleLossSilence` — drop 2 of 4 → 2 delivered, `Lost==2`.
- `TestClientStaleGenDropped` — active sub gen=5; a gen=4 frame → `StaleGen++`,
  not delivered.
- `TestClientReorderThenDeliver` — out-of-order UDP delivered ordered.
- `TestClientDuplicateCounted` — same datagram twice → one delivery, `Duplicate==1`.
- `TestClientTCPDeliver` — TCP subscription against a loopback listener that
  writes 3 length-prefixed frames → all delivered.
- `TestClientHelloFromMuxSocket` — UDP HELLO's source addr == the client's mux
  `LocalAddr()` (the observed-return-path invariant, D22).
- `TestClientKeepaliveCadence` — manual clock: a HELLO emitted every 5 s.
- `TestClientWatchdogRestart` — feed frames, then stall: after 2 s the client
  emits a RESTART (prime-me) control to the source addr; `Restarts==1`.
- `TestClientWatchdogGivesUp` — still no frames after RESTART → `Unsubscribe`
  (BYE sent, loops stopped, no goroutine leak).
- `TestClientReconfigStop` — a RECONFIG(stop) over TCP / via mux → `OnReconfig(
  true)` invoked once.
- `TestClientResubscribeNewGen` — Subscribe(gen=2) after gen=1 tears down the old
  sub (BYE) and resets the window; gen=1 frames then `StaleGen`.
- `TestClientMalformedDropped` — too-short / bad-magic datagram → `Malformed++`.
- `TestClientClose` — Close → BYE, loops stopped, idempotent, no leak.

### `internal/source/ring_test.go`
- `TestRingPushWrap` — push > capacity → oldest overwritten, `count==cap`.
- `TestRingPrimeDeadlineCutoff` — frames older than the live edge minus bufferMs
  are excluded; recent ones included, oldest→newest.
- `TestRingResizeClears` — `resize(newBufferMs)` reallocs to
  `max(2*bufferMs,1000)ms` of frames and empties.
- `TestRingPrimeEmpty` — prime on an empty/just-resized ring → nil.

### `internal/source/registry_test.go`
- `TestRegistryUpsertNewVsKeepalive` — first HELLO → isNew; repeat → refresh only.
- `TestRegistryExpire` — a sub unseen > 15 s is removed; its TCP conn returned for
  close; a refreshed sub survives.
- `TestRegistryRemoveBye` — BYE removes the sub.
- `TestRegistryTransportRouting` — UDP sub has nil conn keyed by addr; TCP sub
  carries its conn.

### `internal/source/prime_test.go`
- `TestPrimeUDPPacing` — manual clock: N frames sent ~5 ms apart (≈4× realtime);
  all carry their original Seq/PTS/Gen and no FEC.
- `TestPrimeTCPBackToBack` — frames written length-prefixed, contiguous, in order.
- `TestPrimeCountsStat` — a completed prime increments `Primes`.

### `internal/source/server_test.go`
- `TestServerSubscribeUDPPrimeThenLive` — release 10 frames; a UDP client HELLOs
  (prime-me) at frame 5 → receives the primed live-edge frames then live frames;
  `Connects==1`, `Primes==1`.
- `TestServerSubscribeTCP` — TCP subscriber dials, HELLOs, gets length-prefixed
  prime+live frames in order.
- `TestServerFanoutAllSubscribers` — two UDP subs; one `ReleaseFrame` arrives at
  both with identical header+payload.
- `TestServerFECCadence` — 4 `ReleaseFrame`s → exactly one TypeFEC datagram per
  UDP sub after the 4th; none after 1–3.
- `TestServerKeepaliveExpiry` — manual clock: no HELLO for 15 s → sub expired,
  `Clients` drops; fan-out skips it.
- `TestServerRestartReprimes` — a subscribed UDP client sends RESTART → re-prime
  burst received, `Restarts==1`.
- `TestServerReconfigBroadcast` — `Reconfig()` → every sub gets a TypeReconfig
  (non-stop) packet.
- `TestServerStopSessionFlushesAndNotifies` — release 6 frames (1 partial FEC
  block of 2), `StopSession` → a tail parity datagram is sent AND a RECONFIG with
  the stop flag; ring cleared; `active=false`.
- `TestServerStatsSurface` — `Stats()` reflects Clients/Connects/Restarts/Primes.
- `TestServerReleaseNoSession` — `ReleaseFrame` before `StartSession` → 0, no send.
- `TestServerClose` — Close stops all goroutines (no leak), closes TCP conns,
  leaves SOURCE_PORT sockets open (K owns them), idempotent.

### End-to-end within the two packages (source ↔ client, no other pieces)
- `TestRoundTripUDPLossy` — `source.Server` (UDP) → `stream.Client` through a
  loopback UDP pair with a deterministic 1-in-5 drop filter; client HELLOs, gets
  primed, streams 100 frames; assert every in-block single loss recovered, the
  delivered seq run contiguous where recoverable, `Lost` only on injected double
  losses (none here → 0).
- `TestRoundTripTCPClean` — Server(TCP) → Client(TCP); 100 frames; all delivered
  in order, `Lost==0`, `Recovered==0`, `FECParity==0`.
- `TestRoundTripLateJoinPrimed` — client subscribes at frame 50 → receives the
  primed live-edge (most recent `bufferMs`) then live, no gap at the join seam.
- `TestRoundTripRestartRecovers` — drop all frames to the client for 2.5 s → the
  client's watchdog issues RESTART → source re-primes → delivery resumes.

All tests run on loopback sockets, in-process fakes, generated bytes, and a manual
clock for the timed paths. No multicast, no audio device, no root.
