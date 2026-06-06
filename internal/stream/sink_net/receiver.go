package sink_net

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/streamgen"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// Config mirrors origin.Config's relevant fields (from the group profile / A.12).
// Zero/invalid fields fall back to the A.12 canonical values.
type Config struct {
	Rate           int // 48000
	Channels       int // 2
	FramesPerChunk int // 480
	WindowPackets  int // reorder/dedupe window size, default 32 (05 §5.6.2)
	LeadMs         int // buffer playout lead before playout, default 300 (A.12 LeadMs)

	// Transport selects the wire transport (05 §5.9, D2). Default TransportUDP;
	// TransportTCP runs the same pipeline minus FEC and minus UDP reordering (TCP
	// delivers in order) over a length-prefixed stream. Mirrors origin.Config.Transport.
	Transport Transport
}

func (c Config) withDefaults() Config {
	if c.Rate <= 0 {
		c.Rate = 48000
	}
	if c.Channels <= 0 {
		c.Channels = 2
	}
	if c.FramesPerChunk <= 0 {
		c.FramesPerChunk = 480
	}
	if c.WindowPackets <= 0 {
		c.WindowPackets = 32
	}
	if c.LeadMs <= 0 {
		c.LeadMs = 300
	}
	return c
}

// pusher is the index-addressed ring hand-off (05 §5.6.2 step c). P3.1's *ring.Ring
// is a sequential SPSC buffer with no PushAt method (risk R1), so the receiver
// targets this minimal interface and wraps the concrete ring in ringPusher: the
// receiver releases chunks strictly in seq order and gap-fills with concealment, so
// a contiguous sequential Write preserves the sampleIndex alignment the cardinal
// rule requires (05 §5.6.3). When P3.1 grows a true PushAt, ringPusher is the only
// shim that changes.
type pusher interface {
	PushAt(sampleIndex int64, pcm []float32)
}

// ringPusher adapts *ring.Ring to pusher via sequential Write. sampleIndex is
// carried for the late-drop decision and for telemetry; the ordered, gap-filled
// stream from the window means the write position already corresponds to
// sampleIndex.
type ringPusher struct {
	ring *ring.Ring
	// playCursor is the sampleIndex of the next sample the ring expects, advanced
	// by FramesPerChunk per push. A chunk whose sampleIndex is behind the cursor is
	// dropped (late, 05 §5.6.3) — the ring would otherwise rewind audio.
	playCursor  int64
	haveCursor  bool
	chunkFrames int64
}

// PushAt writes one chunk at sampleIndex. A chunk behind the play cursor is dropped
// (late). On the first push the cursor is seeded from sampleIndex.
func (rp *ringPusher) PushAt(sampleIndex int64, pcm []float32) {
	if !rp.haveCursor {
		rp.playCursor = sampleIndex
		rp.haveCursor = true
	}
	if sampleIndex < rp.playCursor {
		return // late: never push behind the play cursor (05 §5.6.3)
	}
	// Spin a bounded retry: Write returns a short count when the ring is full; the
	// render consumer drains it. In steady state the ring sits ~LeadMs full with
	// headroom, so this rarely blocks. We advance the cursor by what we wrote.
	for len(pcm) > 0 {
		n := rp.ring.Write(pcm)
		if n == 0 {
			break // ring full and not draining (no consumer in tests) — drop the tail
		}
		pcm = pcm[n:]
	}
	rp.playCursor = sampleIndex + rp.chunkFrames
}

