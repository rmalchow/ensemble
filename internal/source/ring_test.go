package source

import (
	"testing"

	"ensemble/internal/stream"
)

func pcm(b byte) []byte {
	p := make([]byte, stream.FrameBytes)
	for i := range p {
		p[i] = b
	}
	return p
}

func TestRingCapacityFrames(t *testing.T) {
	// bufferMs=150 -> 2*150=300 < 1000 -> floor 1000ms / 20 = 50 frames
	if n := ringCapacityFrames(150); n != 50 {
		t.Fatalf("cap=%d want 50", n)
	}
	// bufferMs=800 -> 1600ms / 20 = 80
	if n := ringCapacityFrames(800); n != 80 {
		t.Fatalf("cap=%d want 80", n)
	}
}

func TestRingPushWrap(t *testing.T) {
	var r ringBuffer
	r.resize(150) // 50-frame cap
	for i := 0; i < 60; i++ {
		r.push(uint64(i), int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	if r.count != 50 {
		t.Fatalf("count=%d want 50 (cap)", r.count)
	}
}

func TestRingPrimeDeadlineCutoff(t *testing.T) {
	var r ringBuffer
	r.resize(150) // 50-frame cap; bufMs=150 -> keep most recent 150ms = ~7-8 frames
	// push 50 frames, pts increments by FrameNanos.
	for i := 0; i < 50; i++ {
		r.push(uint64(i), int64(i)*stream.FrameNanos, pcm(byte(i)))
	}
	got := r.prime()
	// cutoff = lastPTS - 150ms = (49*20ms) - 150ms = 980-150 = 830ms -> seq 42..49
	if len(got) == 0 {
		t.Fatal("prime returned nothing")
	}
	// All returned frames must have pts >= cutoff, ordered oldest->newest.
	cutoff := int64(49)*stream.FrameNanos - 150*1_000_000
	for i, s := range got {
		if s.pts < cutoff {
			t.Fatalf("frame below cutoff included: pts=%d", s.pts)
		}
		if i > 0 && got[i-1].seq+1 != s.seq {
			t.Fatalf("not contiguous/ordered: %d then %d", got[i-1].seq, s.seq)
		}
	}
	if got[len(got)-1].seq != 49 {
		t.Fatalf("newest primed seq=%d want 49", got[len(got)-1].seq)
	}
}

func TestRingResizeClears(t *testing.T) {
	var r ringBuffer
	r.resize(150)
	r.push(0, 0, pcm(1))
	r.resize(800) // 80-frame cap
	if r.count != 0 || r.hasLast {
		t.Fatal("resize should clear")
	}
	if len(r.frames) != 80 {
		t.Fatalf("cap=%d want 80", len(r.frames))
	}
}

func TestRingPrimeEmpty(t *testing.T) {
	var r ringBuffer
	r.resize(150)
	if r.prime() != nil {
		t.Fatal("empty ring prime should be nil")
	}
}
