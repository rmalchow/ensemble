package origin

import "container/heap"

// pacer is the send-time scheduler (05 §5.6.1): a min-heap keyed by the monotonic
// send instant (master-mono ns). The origin loop pushes each chunk's bundle of
// already-marshaled packets (source + interleaved repairs, in the order
// fec.Protect returned them) with the send instant playout(idx)−Lead; the Run loop
// pops the due bundles in send-time order and fans each out to every listener.
//
// Repair packets trail their group's source packets because the FEC layer emits
// them after the group completes (05 §5.5.3); the pacer preserves that order via a
// monotonically increasing seqno tiebreaker so two bundles with the same sendAt
// (or a repair scheduled at the same instant as a later source chunk) keep their
// production order. The heap is single-goroutine (owned by Run); no locking.
type pacer struct {
	h pacerHeap
	// nextOrd is the FIFO tiebreaker so equal-sendAt bundles release in push order.
	nextOrd uint64
}

// bundle is one scheduled unit: the packets to fan out at sendAt. For PCM/None it
// is a single source packet; for XOR/Dup it is the source plus its repairs as
// returned by fec.Protect (each already a full marshaled wire packet).
type bundle struct {
	sendAt  int64    // master-mono ns at which to write this bundle
	packets [][]byte // marshaled wire packets, fanned out verbatim to each listener
	ord     uint64   // FIFO tiebreaker for equal sendAt
}

func (p *pacer) push(sendAt int64, packets [][]byte) {
	heap.Push(&p.h, bundle{sendAt: sendAt, packets: packets, ord: p.nextOrd})
	p.nextOrd++
}

// peek returns the earliest scheduled send instant and whether the heap is
// non-empty, without popping.
func (p *pacer) peek() (sendAt int64, ok bool) {
	if len(p.h) == 0 {
		return 0, false
	}
	return p.h[0].sendAt, true
}

// pop removes and returns the earliest bundle. The caller must check len via peek.
func (p *pacer) pop() bundle {
	return heap.Pop(&p.h).(bundle)
}

func (p *pacer) len() int { return len(p.h) }

// reset drops all scheduled bundles (used on ResumeAt / generation change so no
// prior-generation packet is ever sent under the new gen, 05 §5.8).
func (p *pacer) reset() {
	p.h = p.h[:0]
	p.nextOrd = 0
}

// pacerHeap is the container/heap implementation ordered by (sendAt, ord).
type pacerHeap []bundle

func (h pacerHeap) Len() int { return len(h) }
func (h pacerHeap) Less(i, j int) bool {
	if h[i].sendAt != h[j].sendAt {
		return h[i].sendAt < h[j].sendAt
	}
	return h[i].ord < h[j].ord
}
func (h pacerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *pacerHeap) Push(x any) { *h = append(*h, x.(bundle)) }

func (h *pacerHeap) Pop() any {
	old := *h
	n := len(old)
	b := old[n-1]
	old[n-1] = bundle{} // drop the slice reference so it can be GC'd
	*h = old[:n-1]
	return b
}