// Receiver is the follower-side stream receiver (05 §5.6.2). One per playing group
// while this node is a follower. Run is single-goroutine; FlushAndReprime is safe
// to call only when Run is not concurrently mutating state (the group engine
// sequences (re)start through the role applier, A.14.4).
type Receiver struct {
	codec codec.Codec    // §6.3 (P4.1)
	fec   fec.FEC        // §6.3 (P4.2)
	ring  *ring.Ring     // jitter buffer, pushed by sampleIndex (P3.1)
	allow *allowlist.Set // §6.5 / 03 / P2.4 — source-IP gate

	cfg         Config
	chunkFrames int64
	win         *window
	push        pusher

	// gate is the receiver-side generation gate (P5.3 / 05 §5.8): a higher gen
	// flushes+adopts, a lower gen is a stale straggler that is dropped, an equal gen
	// passes. The accepted gen is gate.Current() (fed to the FollowerTimeline).
	gate *streamgen.Gate

	// Prime gate (05 §5.6.4 / §5.7): on a (re)prime — late join, role (re)start, or
	// a generation adopt — playout is withheld (LatestChunkMeta ok=false, which
	// makes the FollowerTimeline report ok=false so render holds) until one buffer
	// lead (cfg.LeadMs) of keyframe-anchored audio has accumulated. awaitKeyframe
	// gates keyframe-first decode: after a (re)prime, chunks before the first
	// keyframe of the (new) generation are not pushed (PCM is always a keyframe, so
	// this only bites inter-frame codecs).
	primed        bool  // true once primeFrames >= primeTarget; gates LatestChunkMeta ok
	awaitKeyframe bool  // true until the first keyframe of the current (re)primed gen lands
	primeFrames   int64 // frames pushed since the last (re)prime
	primeTarget   int64 // LeadMs in frames (cfg.LeadMs/1000 * Rate)

	// metaMu guards the timeline-facing snapshot (latest/haveChunk/primed/genSnap)
	// because the FollowerTimeline projection reads it from the render goroutine
	// while the recv loop writes it (05 §6.2). It is taken only on the ~chunk-rate
	// recv path and the ~100 Hz render read, never in the audio hot path, so it
	// costs nothing measurable on the Pi.
	metaMu sync.Mutex

	// latest is the newest accepted chunk's anchor, exposed via LatestChunkMeta for
	// the group's FollowerTimeline projection (it supplies ChunkMetaSource).
	latest    chunkMeta
	haveChunk bool
	genSnap   uint64 // accepted gen mirror for the StreamGen() status read

	// readBuf is the reusable recv buffer (allocation-free recv hot path).
	readBuf []byte
}

// chunkMeta is the newest received chunk's anchor (README §6.4 header fields). It
// mirrors group.ChunkMeta field-for-field; the P4.9 wiring adapts it to the
// group.ChunkMetaSource interface (sink_net must not import group, 01 §2).
type chunkMeta struct {
	SampleIndex int64
	MasterMono  int64
	StreamGen   uint64
	Playing     bool
}

// maxDatagram bounds one recv: the 44-byte header + a full PCM chunk (1920 B at the
// canonical profile) + slack. UDP datagrams never exceed this in this protocol.
const maxDatagram = 2048

// New constructs a Receiver over the negotiated codec/FEC, the jitter ring, and the
// source-IP allowlist. cfg is normalized to the A.12 canonical values.
func New(c codec.Codec, f fec.FEC, r *ring.Ring, allow *allowlist.Set, cfg Config) *Receiver {
	cfg = cfg.withDefaults()
	chunkFrames := int64(cfg.FramesPerChunk)
	rp := &ringPusher{ring: r, chunkFrames: chunkFrames}
	return &Receiver{
		codec:         c,
		fec:           f,
		ring:          r,
		allow:         allow,
		cfg:           cfg,
		chunkFrames:   chunkFrames,
		win:           newWindow(cfg.WindowPackets),
		push:          rp,
		gate:          streamgen.NewGate(),
		awaitKeyframe: true, // a fresh receiver primes from its first keyframe
		primeTarget:   int64(cfg.LeadMs) * int64(cfg.Rate) / 1000,
		readBuf:       make([]byte, maxDatagram),
	}
}

// LatestChunkMeta returns the newest accepted chunk's anchor and whether playout
// is enabled. It is the ChunkMetaSource the group's FollowerTimeline consumes (05
// §6.2 / A.2); the receiver never computes the sample index, it reads it from the
// header (05 §5.1). ok is false until BOTH a chunk has been accepted AND the prime
// lead is filled (05 §5.6.4): the prime gate withholds playout through the
// FollowerTimeline ok=false until one buffer lead of keyframe-anchored audio has
// accumulated, so render holds (orphan/silence) rather than playing a half-empty
// buffer on a late join / (re)prime.
func (r *Receiver) LatestChunkMeta() (sampleIndex, masterMono int64, gen uint64, playing, ok bool) {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	m := r.latest
	return m.SampleIndex, m.MasterMono, m.StreamGen, m.Playing, r.haveChunk && r.primed
}

// StreamGen reports the currently accepted generation (status snapshot). It reads
// the mutex-guarded mirror rather than the gate (which is owned by the single recv
// goroutine) so it is safe to call from a status/render goroutine.
func (r *Receiver) StreamGen() uint64 {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	return r.genSnap
}

// Run reads datagrams from conn and drives allowlist→unwrap→Recover→reorder/dedupe→
// decode→ring.PushAt until ctx is cancelled (05 §5.6.2). conn is bound by the group
// engine (the audio UDP socket, :9100). Read deadlines let the loop observe ctx
// cancellation without a blocking read holding it open.
func (r *Receiver) Run(ctx context.Context, conn *net.UDPConn) error {
	// Unblock the blocking ReadFromUDPAddrPort on ctx cancellation by closing conn.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, from, err := conn.ReadFromUDPAddrPort(r.readBuf)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			continue // transient recv error: skip this datagram
		}
		r.handle(r.readBuf[:n], from.Addr())
	}
}

