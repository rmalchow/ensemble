package sink_net

import "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"

// window is the bounded, seq-keyed reorder + dedupe buffer (05 §5.6.2). It absorbs
// network reordering and FEC/duplication deduping: packets are held until they can
// be released in seq order, duplicates are dropped, and packets that age out (the
// window slides past their seq) are reported as gaps so the receiver conceals the
// missing chunk at the correct sampleIndex (the cardinal rule, 05 §5.6.3).
//
// The window tracks the next seq to release (expected, the play frontier). insert
// returns the source packets that become releasable in seq order, plus any seq
// gaps the slide skipped over (unrecoverable — to be concealed). It is
// single-goroutine (owned by the receiver Run loop); no locking.
//
// Sizing: capacity is Config.WindowPackets (default 32 ≈ 320 ms), which must exceed
// D×k (= 32 at the A.12 defaults) so an interleaved parity group can complete
// before its packets age out (05 §5.6.2).
type window struct {
	cap      int                    // max distinct held seqs before forcing a slide
	expected uint64                 // next seq to release (the play frontier)
	primed   bool                   // false until the first packet seeds expected
	held     map[uint64]wire.Packet // seq → packet awaiting in-order release

	// delivered records released seqs so a duplicate (Dup mode or accidental) is
	// dropped; pruned below the frontier−cap so it stays bounded.
	delivered map[uint64]struct{}

	// lastReleasedIdx is the sampleIndex of the most recently released or concealed
	// chunk; a gap at the frontier sits one FramesPerChunk after it (05 §5.1: the
	// chunk size is fixed for a streamGen). Seeded from the first packet so the
	// first concealment projects correctly.
	lastReleasedIdx int64
	haveIdx         bool
}

// gap is an unrecoverable missing chunk reported by the window when it slides past
// a seq that never arrived. The receiver conceals one chunk at sampleIndex.
type gap struct {
	seq         uint64
	sampleIndex int64
}

func newWindow(capPackets int) *window {
	if capPackets <= 0 {
		capPackets = 32
	}
	return &window{
		cap:       capPackets,
		held:      make(map[uint64]wire.Packet, capPackets),
		delivered: make(map[uint64]struct{}, capPackets*2),
	}
}

// reset clears all state (generation change / FlushAndReprime, 05 §5.8 / R11).
func (w *window) reset() {
	w.expected = 0
	w.primed = false
	w.haveIdx = false
	w.lastReleasedIdx = 0
	for k := range w.held {
		delete(w.held, k)
	}
	for k := range w.delivered {
		delete(w.delivered, k)
	}
}

// insert adds one recovered source packet p (already Cloned — it is retained) and
// returns the packets now releasable in seq order plus any gaps the window skipped.
//
// Policy (05 §5.6.3):
//   - duplicate (already delivered or held): dropped.
//   - late (seq < expected, behind the play frontier): dropped.
//   - in-order / early-within-window: held; the in-order prefix is released.
//   - window full with a stuck frontier: slide forward one seq, reporting the
//     skipped seq as a gap so the receiver conceals that chunk (never shifts
//     subsequent audio — the cardinal rule).
//
// chunkFrames is FramesPerChunk, used to project a gap's sampleIndex from its
// predecessor.
func (w *window) insert(p wire.Packet, chunkFrames int64) (released []wire.Packet, gaps []gap) {
	seq := p.Header.Seq

	if !w.primed {
		w.expected = seq
		w.primed = true
	}
	if !w.haveIdx {
		// Seed the index frontier one chunk before the first sample so the first
		// release lands on p.SampleIndex and a leading gap projects correctly.
		w.lastReleasedIdx = p.Header.SampleIndex - chunkFrames
		w.haveIdx = true
	}

	if seq < w.expected {
		return nil, nil // late: behind the play frontier — drop
	}
	if _, ok := w.delivered[seq]; ok {
		return nil, nil // duplicate already released — drop
	}
	if _, ok := w.held[seq]; ok {
		return nil, nil // duplicate still in window — drop
	}

	w.held[seq] = p

	// Force slides while the window holds more than cap distinct seqs and the
	// frontier itself is missing — concealing the stuck gap so newer audio is not
	// held hostage.
	for len(w.held) > w.cap {
		if _, ok := w.held[w.expected]; ok {
			break // frontier is fillable; release it below instead of skipping
		}
		w.lastReleasedIdx += chunkFrames
		gaps = append(gaps, gap{seq: w.expected, sampleIndex: w.lastReleasedIdx})
		w.markDelivered(w.expected)
		w.expected++
	}

	// Release the in-order prefix.
	for {
		pkt, ok := w.held[w.expected]
		if !ok {
			break
		}
		released = append(released, pkt)
		w.lastReleasedIdx = pkt.Header.SampleIndex
		w.markDelivered(w.expected)
		delete(w.held, w.expected)
		w.expected++
	}
	return released, gaps
}

// markDelivered records a released/concealed seq and prunes the dedupe set below
// the frontier−cap so it stays bounded.
func (w *window) markDelivered(seq uint64) {
	w.delivered[seq] = struct{}{}
	if w.expected > uint64(w.cap) {
		prune := w.expected - uint64(w.cap)
		// Bounded prune: drop the single entry that just fell off the trailing edge.
		delete(w.delivered, prune-1)
	}
}
