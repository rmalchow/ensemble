package fec

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// dupProtect runs Protect over seq 0..n-1 and returns the emitted packets parsed,
// in send order, tagging each with whether it is the source's first emission.
func dupProtect(t *testing.T, f FEC, n int) []wire.Packet {
	t.Helper()
	var out []wire.Packet
	for seq := uint64(0); seq < uint64(n); seq++ {
		for _, b := range f.Protect(seq, srcPacket(t, seq, 1920)) {
			out = append(out, parse(t, b))
		}
	}
	return out
}

func TestDupOffset(t *testing.T) {
	const offset, n = 5, 20
	f := NewDup(DupConfig{Offset: offset})
	emitted := dupProtect(t, f, n)

	// In steady state each Protect after the fill phase emits [source, dup] where
	// dup.seq == source.seq - offset. The first `offset` calls emit source only.
	seen := map[uint64]int{}
	for _, p := range emitted {
		seen[p.Header.Seq]++
	}
	// Every seq whose displaced dup still fits in the stream (seq < n-offset... )
	// is emitted twice; the last `offset` seqs have their dup still in the delay
	// line (tail not flushed — receiver re-News on gen change, 05 §5.8 / open-q#4).
	for seq := uint64(0); seq < uint64(n); seq++ {
		want := 2
		if seq >= uint64(n-offset) {
			want = 1 // dup still buffered, not yet displaced
		}
		if seen[seq] != want {
			t.Fatalf("seq %d emitted %d times, want %d", seq, seen[seq], want)
		}
	}

	// Verify the displacement: the dup of seq S is emitted in the same bundle as
	// source S+offset (the FIFO of depth offset). Walk per-Protect bundles.
	enc2 := NewDup(DupConfig{Offset: offset})
	for seq := uint64(0); seq < uint64(n); seq++ {
		bundle := enc2.Protect(seq, srcPacket(t, seq, 1920))
		if seq < offset {
			if len(bundle) != 1 {
				t.Fatalf("fill phase seq %d emitted %d packets, want 1", seq, len(bundle))
			}
			continue
		}
		if len(bundle) != 2 {
			t.Fatalf("steady seq %d emitted %d packets, want 2", seq, len(bundle))
		}
		dup := parse(t, bundle[1])
		if dup.Header.Seq != seq-offset {
			t.Fatalf("seq %d dup is seq %d, want %d (offset %d)", seq, dup.Header.Seq, seq-offset, offset)
		}
	}
}

func TestDupDedupe(t *testing.T) {
	const offset, n = 5, 20
	enc := NewDup(DupConfig{Offset: offset})
	emitted := dupProtect(t, enc, n)

	dec := NewDup(DupConfig{Offset: offset})
	delivered := map[uint64]int{}
	for _, p := range emitted {
		for _, r := range dec.Recover(p) {
			delivered[r.Header.Seq]++
		}
	}
	for seq := uint64(0); seq < uint64(n); seq++ {
		if delivered[seq] != 1 {
			t.Fatalf("seq %d delivered %d times, want exactly 1 (first wins)", seq, delivered[seq])
		}
	}
}

func TestDupSingleCopySurvival(t *testing.T) {
	const offset, n = 5, 20
	enc := NewDup(DupConfig{Offset: offset})
	emitted := dupProtect(t, enc, n)

	tests := []struct {
		name string
		drop func(copyIndex int) bool // copyIndex: 0=first copy, 1=second copy
	}{
		{"drop-first-copy", func(ci int) bool { return ci == 0 }},
		{"drop-second-copy", func(ci int) bool { return ci == 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := NewDup(DupConfig{Offset: offset})
			delivered := map[uint64]int{}
			copySeen := map[uint64]int{}
			for _, p := range emitted {
				ci := copySeen[p.Header.Seq]
				copySeen[p.Header.Seq]++
				if tt.drop(ci) {
					continue // simulate this copy lost
				}
				for _, r := range dec.Recover(p) {
					delivered[r.Header.Seq]++
				}
			}
			// Any seq with two copies in the stream survives the loss of one.
			for seq := uint64(0); seq < uint64(n-offset); seq++ {
				if delivered[seq] != 1 {
					t.Fatalf("%s: seq %d delivered %d, want 1", tt.name, seq, delivered[seq])
				}
			}
		})
	}

	t.Run("both-copies-lost-gap", func(t *testing.T) {
		dec := NewDup(DupConfig{Offset: offset})
		delivered := map[uint64]int{}
		const gapSeq = 3
		for _, p := range emitted {
			if p.Header.Seq == gapSeq {
				continue // both copies dropped
			}
			for _, r := range dec.Recover(p) {
				delivered[r.Header.Seq]++
			}
		}
		if delivered[gapSeq] != 0 {
			t.Fatalf("seq %d delivered despite both copies lost", gapSeq)
		}
	})
}

func TestDupBurstWithinDdup(t *testing.T) {
	const offset, n = 5, 30
	enc := NewDup(DupConfig{Offset: offset})
	emitted := dupProtect(t, enc, n)

	// A burst shorter than the displacement: drop a contiguous run of emitted
	// packets of length < offset. Because each source's dup is offset positions
	// away, a burst < offset cannot swallow both copies of the same seq.
	dec := NewDup(DupConfig{Offset: offset})
	delivered := map[uint64]bool{}
	burstStart, burstLen := 7, offset-1
	for i, p := range emitted {
		if i >= burstStart && i < burstStart+burstLen {
			continue // dropped in the burst
		}
		for _, r := range dec.Recover(p) {
			delivered[r.Header.Seq] = true
		}
	}
	for seq := uint64(0); seq < uint64(n-offset); seq++ {
		if !delivered[seq] {
			t.Fatalf("seq %d lost to a burst of %d (< Ddup=%d) — should survive", seq, burstLen, offset)
		}
	}
}

func TestDupID(t *testing.T) {
	if NewDup(DefaultDupConfig()).ID() != Duplicate {
		t.Fatal("NewDup ID != Duplicate")
	}
}
