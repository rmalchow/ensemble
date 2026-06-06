package ring

import (
	"sync"
	"testing"
)

func ramp(n int) []float32 {
	p := make([]float32, n)
	for i := range p {
		p[i] = float32(i)
	}
	return p
}

func equalF32(a, b []float32) bool {
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

func TestRingNewCap(t *testing.T) {
	if got := NewRing(16).Cap(); got != 16 {
		t.Fatalf("Cap = %d, want 16", got)
	}
	// Non-positive capacity is a usable empty ring, not a panic.
	r := NewRing(-4)
	if r.Cap() != 0 {
		t.Fatalf("Cap = %d, want 0", r.Cap())
	}
	if n := r.Write(ramp(4)); n != 0 {
		t.Fatalf("Write into zero-cap ring = %d, want 0", n)
	}
}

func TestRingRoundTrip(t *testing.T) {
	r := NewRing(8)
	in := ramp(8)
	if n := r.Write(in); n != 8 {
		t.Fatalf("Write = %d, want 8", n)
	}
	if got := r.Len(); got != 8 {
		t.Fatalf("Len = %d, want 8", got)
	}
	out := make([]float32, 8)
	if n := r.Read(out); n != 8 {
		t.Fatalf("Read = %d, want 8", n)
	}
	if !equalF32(in, out) {
		t.Fatalf("round-trip mismatch: in=%v out=%v", in, out)
	}
	if got := r.Len(); got != 0 {
		t.Fatalf("Len after full read = %d, want 0", got)
	}
}

func TestRingOverrun(t *testing.T) {
	r := NewRing(4)
	// First write fills the ring exactly.
	if n := r.Write(ramp(4)); n != 4 {
		t.Fatalf("Write = %d, want 4", n)
	}
	// Second write must short-count to 0 and leave the existing tail intact.
	extra := []float32{100, 101, 102}
	if n := r.Write(extra); n != 0 {
		t.Fatalf("Write when full = %d, want 0", n)
	}
	out := make([]float32, 4)
	if n := r.Read(out); n != 4 {
		t.Fatalf("Read = %d, want 4", n)
	}
	if !equalF32(out, ramp(4)) {
		t.Fatalf("unread tail clobbered: got %v", out)
	}
}

func TestRingPartialOverrun(t *testing.T) {
	r := NewRing(4)
	r.Write(ramp(2)) // 2 buffered, 2 free
	// Offer 5; only 2 should fit.
	in := []float32{10, 11, 12, 13, 14}
	if n := r.Write(in); n != 2 {
		t.Fatalf("Write = %d, want 2", n)
	}
	if got := r.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4", got)
	}
	out := make([]float32, 4)
	r.Read(out)
	want := []float32{0, 1, 10, 11}
	if !equalF32(out, want) {
		t.Fatalf("got %v, want %v", out, want)
	}
}

func TestRingUnderrun(t *testing.T) {
	r := NewRing(8)
	// Empty read returns 0.
	if n := r.Read(make([]float32, 4)); n != 0 {
		t.Fatalf("Read empty = %d, want 0", n)
	}
	r.Write(ramp(3))
	out := make([]float32, 8)
	if n := r.Read(out); n != 3 {
		t.Fatalf("Read = %d, want 3 (short count)", n)
	}
	if !equalF32(out[:3], ramp(3)) {
		t.Fatalf("got %v, want %v", out[:3], ramp(3))
	}
}

