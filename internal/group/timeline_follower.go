package group

// FollowerTimeline is the follower-side projection of the master's timeline,
// adapted from media internal/sync.BeaconTimeline (the pattern source) but with
// beacons/publisher dropped: the master uses MasterTimeline directly and the
// per-chunk header (doc 04 §4.4.2/§4.4.3) carries the anchor, so there is no
// 10 Hz beacon. The follower projects via derivation A (doc 04 §4.4.3, A.2):
//
//	master_now = clock.NowMono() + Offset
//	NowSample  = chunk.SampleIndex + round((master_now - chunk.MasterMono)*rate/1e9)
//
// ok is false until both a clock offset exists AND a chunk for the current
// streamGen has been received; a streamGen mismatch is treated as not-synced
// until a current-generation chunk arrives (A.2 — handles media change/seek). A
// paused master carries Playing=false on its chunks, freezing NowSample.

import (
	"sync/atomic"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/clock"
)

// ChunkMeta is the newest received chunk's anchor (README §6.4 header fields),
// pushed by the stream receiver via ChunkMetaSource.
type ChunkMeta struct {
	SampleIndex int64  // first canonical-rate frame on the group timeline
	MasterMono  int64  // master monotonic ns when the chunk was sourced
	StreamGen   uint64 // group stream generation
	Playing     bool   // carried from master transport state
}

// ChunkMetaSource is supplied by the stream receiver (stream/sink_net). It is an
// interface so internal/group compiles and is unit-testable without stream/*.
type ChunkMetaSource interface {
	LatestChunkMeta() (ChunkMeta, bool)
}

// FollowerTimeline projects the master timeline to "now". It implements Timeline.
type FollowerTimeline struct {
	chunks ChunkMetaSource
	clk    ClockSource
	rate   int

	// gen is the current stream generation gate; a chunk whose StreamGen differs
	// is treated as not-synced (A.2). Atomic so SetStreamGen is concurrency-safe
	// with NowSample readers on the render path.
	gen atomic.Uint64

	// nowMono is the local monotonic timebase; injectable for deterministic tests
	// (defaults to clock.NowMono).
	nowMono func() int64
}

// NewFollowerTimeline wires a follower projection over a chunk source and clock
// source at the canonical rate (Hz). A non-positive rate falls back to A.12's
// default 48000.
func NewFollowerTimeline(chunks ChunkMetaSource, clk ClockSource, rate int) *FollowerTimeline {
	if rate <= 0 {
		rate = defaultRate
	}
	return &FollowerTimeline{chunks: chunks, clk: clk, rate: rate, nowMono: clock.NowMono}
}

// SetStreamGen sets the current generation gate (doc 04 §4.4.3). Until a chunk
// of this generation arrives, NowSample reports ok=false.
func (f *FollowerTimeline) SetStreamGen(gen uint64) { f.gen.Store(gen) }

// Rate returns the canonical sample rate in Hz.
func (f *FollowerTimeline) Rate() int { return f.rate }

// NowSample projects the master timeline to the follower's "now" (doc 04 §4.4.3
// derivation A, A.2). ok=false unless the clock offset is available AND the
// newest chunk matches the current streamGen.
func (f *FollowerTimeline) NowSample() (int64, bool, bool) {
	off, ok := f.clk.Offset()
	if !ok {
		return 0, false, false
	}
	chunk, ok := f.chunks.LatestChunkMeta()
	if !ok {
		return 0, false, false
	}
	if chunk.StreamGen != f.gen.Load() {
		return 0, false, false // generation mismatch ⇒ not synced (A.2)
	}
	if !chunk.Playing {
		return chunk.SampleIndex, false, true // paused master ⇒ freeze
	}
	masterNow := f.nowMono() + off.Nanoseconds()
	sample := chunk.SampleIndex + projectSamples(masterNow-chunk.MasterMono, f.rate)
	return sample, true, true
}
