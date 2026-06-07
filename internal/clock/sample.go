package clock

import "sort"

// sample is one completed NTP-style exchange.
//
//	offset = ((t2 - t1) + (t3 - t4)) / 2   (master_ns - local_ns)
//	rtt    = (t4 - t1) - (t3 - t2)          (>= 0, smaller is better)
type sample struct {
	offset int64
	rtt    int64
}

// newSample computes offset and rtt from the four NTP timestamps (all ns).
func newSample(t1, t2, t3, t4 int64) sample {
	return sample{
		offset: ((t2 - t1) + (t3 - t4)) / 2,
		rtt:    (t4 - t1) - (t3 - t2),
	}
}

const (
	windowSize = 30 // "last 30" (§7)
	bestN      = 5  // "5 best-RTT samples" (§7)
)

// estimator keeps the last windowSize samples and reports the median offset of
// the bestN with smallest RTT. Not safe for concurrent use; the Follower's
// mutex guards it.
type estimator struct {
	ring  []sample // up to windowSize, oldest-first (append + drop front)
	count uint64   // total samples ever added (for stats / debugging)
}

// add inserts a sample, evicting the oldest when the window is full.
func (e *estimator) add(s sample) {
	e.count++
	if len(e.ring) < windowSize {
		e.ring = append(e.ring, s)
		return
	}
	// Full: drop the oldest (front), append the newest.
	copy(e.ring, e.ring[1:])
	e.ring[len(e.ring)-1] = s
}

// offset returns the current estimate and whether it is usable.
// ok is false until at least one sample exists (the *unsynced* gate, §7).
// With 1..bestN samples it medians whatever it has; with more, the bestN by RTT.
func (e *estimator) offset() (offsetNanos int64, ok bool) {
	if len(e.ring) == 0 {
		return 0, false
	}
	// Copy and sort by RTT ascending to pick the best-RTT samples.
	byRTT := make([]sample, len(e.ring))
	copy(byRTT, e.ring)
	sort.Slice(byRTT, func(i, j int) bool { return byRTT[i].rtt < byRTT[j].rtt })

	n := bestN
	if n > len(byRTT) {
		n = len(byRTT)
	}
	offsets := make([]int64, n)
	for i := 0; i < n; i++ {
		offsets[i] = byRTT[i].offset
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	// Lower-middle median (deterministic, integer-only; avoids int64 averaging).
	return offsets[(len(offsets)-1)/2], true
}

// reset discards all samples (resync on generation / master change, §7/§8.4).
func (e *estimator) reset() {
	e.ring = e.ring[:0]
}

// len reports how many samples are currently held (tests / stats).
func (e *estimator) len() int {
	return len(e.ring)
}
