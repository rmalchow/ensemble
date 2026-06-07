package stream

import "sort"

// reorderBuffer delivers frames in Seq order, at most once, tolerating bounded
// out-of-order arrival and gaps. It does NOT buffer for the jitter deadline
// (E's job) — it only fixes ordering/dedup over the FEC/reorder horizon
// (maxAhead frames). Guarded by the caller's mutex.
type reorderBuffer struct {
	gen      uint32
	next     uint64 // next Seq to deliver
	started  bool   // first frame has anchored next
	pend     map[uint64]frameRec
	maxAhead uint64
}

type frameRec struct {
	seq     uint64
	pts     int64
	payload []byte
}

// defaultMaxAhead is the reorder horizon in frames (~640 ms at 20 ms/frame).
const defaultMaxAhead = 32

func newReorderBuffer(gen uint32) reorderBuffer {
	return reorderBuffer{
		gen:      gen,
		pend:     make(map[uint64]frameRec),
		maxAhead: defaultMaxAhead,
	}
}

// reset rebinds to a new generation and drops all pending frames.
func (b *reorderBuffer) reset(gen uint32) {
	b.gen = gen
	b.next = 0
	b.started = false
	b.pend = make(map[uint64]frameRec)
}

// admit ingests one frame (gen-checked by the caller). It returns:
//   - deliver: zero or more frames now deliverable in Seq order
//   - lost:    frames skipped (gaps the window gave up on) this call
//   - dup:     true if seq was a duplicate / already-past frame (dropped)
//   - stale:   true if gen < b.gen (caller should already have filtered)
func (b *reorderBuffer) admit(gen uint32, seq uint64, pts int64, payload []byte) (deliver []frameRec, lost int, dup bool, stale bool) {
	if gen < b.gen {
		return nil, 0, false, true
	}
	if gen > b.gen {
		// Defensive: a higher gen re-anchors the window.
		b.reset(gen)
	}
	if !b.started {
		b.started = true
		b.next = seq
	}
	if seq < b.next {
		return nil, 0, true, false // already delivered or skipped
	}
	if _, ok := b.pend[seq]; ok {
		return nil, 0, true, false // duplicate pending
	}

	cp := make([]byte, len(payload))
	copy(cp, payload)
	b.pend[seq] = frameRec{seq: seq, pts: pts, payload: cp}

	// Force progress if the furthest pending frame is beyond the horizon:
	// drop the contiguous gap at the head until we are back within maxAhead.
	if maxSeq := b.maxPending(); maxSeq >= b.next+b.maxAhead {
		for maxSeq >= b.next+b.maxAhead {
			if _, ok := b.pend[b.next]; !ok {
				lost++
			}
			delete(b.pend, b.next)
			b.next++
		}
	}

	// Drain in-order frames.
	for {
		rec, ok := b.pend[b.next]
		if !ok {
			break
		}
		delete(b.pend, b.next)
		deliver = append(deliver, rec)
		b.next++
	}
	return deliver, lost, false, false
}

func (b *reorderBuffer) maxPending() uint64 {
	var mx uint64
	first := true
	for s := range b.pend {
		if first || s > mx {
			mx = s
			first = false
		}
	}
	return mx
}

// pendingSeqs returns the sorted pending seqs (test helper).
func (b *reorderBuffer) pendingSeqs() []uint64 {
	out := make([]uint64, 0, len(b.pend))
	for s := range b.pend {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
