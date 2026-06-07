package sink

import (
	"sync"
	"testing"

	"ensemble/internal/stream"
)

// flushBackend records Flush calls (contracts.Flusher).
type flushBackend struct {
	mu      sync.Mutex
	flushes int
}

func (f *flushBackend) Write([]byte) error { return nil }
func (f *flushBackend) Close() error       { return nil }
func (f *flushBackend) Flush() {
	f.mu.Lock()
	f.flushes++
	f.mu.Unlock()
}
func (f *flushBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// TestDisarmFlushesBackend pins the leave/rejoin fix: ending a session must
// Flush the backend (device/player layers retain queued audio across a write
// stall and would replay STALE audio when the node rejoins a stream).
func TestDisarmFlushesBackend(t *testing.T) {
	fb := &flushBackend{}
	clk := newFakeClock(true)
	p := New(Config{Backend: fb, Clock: clk, BufferMs: 150, Volume: 1, now: clk.nowNs})
	t.Cleanup(func() { p.Close() })

	p.Reset(1)
	m, _ := clk.MasterNow()
	p.Push(1, 0, m, make([]byte, stream.FrameBytes))
	p.Disarm()
	if fb.count() != 1 {
		t.Fatalf("flushes after Disarm = %d, want 1", fb.count())
	}
	// Idempotent: a second Disarm (already disarmed) must not double-flush.
	p.Disarm()
	if fb.count() != 1 {
		t.Fatalf("flushes after repeat Disarm = %d, want 1", fb.count())
	}
}
