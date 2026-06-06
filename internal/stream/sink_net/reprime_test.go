package sink_net

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/ring"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// buildNonKeyframe marshals a source packet WITHOUT the keyframe flag (an Opus-style
// inter-frame chunk) so keyframe-first gating can be exercised. Payload is still PCM
// bytes (the codec is PCM in tests); only the header flag differs.
func buildNonKeyframe(gen, seq uint64, sampleIndex int64, framesPerChunk, channels int) []byte {
	c := codec.NewPCM(channels)
	pcm := make([]float32, framesPerChunk*channels)
	payload, _ := c.Encode(pcm)
	hdr := wire.Header{
		Flags:       0, // NOT a keyframe
		CodecID:     wire.CodecPCM,
		FECID:       wire.FECNone,
		StreamGen:   gen,
		Seq:         seq,
		SampleIndex: sampleIndex,
		MasterMono:  int64(seq) * 10_000_000,
		Rate100:     480,
	}
	buf, _ := wire.Marshal(hdr, payload)
	return buf
}

// TestFlushDropsRingTail uses the REAL ringPusher (not the capture fake) to assert
// the stale ring tail is dropped on flush (05 §5.6.3 / R11).
func TestFlushDropsRingTail(t *testing.T) {
	rng := ring.NewRing(48000 * tChannels)
	r := New(newPCM(t), newNone(t), rng, allowingSet("127.0.0.1"), canonCfg())
	r.primeTarget = 1
	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.5), loopbackAddr)
	r.handle(buildPacket(0, 1, tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if rng.Len() == 0 {
		t.Fatal("ring empty before flush (expected buffered audio)")
	}
	r.FlushAndReprime()
	if rng.Len() != 0 {
		t.Errorf("ring tail not dropped after flush: Len=%d want 0", rng.Len())
	}
}

// TestAdoptFlushesAndReanchors: gen-7 chunks fill the window/ring, then a gen-8
// keyframe at a NEW sampleIndex ⇒ the gate advances to 8, the window/FEC are
// flushed, and the first post-adopt push is at the gen-8 sampleIndex (the old gen-7
// audio is not shifted onto it). Trailing gen-7 stragglers are dropped.
func TestAdoptFlushesAndReanchors(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.primeTarget = 1 // make playout/anchor accounting deterministic; prime tested separately

	for s := uint64(0); s < 3; s++ {
		r.handle(buildPacket(7, s, int64(s)*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	}
	if r.StreamGen() != 7 {
		t.Fatalf("gen=%d want 7", r.StreamGen())
	}
	pushesGen7 := cap.len()

	// gen-8 keyframe at a far-away sampleIndex (a seek).
	const seek = 480000
	r.handle(buildPacket(8, 0, seek, tFrames, tChannels, 0.25), loopbackAddr)
	if r.StreamGen() != 8 {
		t.Errorf("gen=%d want 8 after adopt", r.StreamGen())
	}
	// The first push after the adopt is at the new sampleIndex (re-anchor, no shift).
	if got := cap.idx[cap.len()-1]; got != seek {
		t.Errorf("first post-adopt push idx=%d want %d (re-anchor)", got, seek)
	}

	// A trailing gen-7 straggler arriving late must be dropped (monotonic gate).
	before := cap.len()
	r.handle(buildPacket(7, 9, 9*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if cap.len() != before {
		t.Errorf("gen-7 straggler pushed after adopt: pushes %d→%d", before, cap.len())
	}
	_ = pushesGen7
}

// TestPrimeGateWithholdsPlayout: a fresh receiver does not enable playout (ok via
// LatestChunkMeta) until LeadMs (300 ms @48k = 14400 frames = 30 chunks) of
// keyframe-anchored audio has accumulated. Below the lead, ok=false; at the lead,
// ok flips true. The cardinal-rule index alignment is preserved throughout.
func TestPrimeGateWithholdsPlayout(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	// canonCfg has LeadMs=0 ⇒ default 300 ms. primeTarget = 300/1000*48000 = 14400.
	if r.primeTarget != 14400 {
		t.Fatalf("primeTarget=%d want 14400 (LeadMs 300 @48k)", r.primeTarget)
	}
	chunksToLead := int(r.primeTarget / r.chunkFrames) // 30

	for s := 0; s < chunksToLead-1; s++ {
		r.handle(buildPacket(0, uint64(s), int64(s)*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
		if _, _, _, _, ok := r.LatestChunkMeta(); ok {
			t.Fatalf("playout enabled after %d chunks (<lead), want ok=false", s+1)
		}
	}
	// The chunk that reaches the lead flips ok=true.
	r.handle(buildPacket(0, uint64(chunksToLead-1), int64(chunksToLead-1)*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if _, _, _, _, ok := r.LatestChunkMeta(); !ok {
		t.Errorf("playout not enabled at the prime lead (%d chunks)", chunksToLead)
	}
	// Index alignment: pushes are sample-aligned from 0 (no shift during prime).
	for i := 0; i < cap.len(); i++ {
		if idx, _ := cap.at(i); idx != int64(i)*tFrames {
			t.Errorf("prime push %d idx=%d want %d", i, idx, int64(i)*tFrames)
		}
	}
}

// TestKeyframeFirstAfterReprime: after FlushAndReprime, non-keyframe chunks arriving
// before the first keyframe of the (new) generation are NOT pushed; the first
// keyframe and everything after it are. PCM is always a keyframe, so this models an
// inter-frame codec via buildNonKeyframe.
func TestKeyframeFirstAfterReprime(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.primeTarget = 1

	r.FlushAndReprime() // enter prime + awaitKeyframe

	// Two non-keyframe chunks (same gen 0) before any keyframe: must be withheld.
	r.handle(buildNonKeyframe(0, 0, 0, tFrames, tChannels), loopbackAddr)
	r.handle(buildNonKeyframe(0, 1, tFrames, tFrames, tChannels), loopbackAddr)
	if cap.len() != 0 {
		t.Fatalf("non-keyframe chunks pushed before first keyframe: %d pushes", cap.len())
	}
	// The first keyframe lands ⇒ it decodes and is pushed; subsequent chunks flow.
	r.handle(buildPacket(0, 2, 2*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if cap.len() != 1 {
		t.Fatalf("keyframe not pushed: %d pushes want 1", cap.len())
	}
	if idx, _ := cap.at(0); idx != 2*tFrames {
		t.Errorf("first post-reprime push idx=%d want %d (keyframe anchor)", idx, 2*tFrames)
	}
}

// TestFlushClearsWindowAndFEC: after a flush, a fresh stream re-primes from a high
// seq without late-dropping (the window frontier was reset) and the ring was reset.
func TestFlushClearsWindowAndFEC(t *testing.T) {
	r, cap := newTestReceiver(t, allowingSet("127.0.0.1"), canonCfg())
	r.primeTarget = 1
	r.handle(buildPacket(0, 0, 0, tFrames, tChannels, 0.5), loopbackAddr)
	r.handle(buildPacket(0, 1, tFrames, tFrames, tChannels, 0.5), loopbackAddr)

	r.FlushAndReprime()
	if _, _, _, _, ok := r.LatestChunkMeta(); ok {
		t.Error("playout enabled immediately after flush, want ok=false")
	}

	// Re-prime from a high seq: not late-dropped (frontier was cleared).
	before := cap.len()
	r.handle(buildPacket(0, 500, 500*tFrames, tFrames, tChannels, 0.5), loopbackAddr)
	if cap.len() != before+1 {
		t.Errorf("post-flush re-prime pushed %d, want 1", cap.len()-before)
	}
	if idx, _ := cap.at(cap.len() - 1); idx != 500*tFrames {
		t.Errorf("re-prime push idx=%d want %d", idx, 500*tFrames)
	}
}
