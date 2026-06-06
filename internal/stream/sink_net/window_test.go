package sink_net

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// pkt builds a window-test source packet with the given seq/sampleIndex.
func pkt(seq uint64, sampleIndex int64) wire.Packet {
	return wire.Packet{Header: wire.Header{Seq: seq, SampleIndex: sampleIndex, CodecID: wire.CodecPCM}}
}

const chunkFrames = int64(480)

// seqsOf extracts the seqs from released packets, in release order.
func seqsOf(ps []wire.Packet) []uint64 {
	out := make([]uint64, len(ps))
	for i, p := range ps {
		out[i] = p.Header.Seq
	}
	return out
}

// TestWindowInOrder asserts in-order inserts release immediately in seq order.
func TestWindowInOrder(t *testing.T) {
	w := newWindow(8)
	var got []uint64
	for s := uint64(0); s < 5; s++ {
		rel, gaps := w.insert(pkt(s, int64(s)*chunkFrames), chunkFrames)
		if len(gaps) != 0 {
			t.Fatalf("seq %d: unexpected gaps %v", s, gaps)
		}
		got = append(got, seqsOf(rel)...)
	}
	want := []uint64{0, 1, 2, 3, 4}
	if !equalU64(got, want) {
		t.Errorf("release order=%v want %v", got, want)
	}
}

// TestWindowReorder asserts out-of-order inserts are held and released in seq order
// once the gap fills.
func TestWindowReorder(t *testing.T) {
	w := newWindow(8)
	// Arrive 0, 2, 3, then 1: 0 releases first; 2,3 held; 1 fills the gap → 1,2,3.
	rel, _ := w.insert(pkt(0, 0), chunkFrames)
	if !equalU64(seqsOf(rel), []uint64{0}) {
		t.Fatalf("after 0: %v", seqsOf(rel))
	}
	rel, _ = w.insert(pkt(2, 2*chunkFrames), chunkFrames)
	if len(rel) != 0 {
		t.Fatalf("early seq 2 should be held, got %v", seqsOf(rel))
	}
	rel, _ = w.insert(pkt(3, 3*chunkFrames), chunkFrames)
	if len(rel) != 0 {
		t.Fatalf("early seq 3 should be held, got %v", seqsOf(rel))
	}
	rel, _ = w.insert(pkt(1, 1*chunkFrames), chunkFrames)
	if !equalU64(seqsOf(rel), []uint64{1, 2, 3}) {
		t.Errorf("gap-fill release=%v want [1 2 3]", seqsOf(rel))
	}
}

// TestWindowDedupe asserts a seq delivered twice releases once.
func TestWindowDedupe(t *testing.T) {
	w := newWindow(8)
	rel, _ := w.insert(pkt(0, 0), chunkFrames)
	if len(rel) != 1 {
		t.Fatalf("first insert released %d", len(rel))
	}
	rel, _ = w.insert(pkt(0, 0), chunkFrames) // duplicate of a delivered seq
	if len(rel) != 0 {
		t.Errorf("duplicate of delivered seq released %d, want 0", len(rel))
	}
	// Duplicate of a HELD (not yet released) seq.
	_, _ = w.insert(pkt(2, 2*chunkFrames), chunkFrames) // held
	rel, _ = w.insert(pkt(2, 2*chunkFrames), chunkFrames)
	if len(rel) != 0 {
		t.Errorf("duplicate of held seq released %d, want 0", len(rel))
	}
}

// TestWindowLateDrop asserts a seq behind the frontier is dropped, never released.
func TestWindowLateDrop(t *testing.T) {
	w := newWindow(8)
	_, _ = w.insert(pkt(5, 5*chunkFrames), chunkFrames) // primes frontier at 5
	rel, _ := w.insert(pkt(3, 3*chunkFrames), chunkFrames)
	if len(rel) != 0 {
		t.Errorf("late seq 3 (frontier 6) released %d, want 0", len(rel))
	}
}

// TestWindowSlideConceal asserts that when the window fills with a stuck frontier,
// it slides forward reporting the missing seq as a gap at the right sampleIndex,
// then releases the held packets — subsequent audio is NOT shifted.
func TestWindowSlideConceal(t *testing.T) {
	w := newWindow(4) // small window to force a slide
	// seq 0 arrives (primes + releases, frontier→1); seq 1 never arrives; fill
	// 2..6 → window holds 5 > cap 4 with a stuck frontier at 1 → slide past 1,
	// concealing exactly one chunk at sampleIndex 1*chunkFrames.
	relFirst, _ := w.insert(pkt(0, 0), chunkFrames)
	if !equalU64(seqsOf(relFirst), []uint64{0}) {
		t.Fatalf("seq 0 release=%v want [0]", seqsOf(relFirst))
	}
	var gotGaps []gap
	var released []uint64
	for s := uint64(2); s <= 6; s++ {
		rel, gaps := w.insert(pkt(s, int64(s)*chunkFrames), chunkFrames)
		gotGaps = append(gotGaps, gaps...)
		released = append(released, seqsOf(rel)...)
	}
	if len(gotGaps) != 1 {
		t.Fatalf("expected exactly 1 conceal gap, got %d: %v", len(gotGaps), gotGaps)
	}
	if gotGaps[0].seq != 1 {
		t.Errorf("gap seq=%d want 1", gotGaps[0].seq)
	}
	// The concealed chunk sits at sampleIndex 1*chunkFrames (its true slot),
	// preserving alignment: seq 2's chunk stays at 2*chunkFrames.
	if gotGaps[0].sampleIndex != chunkFrames {
		t.Errorf("conceal sampleIndex=%d want %d", gotGaps[0].sampleIndex, chunkFrames)
	}
	// After the slide, 2..6 release in order (none lost/shifted).
	if !equalU64(released, []uint64{2, 3, 4, 5, 6}) {
		t.Errorf("post-slide release=%v want [2 3 4 5 6]", released)
	}
}

// TestWindowReset asserts reset re-primes cleanly.
func TestWindowReset(t *testing.T) {
	w := newWindow(8)
	_, _ = w.insert(pkt(0, 0), chunkFrames)
	_, _ = w.insert(pkt(1, chunkFrames), chunkFrames)
	w.reset()
	// After reset, a high seq re-primes the frontier without treating it as late.
	rel, gaps := w.insert(pkt(100, 100*chunkFrames), chunkFrames)
	if len(gaps) != 0 {
		t.Errorf("post-reset gaps=%v want none", gaps)
	}
	if !equalU64(seqsOf(rel), []uint64{100}) {
		t.Errorf("post-reset release=%v want [100]", seqsOf(rel))
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
