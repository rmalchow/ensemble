package sink

import (
	"encoding/binary"
	"math"
	"testing"

	"ensemble/internal/stream"
)

// makeFrame builds a canonical PCM frame from a per-sample generator (L,R).
func makeFrame(gen func(i int) (int16, int16)) []byte {
	f := make([]byte, stream.FrameBytes)
	for i := 0; i < stream.FrameSamples; i++ {
		l, r := gen(i)
		off := i * stream.Channels * stream.BytesPerSmpl
		binary.LittleEndian.PutUint16(f[off:off+2], uint16(l))
		binary.LittleEndian.PutUint16(f[off+2:off+4], uint16(r))
	}
	return f
}

func sampleLR(frame []byte, i int) (int16, int16) {
	off := i * stream.Channels * stream.BytesPerSmpl
	l := int16(binary.LittleEndian.Uint16(frame[off : off+2]))
	r := int16(binary.LittleEndian.Uint16(frame[off+2 : off+4]))
	return l, r
}

func TestResamplerUnitRateDelayedIdentity(t *testing.T) {
	// At rate 1 the resampler is a pure 1-frame delay: output frame f equals
	// input frame f-1 (frame 0 is silence). The one-frame lookahead is what
	// gives every seam a real 4-tap window.
	r := newResampler()
	r.setRate(0)
	var frames [][]byte
	for f := 0; f < 5; f++ {
		base := f * stream.FrameSamples
		frames = append(frames, makeFrame(func(i int) (int16, int16) {
			v := int16((base + i) % 1000)
			return v, -v
		}))
	}
	var outs [][]byte
	for _, in := range frames {
		o := make([]byte, stream.FrameBytes)
		copy(o, r.process(in))
		outs = append(outs, o)
	}
	for i := 0; i < stream.FrameBytes; i++ {
		if outs[0][i] != 0 {
			t.Fatalf("frame 0 must be silence; byte %d = %d", i, outs[0][i])
		}
	}
	for f := 1; f < 5; f++ {
		for i := 0; i < stream.FrameSamples; i++ {
			il, ir := sampleLR(frames[f-1], i)
			ol, or := sampleLR(outs[f], i)
			if il != ol || ir != or {
				t.Fatalf("frame %d sample %d: want prev-frame (%d,%d) got (%d,%d)", f, i, il, ir, ol, or)
			}
		}
	}
}

func TestResamplerOutputFrameSize(t *testing.T) {
	r := newResampler()
	r.setRate(300)
	in := makeFrame(func(i int) (int16, int16) { return int16(i), int16(i) })
	out := r.process(in)
	if len(out) != stream.FrameBytes {
		t.Fatalf("output %d bytes, want %d", len(out), stream.FrameBytes)
	}
}

func TestResamplerNoSeamGlitch(t *testing.T) {
	// THE regression test for the 50 Hz seam buzz: with the servo active
	// (rate ≠ 1), the OLD resampler clamped p2/p3 at every frame end (no
	// lookahead), kinking the waveform once per 20 ms frame. Reconstruct the
	// full output of a continuous 1 kHz sine and assert the curvature (second
	// difference) has NO spike at the frame boundaries — a clean resample has
	// smooth curvature everywhere.
	r := newResampler()
	r.setRate(250) // servo-class correction → non-trivial fractional cursor
	freq, amp := 1000.0, 9000.0
	const frames = 12
	var out []int16
	for f := 0; f < frames; f++ {
		base := f * stream.FrameSamples
		in := makeFrame(func(i int) (int16, int16) {
			v := int16(amp * math.Sin(2*math.Pi*freq*float64(base+i)/float64(stream.SampleRate)))
			return v, v
		})
		o := r.process(in)
		for i := 0; i < stream.FrameSamples; i++ {
			l, _ := sampleLR(o, i)
			out = append(out, l)
		}
	}
	// Skip the first 2 frames (silence + warmup seam). Curvature = x[k+1]-2x[k]+x[k-1].
	// For a 1 kHz sine at 48 k the per-sample curvature peaks at amp*(2πf/Fs)^2 ≈ 154.
	start := 2 * stream.FrameSamples
	var maxCurv float64
	for k := start + 1; k < len(out)-1; k++ {
		c := math.Abs(float64(out[k+1]) - 2*float64(out[k]) + float64(out[k-1]))
		if c > maxCurv {
			maxCurv = c
		}
	}
	// A clean sine's curvature is ~154; a seam clamp spikes it into the
	// thousands. Allow generous headroom for interpolation/quantization.
	if maxCurv > 400 {
		t.Fatalf("curvature spike %.0f (clean sine ~154) — seam glitch present", maxCurv)
	}
}

func TestResamplerSilenceStaysSilence(t *testing.T) {
	r := newResampler()
	r.setRate(200)
	silence := make([]byte, stream.FrameBytes)
	for f := 0; f < 3; f++ {
		out := r.process(silence)
		for i := 0; i < stream.FrameBytes; i++ {
			if out[i] != 0 {
				t.Fatalf("frame %d byte %d non-zero: %d", f, i, out[i])
			}
		}
	}
}

func TestResamplerStereoIndependence(t *testing.T) {
	r := newResampler()
	r.setRate(0) // unity → 1-frame-delayed identity
	in := makeFrame(func(i int) (int16, int16) {
		return int16(i % 500), int16(-(i % 500))
	})
	r.process(in)        // frame 0 → silence
	out := r.process(in) // frame 1 → frame 0 (this same input) delayed
	for i := 0; i < stream.FrameSamples; i++ {
		ol, or := sampleLR(out, i)
		if ol != int16(i%500) || or != int16(-(i%500)) {
			t.Fatalf("sample %d L/R cross-contaminated: (%d,%d)", i, ol, or)
		}
	}
}

func TestResamplerCursorBounded(t *testing.T) {
	// Over many frames at a sustained correction the cursor must stay bounded
	// (the leftover fraction is kept; whole input samples are discarded). A
	// run-away cursor would mean unbounded carry growth / precision loss.
	r := newResampler()
	r.setRate(400)
	in := makeFrame(func(i int) (int16, int16) { return int16(i % 777), int16(i % 777) })
	for f := 0; f < 2000; f++ {
		r.process(in)
		if r.cursor < 0 || r.cursor >= float64(leadPad)+2 {
			t.Fatalf("frame %d: cursor %.4f left bounded range", f, r.cursor)
		}
		for ch := 0; ch < stream.Channels; ch++ {
			if len(r.carry[ch]) > 2*stream.FrameSamples+leadPad+8 {
				t.Fatalf("frame %d ch %d: carry grew to %d", f, ch, len(r.carry[ch]))
			}
		}
	}
}
