package resampler

import (
	"math"
	"testing"
)

const ppmEps = 1e-9

func TestSetRatioClamp(t *testing.T) {
	lo := 1.0 - MaxPPM*1e-6
	hi := 1.0 + MaxPPM*1e-6
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"above clamps high", 1.001, hi},
		{"below clamps low", 0.5, lo},
		{"unity stays", 1.0, 1.0},
		{"just inside high", hi, hi},
		{"just inside low", lo, lo},
		{"way above", 2.0, hi},
		{"way below", 0.0, lo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResampler(2)
			r.SetRatio(tc.in)
			if got := r.ratio(); math.Abs(got-tc.want) > ppmEps {
				t.Fatalf("ratio() = %.9f, want %.9f", got, tc.want)
			}
		})
	}
}

// observedRatioLength asserts the clamp via the output length over a long buffer.
func TestClampViaOutputLength(t *testing.T) {
	const n = 48000
	in := make([]float32, n) // mono ramp
	for i := range in {
		in[i] = float32(i) / float32(n)
	}
	// Requesting ratio 1.001 must behave as the clamped 1+200ppm: output length
	// must be close to n*(1+200e-6), not n*1.001.
	r := NewResampler(1)
	r.SetRatio(1.001)
	out, consumed := r.Process(nil, in)
	if consumed != n {
		t.Fatalf("consumed = %d, want %d", consumed, n)
	}
	hi := 1.0 + MaxPPM*1e-6
	want := float64(n) * hi
	// 4-tap warmup costs ~2 frames; allow generous slack but reject the unclamped 1.001.
	if math.Abs(float64(len(out))-want) > 8 {
		t.Fatalf("len(out) = %d, want ~%.1f (clamped)", len(out), want)
	}
	unclamped := float64(n) * 1.001
	if math.Abs(float64(len(out))-unclamped) < 8 {
		t.Fatalf("len(out) = %d matches UNCLAMPED %.1f; clamp not applied", len(out), unclamped)
	}
}

func TestNearUnityLengthAndBounds(t *testing.T) {
	const n = 48000
	in := make([]float32, n)
	for i := range in {
		in[i] = float32(i) / float32(n) // monotonic ramp 0..~1
	}
	r := NewResampler(1)
	r.SetRatio(1.0)
	out, consumed := r.Process(nil, in)
	if consumed != n {
		t.Fatalf("consumed = %d, want %d", consumed, n)
	}
	// Length within a few frames of n (warmup latency only).
	if d := int(math.Abs(float64(len(out) - n))); d > 4 {
		t.Fatalf("len(out) = %d, want ~%d (|diff|=%d)", len(out), n, d)
	}
	prev := float32(math.Inf(-1))
	for i, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("out[%d] = %v non-finite", i, v)
		}
		if v < -1.0001 || v > 1.0001 {
			t.Fatalf("out[%d] = %v out of [-1,1]", i, v)
		}
		// Catmull-Rom over a strict ramp is monotonic non-decreasing.
		if v < prev-1e-6 {
			t.Fatalf("out not monotonic at %d: %v < %v", i, v, prev)
		}
		prev = v
	}
}

func TestDCPassthrough(t *testing.T) {
	const n = 4096
	const dc = float32(0.37)
	in := make([]float32, n*2) // stereo
	for i := range in {
		in[i] = dc
	}
	for _, ratio := range []float64{1.0, 1.0 + MaxPPM*1e-6, 1.0 - MaxPPM*1e-6} {
		r := NewResampler(2)
		r.SetRatio(ratio)
		out, _ := r.Process(nil, in)
		if len(out) == 0 {
			t.Fatalf("ratio %.6f: no output", ratio)
		}
		for i, v := range out {
			if math.Abs(float64(v-dc)) > 1e-5 {
				t.Fatalf("ratio %.6f: out[%d] = %v, want DC %v", ratio, i, v, dc)
			}
		}
	}
}

