package origin

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/clock"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/sink_net"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/source"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/streamgen"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// Timeline is the per-group synchronized stream sample index this origin consumes
// (README §6.2 / 05 §6.2). It is the read-side projection contract; the concrete
// MasterTimeline lives in internal/group (which imports this package, not the
// reverse — 01 §2 layering), so the interface is declared here at the consumer to
// keep stream/* a leaf. *group.MasterTimeline satisfies it structurally.
type Timeline interface {
	// NowSample returns the canonical-rate frame index due at "now", the playing
	// flag, and ok (always true on the master — it is the reference, 05 §6.2 / A.2).
	NowSample() (sample int64, playing bool, ok bool)
}

// Config carries the per-group, per-run parameters (all from A.12 / the group
// profile). Zero/invalid fields fall back to the A.12 canonical values.
type Config struct {
	Rate           int           // canonical rate, 48000 (A.12)
	Channels       int           // 2 (A.12)
	FramesPerChunk int           // 480, 10 ms (A.12)
	Lead           time.Duration // buffer playout lead, 300 ms (A.12 LeadMs)
	StreamGen      uint64        // starting generation (set by the group engine on (re)start)

	// Transport selects the wire transport (05 §5.9, D2). Default TransportUDP
	// (UDP unicast + FEC); TransportTCP forces fec.None and a length-prefixed
	// stream per listener. Mirrors GroupRecord.Profile.Transport.
	Transport sink_net.Transport
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
	if c.Lead <= 0 {
		c.Lead = 300 * time.Millisecond
	}
	return c
}

// Origin is the master-side stream origin (05 §5.2). One per playing group while
// this node is master: it turns one looping source.Reader PCM stream into
// timestamped, FEC-protected wire packets paced out, one unicast UDP send per
// registered listener, ahead of each chunk's playout instant by the buffer lead.
//
// Not safe for concurrent Run. AddListener/RemoveListener/ResumeAt are safe to
// call concurrently with Run.
type Origin struct {
	tl    Timeline      // §6.2 NowSample — group playout timeline (P3.1/group)
	codec codec.Codec   // §6.3 — negotiated for the group (P4.1)
	fec   fec.FEC       // §6.3 — negotiated for the group (P4.2)
	src   source.Reader // §5.3 — source decode→PCM, looped (P4.4)

	cfg     Config
	chunk   *chunker
	sender  fanOutSender
	codecID wire.CodecID
	fecID   wire.FECID
	rate100 uint16

	// loopback, when set, receives every produced chunk's interleaved PCM before
	// encode — the master's LOCAL render tee (a solo/master node hears its own
	// stream without a UDP self-loop; doc 01 §4.2 D17 loopback render). The
	// callee must copy (the buffer is reused); ring.Ring.Write does. Set via
	// WithLoopback before Run.
	loopback func(pcm []float32)

	// nowMono stamps the master monotonic instant at source time (05 §5.2.2) and is
	// the pacer timebase (the master is the reference, so no offset). Injectable for
	// deterministic tests; defaults to clock.NowMono.
	nowMono func() int64
	// sleepUntil parks the Run loop until a master-mono deadline. Injectable for
	// deterministic tests; defaults to a real time.Timer wait.
	sleepUntil func(ctx context.Context, deadlineMono int64) error

	// gen is the live generation the Run loop stamps on chunks. It is atomic so a
	// concurrent StreamGen status read observes a bump immediately; the authoritative
	// counter is ctrl (mutated under mu by Bump/ResumeAt). seq/idx are owned by Run. A
	// pending bump is latched and applied at the top of the next chunk so a generation
	// change is atomic w.r.t. a chunk boundary (05 §5.8).
	gen atomic.Uint64

	mu      sync.Mutex
	ctrl    *streamgen.Controller // master-side streamGen state machine (P5.3)
	pending *streamgen.Generation // latched bump directives, applied by Run; nil when none
}

