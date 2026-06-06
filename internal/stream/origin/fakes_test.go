package origin

import (
	"context"
	"net"
	"sync"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/fec"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// fakeTimeline is a deterministic Timeline: NowSample returns a fixed anchor.
type fakeTimeline struct {
	sample  int64
	playing bool
}

func (f fakeTimeline) NowSample() (int64, bool, bool) { return f.sample, f.playing, true }

// fakeSource is a deterministic source.Reader: it emits a fixed ramp of frames and
// loops at total. It counts Read calls and the loop point so chunking/loop tests
// are deterministic. Each sample value is its absolute frame index (mod a small
// scale) so the receiver/round-trip can verify ordering.
type fakeSource struct {
	channels int
	total    int   // frames before looping (loops by wrapping pos)
	pos      int64 // absolute frames read (never reset — models the looped content)
	reads    int
}

func (s *fakeSource) Rate() int     { return 48000 }
func (s *fakeSource) Channels() int { return s.channels }
func (s *fakeSource) Close() error  { return nil }

// Read fills dst with whole frames, looping at total. Value = sin-free ramp:
// sample for frame f, channel c = float32((f%total)*channels+c) / 32768 so it
// survives the S16LE round-trip exactly (lands on the int16 grid).
func (s *fakeSource) Read(dst []float32) (int, error) {
	s.reads++
	n := len(dst) - len(dst)%s.channels
	for i := 0; i < n; i += s.channels {
		f := s.pos % int64(s.total)
		for c := 0; c < s.channels; c++ {
			dst[i+c] = float32((f*int64(s.channels)+int64(c))%256) / 32768
		}
		s.pos++
	}
	return n, nil
}

// captureWriter records every Write so fan-out / encode-once can be asserted.
type captureWriter struct {
	mu      sync.Mutex
	writes  [][]byte
	closed  bool
}

func (w *captureWriter) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	w.mu.Lock()
	w.writes = append(w.writes, cp)
	w.mu.Unlock()
	return len(b), nil
}
func (w *captureWriter) Close() error { w.closed = true; return nil }

func (w *captureWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

func (w *captureWriter) headers() []wire.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	hs := make([]wire.Header, 0, len(w.writes))
	for _, b := range w.writes {
		h, _, err := wire.Unmarshal(b)
		if err == nil {
			hs = append(hs, h)
		}
	}
	return hs
}

// countingCodec wraps a real codec and counts Encode calls (fan-out test: encode
// once regardless of listener count, D5).
type countingCodec struct {
	codec.Codec
	encodes int
}

func (c *countingCodec) Encode(pcm []float32) ([]byte, error) {
	c.encodes++
	return c.Codec.Encode(pcm)
}

// countingFEC wraps a real FEC and counts Protect calls.
type countingFEC struct {
	fec.FEC
	protects int
}

func (f *countingFEC) Protect(seq uint64, pkt []byte) [][]byte {
	f.protects++
	return f.FEC.Protect(seq, pkt)
}

// interFrameCodec is a fake inter-frame codec (advertises OPUS) used to exercise
// the join/gen keyframe-forcing path without a real Opus build (risk R3). Encode
// is PCM under the hood; only ID() differs so keyframeFlag treats it as
// non-PCM (forced-keyframe-only).
type interFrameCodec struct {
	codec.Codec
}

func (c interFrameCodec) ID() codec.CodecID { return codec.OPUS }

// stepClock is a controllable monotonic clock for deterministic pacing tests.
type stepClock struct {
	mu sync.Mutex
	t  int64
}

func (c *stepClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *stepClock) advance(ns int64) {
	c.mu.Lock()
	c.t += ns
	c.mu.Unlock()
}

// newTestOrigin builds an Origin wired to capture writers and a controllable clock.
// sleepUntil is replaced with an immediate (no real wait) variant driven by the
// step clock so Run produces deterministically without wall-clock dependence.
func newTestOrigin(c codec.Codec, f fec.FEC, src *fakeSource, cfg testCfg) (*Origin, *stepClock, map[string]*captureWriter) {
	o := New(fakeTimeline{sample: cfg.startIdx}, c, f, src, cfg.Config)
	clk := &stepClock{}
	o.nowMono = clk.now
	caps := map[string]*captureWriter{}
	var capMu sync.Mutex
	o.sender.(*sender).dial = func(addr *net.UDPAddr) (packetWriter, error) {
		w := &captureWriter{}
		capMu.Lock()
		caps[addr.String()] = w
		capMu.Unlock()
		return w, nil
	}
	// sleepUntil advances the step clock to the deadline (collapsing the 10 ms
	// pacing to test speed) but yields a tiny real interval so Run does not spin
	// the CPU producing unbounded chunks faster than the test can observe them.
	o.sleepUntil = func(ctx context.Context, deadline int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if deadline > clk.now() {
			clk.mu.Lock()
			clk.t = deadline
			clk.mu.Unlock()
		}
		t := time.NewTimer(100 * time.Microsecond)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	return o, clk, caps
}

// testCfg bundles Config with test-only anchor.
type testCfg struct {
	Config
	startIdx int64
}
