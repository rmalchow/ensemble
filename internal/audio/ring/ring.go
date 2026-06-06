// Package ring is the jitter buffer (~LeadMs, canonically 300 ms). Leaf: imports
// no sibling internal packages.
package ring

import "sync"

// Ring is a single-producer/single-consumer interleaved-f32 jitter buffer holding
// ~LeadMs of audio ahead of the sink. Producer = decode+resample goroutine;
// consumer = sink Write. A mutex guards the indices and count; that is sufficient
// because there is no live hardware in the hot path and the buffer is the system's
// jitter absorber, not a lock-free contention point.
type Ring struct {
	mu   sync.Mutex
	buf  []float32
	head int // read position
	tail int // write position
	n    int // samples currently buffered (0..len(buf))
}

// NewRing sizes the buffer to hold `capSamples` interleaved samples. A non-positive
// capacity yields an empty ring (every Write returns 0).
func NewRing(capSamples int) *Ring {
	if capSamples < 0 {
		capSamples = 0
	}
	return &Ring{buf: make([]float32, capSamples)}
}

// Write copies up to the free space from p, returning the number of samples written.
// It returns a short count (possibly 0) when the ring is full; the producer then waits
// and retries.
func (r *Ring) Write(p []float32) (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	free := len(r.buf) - r.n
	if free <= 0 || len(p) == 0 {
		return 0
	}
	if len(p) > free {
		p = p[:free]
	}
	for len(p) > 0 {
		c := copy(r.buf[r.tail:], p)
		r.tail += c
		if r.tail == len(r.buf) {
			r.tail = 0
		}
		r.n += c
		n += c
		p = p[c:]
	}
	return n
}

// Read copies up to the available samples into p, returning the number read. It
// returns a short count (possibly 0) when the ring is empty; the consumer then fills
// the remainder with silence.
func (r *Ring) Read(p []float32) (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	avail := r.n
	if avail <= 0 || len(p) == 0 {
		return 0
	}
	if len(p) > avail {
		p = p[:avail]
	}
	for len(p) > 0 {
		c := copy(p, r.buf[r.head:])
		r.head += c
		if r.head == len(r.buf) {
			r.head = 0
		}
		r.n -= c
		n += c
		p = p[c:]
	}
	return n
}

// Len reports the samples currently buffered.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Cap reports the buffer capacity in samples.
func (r *Ring) Cap() int {
	return len(r.buf)
}

// Reset drops all buffered samples (used on a hard reseek).
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = 0
	r.tail = 0
	r.n = 0
}
