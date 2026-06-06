package audio

import (
	"errors"
	"sync"
	"testing"
)

// fakeSink is the test harness AudioSink used by the renderer tests (R1). It
// records every Write so a test can assert the exact sample stream the renderer
// produced, walks a scripted Delay() table so the drift loop can be driven
// through arbitrary error sequences, and can inject backpressure (block Write
// until released) or a write error to exercise recovery paths.
//
// It is a single shared double; methods are mutex-guarded because the renderer
// calls Write from its consumer goroutine while a test inspects the log from the
// outside. Delay() does not block.
type fakeSink struct {
	mu sync.Mutex

	started       bool
	startRate     int
	startChannels int
	startErr      error // returned by Start when set
	startCalls    int

	closed     bool
	closeCalls int

	// writes accumulates every Write call's frames (a copy of each slice) and the
	// running total of samples consumed.
	writes     [][]float32
	written    int   // total float32 samples consumed across all Writes
	writeErr   error // when set, Write returns (0, writeErr)
	shortWrite bool  // when set, Write consumes only one frame per call (backpressure-ish)
	writeCalls int
	writeWidth int // channels, captured at Start (for the multiple-of-channels guard)

	// block, when non-nil, is received-on inside Write (after recording) so a test
	// can gate the consumer goroutine deterministically. Closing the channel (via
	// release) lets all pending and future writes proceed.
	block chan struct{}

	// delays is the scripted Delay() table; each call pops the next entry. When the
	// table is exhausted the last entry repeats. An empty table reports (0,false).
	delays     []delayStep
	delayCalls int
}

// delayStep is one scripted Delay() return value.
type delayStep struct {
	samples int
	ok      bool
}

func (f *fakeSink) Start(rate, channels int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return f.startErr
	}
	if f.started {
		return errors.New("fakeSink: already started")
	}
	f.started = true
	f.startRate = rate
	f.startChannels = channels
	f.writeWidth = channels
	return nil
}

func (f *fakeSink) Write(frames []float32) (int, error) {
	f.mu.Lock()
	f.writeCalls++
	if f.writeErr != nil {
		err := f.writeErr
		f.mu.Unlock()
		return 0, err
	}
	if f.writeWidth > 0 && len(frames)%f.writeWidth != 0 {
		f.mu.Unlock()
		return 0, errors.New("fakeSink: frames not a multiple of channels")
	}
	cp := append([]float32(nil), frames...)
	f.writes = append(f.writes, cp)
	n := len(frames)
	if f.shortWrite && f.writeWidth > 0 && len(frames) >= f.writeWidth {
		n = f.writeWidth // consume a single frame, signalling backpressure
	}
	f.written += n
	block := f.block
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	return n, nil
}

func (f *fakeSink) Delay() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delayCalls++
	if len(f.delays) == 0 {
		return 0, false
	}
	i := f.delayCalls - 1
	if i >= len(f.delays) {
		i = len(f.delays) - 1 // repeat the last scripted step
	}
	s := f.delays[i]
	return s.samples, s.ok
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	f.closed = true
	return nil
}

// release unblocks any Write parked on the backpressure channel and lets future
// writes proceed without blocking.
func (f *fakeSink) release() {
	f.mu.Lock()
	b := f.block
	f.block = nil
	f.mu.Unlock()
	if b != nil {
		close(b)
	}
}

// totalWritten returns the running count of float32 samples consumed.
func (f *fakeSink) totalWritten() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

// concat returns every recorded sample in write order (for assertions on the
// exact stream the renderer produced).
func (f *fakeSink) concat() []float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []float32
	for _, w := range f.writes {
		out = append(out, w...)
	}
	return out
}

// statically assert the fake satisfies the seam its consumers depend on.
var _ AudioSink = (*fakeSink)(nil)

// TestFakeSinkHarness exercises the fake itself so this file compiles and stands
// on its own before R1 wires it into the renderer tests.
func TestFakeSinkHarness(t *testing.T) {
	f := &fakeSink{}
	if err := f.Start(48000, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.Start(48000, 2); err == nil {
		t.Fatalf("second Start should error")
	}
	if f.startRate != 48000 || f.startChannels != 2 {
		t.Fatalf("Start did not record rate/channels: %d/%d", f.startRate, f.startChannels)
	}

	// Default Delay reports no precise figure.
	if s, ok := f.Delay(); s != 0 || ok {
		t.Fatalf("empty Delay table: got (%d,%v), want (0,false)", s, ok)
	}

	// Write records frames and counts samples; multiple-of-channels guard holds.
	n, err := f.Write([]float32{0.1, -0.1, 0.2, -0.2})
	if err != nil || n != 4 {
		t.Fatalf("Write: n=%d err=%v, want n=4", n, err)
	}
	if _, err := f.Write([]float32{0.5}); err == nil {
		t.Fatalf("odd-length Write should error on stereo sink")
	}
	if f.totalWritten() != 4 {
		t.Fatalf("totalWritten=%d, want 4", f.totalWritten())
	}
	if got := f.concat(); len(got) != 4 || got[0] != 0.1 || got[3] != -0.2 {
		t.Fatalf("concat=%v, want the 4 written samples in order", got)
	}

	// Recorded slices are copies: mutating the caller buffer must not change the log.
	buf := []float32{1, 2}
	if _, err := f.Write(buf); err != nil {
		t.Fatalf("Write buf: %v", err)
	}
	buf[0] = 99
	if got := f.concat(); got[4] != 1 {
		t.Fatalf("write log not copied: %v", got)
	}
}

func TestFakeSinkScriptedDelay(t *testing.T) {
	f := &fakeSink{delays: []delayStep{{100, true}, {200, true}, {300, false}}}
	want := []delayStep{{100, true}, {200, true}, {300, false}, {300, false}}
	for i, w := range want {
		s, ok := f.Delay()
		if s != w.samples || ok != w.ok {
			t.Fatalf("Delay call %d: got (%d,%v), want (%d,%v)", i, s, ok, w.samples, w.ok)
		}
	}
}

func TestFakeSinkBackpressureAndErrors(t *testing.T) {
	// Injected write error surfaces to the caller.
	fe := &fakeSink{writeErr: errors.New("boom")}
	if _, err := fe.Write([]float32{0, 0}); err == nil {
		t.Fatalf("Write should surface injected error")
	}

	// shortWrite consumes a single frame per call, modelling backpressure.
	fs := &fakeSink{}
	_ = fs.Start(48000, 2)
	fs.shortWrite = true
	n, err := fs.Write([]float32{0, 0, 1, 1, 2, 2})
	if err != nil || n != 2 {
		t.Fatalf("shortWrite: n=%d err=%v, want n=2", n, err)
	}

	// block parks Write until release(); prove it blocks then unblocks.
	fb := &fakeSink{block: make(chan struct{})}
	_ = fb.Start(48000, 2)
	done := make(chan struct{})
	go func() {
		_, _ = fb.Write([]float32{0, 0})
		close(done)
	}()
	select {
	case <-done:
		t.Fatalf("Write returned before release")
	default:
	}
	fb.release()
	<-done

	// Close is recorded.
	if err := fb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fb.closed || fb.closeCalls != 1 {
		t.Fatalf("Close not recorded: closed=%v calls=%d", fb.closed, fb.closeCalls)
	}
}