// New constructs an Origin over the negotiated codec/FEC, the group timeline, and
// the looping source. cfg is normalized to the A.12 canonical values for any
// zero/invalid field.
func New(tl Timeline, c codec.Codec, f fec.FEC, src source.Reader, cfg Config) *Origin {
	cfg = cfg.withDefaults()

	// TCP fallback forces FEC None (05 §5.9): TCP retransmission already guarantees
	// delivery, so XOR/dup parity is pure waste and Protect must be identity. We
	// swap to a fresh fec.None regardless of the negotiated profile; the wire FECID
	// stamped on the header follows (FECNone=0).
	var snd fanOutSender = newSender()
	if cfg.Transport == sink_net.TransportTCP {
		f = fec.NewNone()
		snd = tcpSenderAdapter{newTCPSender()}
	}

	o := &Origin{
		tl:         tl,
		codec:      c,
		fec:        f,
		src:        src,
		cfg:        cfg,
		chunk:      newChunker(src, cfg.FramesPerChunk, cfg.Channels),
		sender:     snd,
		codecID:    wire.CodecID(c.ID()),
		fecID:      wire.FECID(f.ID()),
		rate100:    uint16(cfg.Rate / 100),
		nowMono:    clock.NowMono,
		sleepUntil: sleepUntilMono,
		ctrl:       streamgen.NewController(cfg.StreamGen),
	}
	o.gen.Store(cfg.StreamGen)
	return o
}

// WithLoopback registers the local PCM tee fn (the master's own render path).
// fn is called from the Run goroutine once per produced chunk with the
// interleaved samples; it must copy them (the chunk buffer is reused). Returns
// the origin for chaining. Must be set before Run.
func (o *Origin) WithLoopback(fn func(pcm []float32)) *Origin {
	o.loopback = fn
	return o
}

// AddListener registers a unicast destination. On a join into a playing group the
// origin forces a keyframe on the next chunk for that generation (05 §5.6.4) — for
// PCM every chunk is already a keyframe, so the flag is just set uniformly.
// Idempotent by id: re-adding a known id is a no-op.
func (o *Origin) AddListener(id string, addr *net.UDPAddr) error {
	added, err := o.sender.add(id, addr)
	if err != nil {
		return err
	}
	if added {
		o.onLateJoin(id)
	}
	return err
}

// RemoveListener drops a destination and closes its socket. Idempotent.
func (o *Origin) RemoveListener(id string) error {
	o.sender.remove(id)
	return nil
}

// Listeners reports the current registered listener count (status snapshot for the
// group engine / web.Deps).
func (o *Origin) Listeners() int { return o.sender.count() }

// StreamGen reports the current generation (status snapshot).
func (o *Origin) StreamGen() uint64 { return o.gen.Load() }