func TestToneLengthShiftAndBounds(t *testing.T) {
	const n = 48000
	const f = 1000.0
	const sr = 48000.0
	in := make([]float32, n)
	for i := range in {
		in[i] = 0.5 * float32(math.Sin(2*math.Pi*f*float64(i)/sr))
	}
	r := NewResampler(1)
	hi := 1.0 + MaxPPM*1e-6
	r.SetRatio(hi)
	out, _ := r.Process(nil, in)
	want := float64(n) * hi
	if math.Abs(float64(len(out))-want) > 8 {
		t.Fatalf("len(out) = %d, want ~%.1f (~+200ppm)", len(out), want)
	}
	for i, v := range out {
		if math.IsNaN(float64(v)) || math.Abs(float64(v)) > 1.0 {
			t.Fatalf("out[%d] = %v unbounded", i, v)
		}
	}
	// Loose THD/energy sanity: RMS of the resampled tone stays near the input RMS.
	var sin, sout float64
	for _, v := range in {
		sin += float64(v) * float64(v)
	}
	for _, v := range out {
		sout += float64(v) * float64(v)
	}
	rin := math.Sqrt(sin / float64(len(in)))
	rout := math.Sqrt(sout / float64(len(out)))
	if math.Abs(rout-rin) > 0.02 {
		t.Fatalf("RMS drift: in=%.4f out=%.4f", rin, rout)
	}
}

func TestBlockBoundarySeam(t *testing.T) {
	const n = 10000
	in := make([]float32, n*2) // stereo
	for i := 0; i < n; i++ {
		in[2*i] = 0.5 * float32(math.Sin(2*math.Pi*440*float64(i)/48000))
		in[2*i+1] = 0.5 * float32(math.Cos(2*math.Pi*660*float64(i)/48000))
	}
	const ratio = 1.0 + 137e-6

	// One call.
	r1 := NewResampler(2)
	r1.SetRatio(ratio)
	whole, c1 := r1.Process(nil, in)
	if c1 != len(in) {
		t.Fatalf("single-call consumed %d, want %d", c1, len(in))
	}

	// Two halves; the split is on a frame boundary (even sample index).
	r2 := NewResampler(2)
	r2.SetRatio(ratio)
	half := (n / 3) * 2 // frame-aligned split point in samples
	part, ca := r2.Process(nil, in[:half])
	part, cb := r2.Process(part, in[half:])
	if ca+cb != len(in) {
		t.Fatalf("split consumed %d+%d=%d, want %d", ca, cb, ca+cb, len(in))
	}

	if len(whole) != len(part) {
		t.Fatalf("length mismatch: whole=%d split=%d", len(whole), len(part))
	}
	for i := range whole {
		if math.Abs(float64(whole[i]-part[i])) > 1e-5 {
			t.Fatalf("seam diff at %d: whole=%v split=%v", i, whole[i], part[i])
		}
	}
}

func TestResetClearsState(t *testing.T) {
	in := make([]float32, 200)
	for i := range in {
		in[i] = float32(i)
	}
	r := NewResampler(1)
	r.SetRatio(1.0)
	out1, _ := r.Process(nil, in)
	r.Reset()
	if r.nhist != 0 || r.phase != 0 {
		t.Fatalf("after Reset nhist=%d phase=%v, want 0,0", r.nhist, r.phase)
	}
	out2, _ := r.Process(nil, in)
	if len(out1) != len(out2) {
		t.Fatalf("post-reset length differs: %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i] != out2[i] {
			t.Fatalf("post-reset value differs at %d: %v vs %v", i, out1[i], out2[i])
		}
	}
}

func TestShortInputCarriesState(t *testing.T) {
	// Feeding one frame at a time must produce the same output as one big call.
	const n = 1000
	in := make([]float32, n)
	for i := range in {
		in[i] = 0.3 * float32(math.Sin(float64(i)/13.0))
	}
	const ratio = 1.0 - 90e-6

	rWhole := NewResampler(1)
	rWhole.SetRatio(ratio)
	whole, _ := rWhole.Process(nil, in)

	rDrip := NewResampler(1)
	rDrip.SetRatio(ratio)
	var drip []float32
	total := 0
	for i := 0; i < n; i++ {
		var c int
		drip, c = rDrip.Process(drip, in[i:i+1])
		total += c
	}
	if total != n {
		t.Fatalf("drip consumed %d, want %d", total, n)
	}
	if len(whole) != len(drip) {
		t.Fatalf("drip length mismatch: whole=%d drip=%d", len(whole), len(drip))
	}
	for i := range whole {
		if math.Abs(float64(whole[i]-drip[i])) > 1e-5 {
			t.Fatalf("drip diff at %d: %v vs %v", i, whole[i], drip[i])
		}
	}
}

func TestProcessRejectsBadInput(t *testing.T) {
	r := NewResampler(2)
	r.SetRatio(1.0)
	out, c := r.Process(nil, []float32{0.1}) // less than one frame
	if c != 0 || len(out) != 0 {
		t.Fatalf("partial frame: got consumed=%d len=%d, want 0,0", c, len(out))
	}
}