func TestRingZeroLenArgs(t *testing.T) {
	r := NewRing(4)
	if n := r.Write(nil); n != 0 {
		t.Fatalf("Write(nil) = %d, want 0", n)
	}
	r.Write(ramp(2))
	if n := r.Read(nil); n != 0 {
		t.Fatalf("Read(nil) = %d, want 0", n)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
}

// TestRingWrap exercises copies that straddle the physical end of the backing slice.
func TestRingWrap(t *testing.T) {
	r := NewRing(4)
	// Advance head/tail to 2 so subsequent writes wrap.
	r.Write(ramp(2))
	r.Read(make([]float32, 2)) // head=tail=2, empty
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
	// Write 4 starting at index 2: indices 2,3 then wrap to 0,1.
	in := []float32{20, 21, 22, 23}
	if n := r.Write(in); n != 4 {
		t.Fatalf("Write = %d, want 4", n)
	}
	if r.Len() != 4 {
		t.Fatalf("Len = %d, want 4", r.Len())
	}
	// Read it all back; ordering must be preserved across the wrap.
	out := make([]float32, 4)
	if n := r.Read(out); n != 4 {
		t.Fatalf("Read = %d, want 4", n)
	}
	if !equalF32(out, in) {
		t.Fatalf("wrap order mismatch: got %v want %v", out, in)
	}
}

// TestRingWrapInterleaved drives many small interleaved write/read cycles so the
// indices wrap repeatedly and Len stays consistent throughout.
func TestRingWrapInterleaved(t *testing.T) {
	const cap = 6
	r := NewRing(cap)
	var next float32 // next value the producer will write
	var expect float32
	buffered := 0
	for round := 0; round < 200; round++ {
		// Try to write 4 (may short-count near full).
		w := make([]float32, 4)
		for i := range w {
			w[i] = next + float32(i)
		}
		n := r.Write(w)
		next += float32(n)
		buffered += n
		if got := r.Len(); got != buffered {
			t.Fatalf("round %d: Len = %d, want %d", round, got, buffered)
		}
		// Read up to 3.
		out := make([]float32, 3)
		rn := r.Read(out)
		buffered -= rn
		for i := 0; i < rn; i++ {
			if out[i] != expect {
				t.Fatalf("round %d: out[%d] = %v, want %v", round, i, out[i], expect)
			}
			expect++
		}
		if got := r.Len(); got != buffered {
			t.Fatalf("round %d: Len after read = %d, want %d", round, got, buffered)
		}
		if buffered > cap {
			t.Fatalf("round %d: buffered %d exceeds cap %d", round, buffered, cap)
		}
	}
}

func TestRingReset(t *testing.T) {
	r := NewRing(8)
	r.Write(ramp(5))
	if r.Len() != 5 {
		t.Fatalf("Len = %d, want 5", r.Len())
	}
	r.Reset()
	if r.Len() != 0 {
		t.Fatalf("Len after Reset = %d, want 0", r.Len())
	}
	if n := r.Read(make([]float32, 4)); n != 0 {
		t.Fatalf("Read after Reset = %d, want 0", n)
	}
	// Ring is fully reusable post-Reset.
	if n := r.Write(ramp(8)); n != 8 {
		t.Fatalf("Write after Reset = %d, want 8", n)
	}
	out := make([]float32, 8)
	if n := r.Read(out); n != 8 || !equalF32(out, ramp(8)) {
		t.Fatalf("reuse after Reset failed: n=%d out=%v", n, out)
	}
}

// TestRingSPSCConcurrent runs a producer and consumer goroutine through the ring and
// checks the consumer observes a contiguous, in-order sample stream (race detector
// catches data races; -race recommended).
func TestRingSPSCConcurrent(t *testing.T) {
	const total = 1 << 16
	r := NewRing(512)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // producer
		defer wg.Done()
		var next float32
		written := 0
		chunk := make([]float32, 0, 64)
		for written < total {
			chunk = chunk[:0]
			for i := 0; i < 64 && written+len(chunk) < total; i++ {
				chunk = append(chunk, next+float32(i))
			}
			n := r.Write(chunk)
			next += float32(n)
			written += n
		}
	}()

	go func() { // consumer
		defer wg.Done()
		var expect float32
		read := 0
		buf := make([]float32, 48)
		for read < total {
			n := r.Read(buf)
			for i := 0; i < n; i++ {
				if buf[i] != expect {
					t.Errorf("at %d: got %v want %v", read+i, buf[i], expect)
					return
				}
				expect++
			}
			read += n
		}
	}()

	wg.Wait()
	if r.Len() != 0 {
		t.Fatalf("Len after drain = %d, want 0", r.Len())
	}
}
