package fec

import "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"

// dupFEC is the MCU-friendly scheme (FECID = Duplicate, doc 05 §5.5.4, A.10):
// every source packet is sent twice, the copy displaced Ddup packets later in the
// send order. A burst shorter than Ddup loses at most one copy of any packet, so
// the other copy survives (A.10: "Ddup = 5 packets (~50 ms burst tolerance)").
// Cost is +100% bandwidth; decode is a hash-set membership test (no XOR math).
//
// Protect uses a FIFO delay line of depth Ddup: it emits the source now and, once
// the line is full, the duplicate of the packet that entered Ddup calls ago.
// Recover keeps a bounded seen-seq window and drops any seq already delivered
// (first copy wins, §5.6.2).
//
// Pure compute, no goroutines/I/O/clock. The delay line holds Ddup slices by
// reference; the seen window is a fixed ring of seqs — O(1) memory, no per-packet
// allocation in steady state.
type dupFEC struct {
	cfg DupConfig

	// delay is the master-side FIFO of the last <=Offset source packets awaiting
	// re-emission as duplicates. It holds the marshaled bytes by reference.
	delay [][]byte
	head  int // index of the oldest entry when the line is full

	// seen is the follower-side dedupe window: a fixed ring of recently delivered
	// seqs plus a presence set. Sized > the displacement so both copies of any
	// in-flight packet are caught (doc 05 §5.6.2: window must exceed the offset).
	seen     map[uint64]struct{}
	seenRing []uint64
	seenPos  int
	seenLen  int
}

// dedupeWindow returns the seen-seq window size for offset: comfortably larger
// than the Ddup displacement and the XOR-class D·k≈32 figure (§5.6.2 "~32
// packets ≈ 320 ms") so reordered duplicates within the recovery span are still
// recognized as duplicates. Bounded => O(1) memory, ages out by seq.
func dedupeWindow(offset int) int {
	const floor = 64 // > D·k (32) and > any sane Ddup
	if n := offset * 8; n > floor {
		return n
	}
	return floor
}

// NewDup constructs the duplication scheme with cfg (non-positive Offset falls
// back to the A.12 default 5). Pre-sizes the delay line and dedupe window.
func NewDup(cfg DupConfig) FEC {
	cfg = cfg.normalize()
	w := dedupeWindow(cfg.Offset)
	return &dupFEC{
		cfg:      cfg,
		delay:    make([][]byte, 0, cfg.Offset),
		seen:     make(map[uint64]struct{}, w),
		seenRing: make([]uint64, w),
	}
}

// ID reports Duplicate (2).
func (*dupFEC) ID() FECID { return Duplicate }

// Protect emits the source packet now plus, once the delay line has filled to
// Offset, the duplicate displaced Offset packets earlier (doc 05 §5.5.4). seq is
// unused: ordering alone drives the displacement.
func (f *dupFEC) Protect(seq uint64, pkt []byte) (out [][]byte) {
	if len(f.delay) < f.cfg.Offset {
		// Filling phase: no displaced duplicate yet; just buffer this packet.
		f.delay = append(f.delay, pkt)
		return [][]byte{pkt}
	}
	// Steady state: emit the source and the packet that entered Offset calls ago,
	// then overwrite that slot with the current packet (ring FIFO).
	dup := f.delay[f.head]
	f.delay[f.head] = pkt
	f.head = (f.head + 1) % f.cfg.Offset
	return [][]byte{pkt, dup}
}

// Recover delivers a source packet the first time its seq is seen and drops any
// later copy (doc 05 §5.6.2: first copy wins). Returns nil for a duplicate.
func (f *dupFEC) Recover(p wire.Packet) (recovered []wire.Packet) {
	if _, ok := f.seen[p.Header.Seq]; ok {
		return nil // duplicate — drop
	}
	f.markSeen(p.Header.Seq)
	return []wire.Packet{p}
}

// markSeen records seq in the bounded dedupe window, evicting the oldest seq when
// the ring wraps so memory stays O(window).
func (f *dupFEC) markSeen(seq uint64) {
	if f.seenLen == len(f.seenRing) {
		delete(f.seen, f.seenRing[f.seenPos]) // evict oldest
	} else {
		f.seenLen++
	}
	f.seenRing[f.seenPos] = seq
	f.seenPos = (f.seenPos + 1) % len(f.seenRing)
	f.seen[seq] = struct{}{}
}
