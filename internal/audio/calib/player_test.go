package calib

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// player_test.go drives CalibratePlayer against a fake sink + a scripted fake
// Timeline: alignment, exact duration, busy/no-sink/solo paths, stop/ctx cancel.

// fakeSink records every Write so a test can assert the exact stream; it can
// inject backpressure (short writes) and a write error.
type fakeSink struct {
	mu         sync.Mutex
	channels   int
	written    int // total float32 samples consumed
	writes     []float32
	writeErr   error
	shortWrite bool
}

func (f *fakeSink) Start(rate, channels int) error { f.channels = channels; return nil }

func (f *fakeSink) Write(frames []float32) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	n := len(frames)
	if f.shortWrite && f.channels > 0 && len(frames) >= f.channels {
		n = f.channels // one frame per call
	}
	f.writes = append(f.writes, frames[:n]...)
	f.written += n
	return n, nil
}

func (f *fakeSink) Delay() (int, bool) { return 0, false }
func (f *fakeSink) Close() error       { return nil }

func (f *fakeSink) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

func (f *fakeSink) snapshot() []float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float32(nil), f.writes...)
}

// fakeTimeline returns a sample that advances by `step` each NowSample call,
// starting at `start`. ok controls the synced flag.
type fakeTimeline struct {
	mu      sync.Mutex
	cur     int64
	step    int64
	ok      bool
	playing bool
}

func (t *fakeTimeline) NowSample() (int64, bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.cur
	t.cur += t.step
	return s, t.playing, t.ok
}

func TestPlayNoSink(t *testing.T) {
	p := NewCalibratePlayer(NewSignal(DefaultSignalParams()), nil, nil)
	if err := p.Play(context.Background(), 0, 1); !errors.Is(err, ErrNoSink) {
		t.Fatalf("Play with nil sink = %v, want ErrNoSink", err)
	}
}

func TestPlaySoloExactDuration(t *testing.T) {
	// No timeline => starts immediately; writes exactly durationSec*Rate frames.
	sig := NewSignal(SignalParams{Rate: 4800, Channels: 2}) // small rate => fast test
	snk := &fakeSink{}
	_ = snk.Start(sig.Rate(), sig.Channels())
	p := NewCalibratePlayer(sig, snk, nil)

	if err := p.Play(context.Background(), 0, 2); err != nil {
		t.Fatalf("Play: %v", err)
	}
	wantSamples := 2 * sig.Rate() * sig.Channels()
	if snk.total() != wantSamples {
		t.Fatalf("written = %d samples, want %d", snk.total(), wantSamples)
	}
	// The stream must equal Signal.Fill from sample 0 (alignment/content check).
	want := make([]float32, wantSamples)
	sig.Fill(want, 0)
	got := snk.snapshot()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPlayWaitsForStartSample(t *testing.T) {
	// Timeline starts below startSample and advances; Play must not write until the
	// timeline reaches startSample, then the content is aligned to the group sample.
	sig := NewSignal(SignalParams{Rate: 4800, Channels: 2})
	snk := &fakeSink{}
	_ = snk.Start(sig.Rate(), sig.Channels())
	const startSample = int64(1000)
	tl := &fakeTimeline{cur: 0, step: 600, ok: true, playing: true} // crosses 1000 after ~2 polls
	p := NewCalibratePlayer(sig, snk, tl)

	if err := p.Play(context.Background(), startSample, 1); err != nil {
		t.Fatalf("Play: %v", err)
	}
	// Wrote exactly 1 s.
	if snk.total() != sig.Rate()*sig.Channels() {
		t.Fatalf("written = %d, want %d", snk.total(), sig.Rate()*sig.Channels())
	}
	// First frame must be the period content at the group sample we actually
	// started at (>= startSample). The timeline returned 1200 on the crossing poll,
	// so the first fill is from sample 1200.
	want := make([]float32, sig.Channels())
	sig.Fill(want, 1200)
	got := snk.snapshot()[:sig.Channels()]
	for c := range want {
		if got[c] != want[c] {
			t.Fatalf("aligned first frame ch %d = %v, want %v (Fill@1200)", c, got[c], want[c])
		}
	}
}

func TestPlayBusy(t *testing.T) {
	sig := NewSignal(SignalParams{Rate: 48000, Channels: 2})
	snk := &fakeSink{shortWrite: true}
	_ = snk.Start(sig.Rate(), sig.Channels())
	p := NewCalibratePlayer(sig, snk, nil)

	errc := make(chan error, 1)
	go func() { errc <- p.Play(context.Background(), 0, 30) }()

	// Wait until the first Play marks itself running.
	deadline := time.After(2 * time.Second)
	for {
		p.mu.Lock()
		running := p.running
		p.mu.Unlock()
		if running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first Play never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	if err := p.Play(context.Background(), 0, 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent Play = %v, want ErrBusy", err)
	}
	p.Stop()
	<-errc
}

func TestPlayStop(t *testing.T) {
	sig := NewSignal(SignalParams{Rate: 48000, Channels: 2})
	snk := &fakeSink{shortWrite: true} // slows the drain so Stop lands mid-play
	_ = snk.Start(sig.Rate(), sig.Channels())
	p := NewCalibratePlayer(sig, snk, nil)

	errc := make(chan error, 1)
	go func() { errc <- p.Play(context.Background(), 0, 600) }() // 10 min nominal

	time.Sleep(10 * time.Millisecond)
	p.Stop()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Play after Stop = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not end Play")
	}
}

func TestPlayCtxCancel(t *testing.T) {
	sig := NewSignal(SignalParams{Rate: 48000, Channels: 2})
	snk := &fakeSink{shortWrite: true}
	_ = snk.Start(sig.Rate(), sig.Channels())
	p := NewCalibratePlayer(sig, snk, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.Play(ctx, 0, 600) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Play after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not end Play")
	}
}

func TestPlayWriteError(t *testing.T) {
	sig := NewSignal(SignalParams{Rate: 4800, Channels: 2})
	boom := errors.New("boom")
	snk := &fakeSink{writeErr: boom}
	_ = snk.Start(sig.Rate(), sig.Channels())
	p := NewCalibratePlayer(sig, snk, nil)
	if err := p.Play(context.Background(), 0, 1); !errors.Is(err, boom) {
		t.Fatalf("Play = %v, want injected write error", err)
	}
}
