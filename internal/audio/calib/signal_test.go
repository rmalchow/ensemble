package calib

import (
	"math"
	"testing"
)

// signal_test.go asserts the A.10b waveform shape (click position/amplitude, tone
// frequency/length, silence span), determinism, period-wrap, and channel fan-out.

// frame returns the per-channel values at period frame f.
func frameAt(s *Signal, f int) []float32 {
	base := f * s.channels
	return s.period[base : base+s.channels]
}

func TestDefaultSignalShape(t *testing.T) {
	s := NewSignal(DefaultSignalParams())

	if s.Rate() != 48000 || s.Channels() != 2 {
		t.Fatalf("rate/channels = %d/%d, want 48000/2", s.Rate(), s.Channels())
	}
	if s.PeriodFrames() != 48000 {
		t.Fatalf("PeriodFrames = %d, want 48000 (1 s)", s.PeriodFrames())
	}
	if len(s.Period()) != 48000*2 {
		t.Fatalf("len(Period) = %d, want %d", len(s.Period()), 48000*2)
	}

	const clickFrames = 48  // 1 ms @ 48k
	const toneFrames = 9600 // 200 ms @ 48k
	const toneStart = clickFrames
	const toneEnd = clickFrames + toneFrames // 9648

	// Click: frame 0 == +1.0 on every channel; the rest of the click window == 0.
	for ch, v := range frameAt(s, 0) {
		if v != 1.0 {
			t.Fatalf("click frame 0 ch %d = %v, want +1.0", ch, v)
		}
	}
	for f := 1; f < clickFrames; f++ {
		for ch, v := range frameAt(s, f) {
			if v != 0 {
				t.Fatalf("click window frame %d ch %d = %v, want 0", f, ch, v)
			}
		}
	}

	// Tone: amplitude peak ~Amp, and the right number of zero crossings for 1 kHz
	// over 200 ms (~400 crossings = 2 per cycle * 1000 Hz * 0.2 s).
	var peak float32
	crossings := 0
	prev := float32(0)
	for f := toneStart; f < toneEnd; f++ {
		v := frameAt(s, f)[0]
		// fan-out: both channels equal.
		if frameAt(s, f)[1] != v {
			t.Fatalf("tone frame %d channels differ: %v vs %v", f, v, frameAt(s, f)[1])
		}
		if abs32(v) > peak {
			peak = abs32(v)
		}
		if (prev < 0 && v >= 0) || (prev >= 0 && v < 0) {
			crossings++
		}
		prev = v
	}
	if math.Abs(float64(peak)-defaultAmp) > 0.01 {
		t.Fatalf("tone peak = %v, want ~%v", peak, defaultAmp)
	}
	// First sample of the tone is sin(0)=0; allow ±2 crossings of slack.
	if crossings < 398 || crossings > 402 {
		t.Fatalf("tone zero crossings = %d, want ~400 (1 kHz over 200 ms)", crossings)
	}

	// Silence: everything after the tone is 0.
	for f := toneEnd; f < s.PeriodFrames(); f++ {
		for ch, v := range frameAt(s, f) {
			if v != 0 {
				t.Fatalf("silence frame %d ch %d = %v, want 0", f, ch, v)
			}
		}
	}
}

func TestSignalDeterministic(t *testing.T) {
	a := NewSignal(DefaultSignalParams())
	b := NewSignal(DefaultSignalParams())
	pa, pb := a.Period(), b.Period()
	if len(pa) != len(pb) {
		t.Fatalf("period lengths differ: %d vs %d", len(pa), len(pb))
	}
	for i := range pa {
		if pa[i] != pb[i] {
			t.Fatalf("period sample %d differs: %v vs %v (must be bit-identical)", i, pa[i], pb[i])
		}
	}
}

func TestFillCrossNodeIdentity(t *testing.T) {
	// Two independent "nodes" filling at the same group sample must agree byte-for-byte.
	a := NewSignal(DefaultSignalParams())
	b := NewSignal(DefaultSignalParams())
	const S = int64(123456)
	da := make([]float32, 4096)
	db := make([]float32, 4096)
	a.Fill(da, S)
	b.Fill(db, S)
	for i := range da {
		if da[i] != db[i] {
			t.Fatalf("Fill sample %d differs across nodes: %v vs %v", i, da[i], db[i])
		}
	}
}

func TestFillMatchesPeriodAndWraps(t *testing.T) {
	s := NewSignal(DefaultSignalParams())
	ch := s.Channels()
	pf := s.PeriodFrames()

	// A read straddling the period boundary must wrap to frame 0 (the click).
	startFrame := int64(pf - 2) // last 2 frames, then wrap into the next period
	frames := 6
	dst := make([]float32, frames*ch)
	s.Fill(dst, startFrame)

	for i := 0; i < frames; i++ {
		wantFrame := int((startFrame + int64(i)) % int64(pf))
		want := frameAt(s, wantFrame)
		got := dst[i*ch : (i+1)*ch]
		for c := 0; c < ch; c++ {
			if got[c] != want[c] {
				t.Fatalf("Fill frame %d ch %d = %v, want %v (period frame %d)", i, c, got[c], want[c], wantFrame)
			}
		}
	}
	// Frame at i=2 wraps to period frame 0, which must be the +1.0 click.
	if dst[2*ch] != 1.0 {
		t.Fatalf("wrapped frame = %v, want the +1.0 click at period start", dst[2*ch])
	}
}

func TestFillOffsetZero(t *testing.T) {
	// fromSample==0 must reproduce the period prefix exactly (click at frame 0).
	s := NewSignal(DefaultSignalParams())
	ch := s.Channels()
	dst := make([]float32, 100*ch)
	s.Fill(dst, 0)
	for i := range dst {
		if dst[i] != s.Period()[i] {
			t.Fatalf("Fill(0) sample %d = %v, want %v", i, dst[i], s.Period()[i])
		}
	}
}

func TestFillPanicsOnBadLength(t *testing.T) {
	s := NewSignal(DefaultSignalParams()) // 2 channels
	defer func() {
		if recover() == nil {
			t.Fatalf("Fill with non-multiple-of-Channels dst should panic")
		}
	}()
	s.Fill(make([]float32, 3), 0)
}

func TestNewSignalFillsDefaults(t *testing.T) {
	// Zero params => all defaults.
	s := NewSignal(SignalParams{})
	if s.Rate() != defaultRate || s.Channels() != defaultChannels {
		t.Fatalf("defaults not filled: rate=%d ch=%d", s.Rate(), s.Channels())
	}
	if s.clickFrames != 48 || s.toneFrames != 9600 {
		t.Fatalf("derived frames = %d/%d, want 48/9600", s.clickFrames, s.toneFrames)
	}
}

func TestNewSignalMonoFanout(t *testing.T) {
	// A single-channel signal still produces a valid click+tone.
	s := NewSignal(SignalParams{Channels: 1})
	if s.Channels() != 1 {
		t.Fatalf("channels = %d, want 1", s.Channels())
	}
	if s.Period()[0] != 1.0 {
		t.Fatalf("mono click = %v, want 1.0", s.Period()[0])
	}
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