// handle runs the per-datagram pipeline (05 §5.6.2). Exposed for the loopback /
// table tests to feed datagrams without a live socket.
func (r *Receiver) handle(buf []byte, src netip.Addr) {
	// 2. allowlist gate — BEFORE unwrap/decode (P2.4 / 03 / A.8).
	if !r.allow.AllowedAddr(src) {
		return // DROP, no reply
	}
	// 3. unwrap + structural validation (magic/version/payloadLen).
	hdr, payload, err := wire.Unmarshal(buf)
	if err != nil {
		return // bad packet — dropped without panic
	}
	// 4. generation gate (05 §5.8) — delegated to streamgen.Gate.
	switch r.gate.Accept(hdr.StreamGen) {
	case streamgen.Adopt:
		// Higher gen: flush window+FEC+stale ring tail and re-enter the prime state
		// (keyframe-first, no playout until the lead refills) BEFORE decoding this
		// (keyframe) packet. The gate has already advanced to hdr.StreamGen; publish
		// the new gen so a concurrent StreamGen() status read observes it.
		r.flushAndReprime()
		r.metaMu.Lock()
		r.genSnap = r.gate.Current()
		r.metaMu.Unlock()
	case streamgen.Drop:
		return // stale prior-generation straggler — DROP
	}

	// Keyframe-first (05 §5.6.4): after a (re)prime, hold non-keyframe chunks until
	// the first keyframe of the (new) generation lands so an inter-frame decoder
	// starts cold. PCM chunks are always keyframes, so this never bites PCM.
	if r.awaitKeyframe {
		if !hdr.Flags.Keyframe() {
			return // pre-keyframe chunk of a freshly-(re)primed gen — not decodable cold
		}
		r.awaitKeyframe = false
	}

	// 5–6. FEC.Recover ingests the packet, returning newly-decodable source packets.
	// The payload is a subslice of buf; Clone before the window retains it.
	recovered := r.fec.Recover(wire.Packet{Header: hdr, Payload: payload})

	// 7. reorder/dedupe → decode → push, in seq order.
	for _, sp := range recovered {
		if sp.Header.Flags.Repair() {
			continue // repair packets are consumed by Recover, never pushed
		}
		released, gaps := r.win.insert(sp.Clone(), r.chunkFrames)
		for _, g := range gaps {
			// Unrecoverable missing chunk: conceal at its sampleIndex (05 §5.6.3). A
			// concealed chunk still advances the prime lead (it keeps the cadence).
			r.push.PushAt(g.sampleIndex, concealChunk(r.cfg.FramesPerChunk, r.cfg.Channels))
			r.advancePrime()
		}
		for _, rp := range released {
			r.deliver(rp)
		}
	}
}

// deliver decodes one released source packet and pushes its PCM into the ring at
// its sampleIndex (05 §5.6.2 step c). A decode failure drops the chunk silently;
// the window has already advanced, so the next gap-slide would conceal it. The
// chunk's anchor is recorded for the FollowerTimeline.
func (r *Receiver) deliver(p wire.Packet) {
	pcm, err := r.codec.Decode(p.Payload)
	if err != nil {
		return
	}
	r.push.PushAt(p.Header.SampleIndex, pcm)
	r.metaMu.Lock()
	r.latest = chunkMeta{
		SampleIndex: p.Header.SampleIndex,
		MasterMono:  p.Header.MasterMono,
		StreamGen:   p.Header.StreamGen,
		Playing:     true, // master transport state; PCM keyframe carries play=true
	}
	r.haveChunk = true
	r.metaMu.Unlock()
	r.advancePrime()
}

// advancePrime accumulates one chunk toward the prime lead and flips primed once
// the lead is filled (05 §5.6.4 / §5.7). Until primed, LatestChunkMeta reports
// ok=false so the FollowerTimeline holds playout. primeFrames/primeTarget are
// owned by the recv goroutine; only the primed flag is read cross-goroutine, so
// the flip is published under metaMu.
func (r *Receiver) advancePrime() {
	if r.primed {
		return
	}
	r.primeFrames += r.chunkFrames
	if r.primeFrames >= r.primeTarget {
		r.metaMu.Lock()
		r.primed = true
		r.metaMu.Unlock()
	}
}

// resetFEC returns a fresh FEC of the same id so a generation change / flush starts
// with clean parity state (05 §5.8). For None this is a fresh stateless value; for
// a test fake whose id is unconstructible, the existing instance is kept.
func resetFEC(f fec.FEC) fec.FEC {
	fresh, err := fec.New(f.ID())
	if err != nil {
		return f
	}
	return fresh
}