// Run drives decode→chunk→encode→wire→FEC.Protect→paced unicast until ctx is
// cancelled or the node loses mastership (the group engine cancels ctx). 05 §5.2.
//
// The master is the reference clock, so each chunk for sampleIndex idx is sent at
// playout(idx)−Lead, which (because the master defines playout) equals the chunk's
// source instant: baseMono + (idx−baseIdx)/rate. Run parks on the pacer's earliest
// due send instant, fans the due bundles out to every listener, then produces the
// next chunk. fec.Protect and codec.Encode run once per chunk (D5); only the
// per-socket Write is O(listeners).
func (o *Origin) Run(ctx context.Context) error {
	defer o.sender.closeAll()

	rate := int64(o.cfg.Rate)
	chunkFrames := int64(o.cfg.FramesPerChunk)

	var p pacer

	// Anchor the timeline: the first chunk's sampleIndex and the master-mono base.
	baseIdx, _, _ := o.tl.NowSample()
	baseMono := o.nowMono()
	idx := baseIdx
	var seq uint64
	// curGen is the live generation the Run loop stamps on chunks. It is advanced
	// ONLY when a latched bump is applied at a chunk boundary (below) — never ahead
	// of the seq/idx reset — so the first chunk of a new generation always carries
	// gen+seq0+re-anchored idx atomically (05 §5.8). o.gen mirrors it for status.
	curGen := o.gen.Load()

	// produce builds, protects, and schedules the next chunk; returns false on a
	// hard source error (Run then returns it).
	produce := func() error {
		// Apply a latched generation bump at the chunk boundary (05 §5.8 steps
		// 1–4): re-anchor sampleIndex (step 3), restart seq at 0 + reset FEC parity
		// state (step 2). The keyframe (step 4) is forced via the per-listener
		// keyframe arm done by Bump/ResumeAt and is set on the header below.
		if g, ok := o.takePending(); ok {
			curGen = g.Gen
			if g.ResetSeq {
				seq = 0
			}
			idx = g.FirstSampleIndex
			baseIdx = g.FirstSampleIndex
			baseMono = o.nowMono()
			p.reset()
			if g.ResetFEC {
				o.fec = resetFEC(o.fec) // fresh parity state for the new generation
			}
			o.gen.Store(curGen) // publish the now-live gen for status reads
		}

		pcm, err := o.chunk.next()
		if err != nil {
			return err
		}
		if o.loopback != nil {
			o.loopback(pcm) // local render tee (solo/master hears its own stream)
		}
		payload, err := o.codec.Encode(pcm)
		if err != nil {
			return err
		}

		flags := keyframeFlag(o.codecID, o.sender.needKeyframe())

		hdr := wire.Header{
			Flags:       flags,
			CodecID:     o.codecID,
			FECID:       o.fecID,
			StreamGen:   curGen,
			Seq:         seq,
			SampleIndex: idx,
			MasterMono:  o.nowMono(), // master mono at SOURCE time (05 §5.2.2)
			Rate100:     o.rate100,
		}
		pkt, err := wire.Marshal(hdr, payload)
		if err != nil {
			return err
		}
		out := o.fec.Protect(seq, pkt) // [source, repair…] — once per chunk (D5)

		// Send instant: playout(idx) − Lead. Because the master IS the reference,
		// playout(idx) = sourceInstant(idx) + Lead with sourceInstant(idx) =
		// baseMono + (idx−baseIdx)/rate·1e9, so the Lead cancels and sendAt is the
		// source instant — the 10 ms-per-chunk cadence (05 §5.2.2 / §5.6.1).
		sendAt := baseMono + (idx-baseIdx)*int64(time.Second)/rate
		// The keyframe forced for a join/gen-change is satisfied by this chunk.
		o.sender.clearKeyframe()
		p.push(sendAt, out)

		seq++
		idx += chunkFrames
		return nil
	}

	// Prime one chunk so the heap is non-empty.
	if err := produce(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		sendAt, ok := p.peek()
		if !ok {
			if err := produce(); err != nil {
				return err
			}
			continue
		}
		if err := o.sleepUntil(ctx, sendAt); err != nil {
			return err
		}
		// Fan out every bundle now due (sendAt <= now).
		now := o.nowMono()
		for {
			next, ok := p.peek()
			if !ok || next > now {
				break
			}
			b := p.pop()
			o.sender.fanOut(b.packets)
		}
		// Keep the heap ~one chunk ahead so the next park has something to wait on.
		if p.len() == 0 {
			if err := produce(); err != nil {
				return err
			}
		}
	}
}

// keyframeFlag computes the header flags for one chunk. PCM is always a keyframe
// (every chunk is independent, 05 §5.4.1); inter-frame codecs set the keyframe flag
// only when a join / generation change has armed it (05 §5.6.4 / §5.8).
func keyframeFlag(codecID wire.CodecID, forced bool) wire.Flags {
	if codecID == wire.CodecPCM || forced {
		return wire.FlagKeyframe
	}
	return 0
}

// sleepUntilMono parks until the master-mono deadline (ns) or ctx cancellation.
func sleepUntilMono(ctx context.Context, deadlineMono int64) error {
	d := time.Duration(deadlineMono - clock.NowMono())
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
