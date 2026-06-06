package codec

import (
	"math"
	"testing"
)

// enc converts a single float32 sample to its little-endian int16 byte pair.
func enc(v float32) (lo, hi byte) {
	dst := make([]byte, 2)
	f32ToS16LE([]float32{v}, dst)
	return dst[0], dst[1]
}

// encVal converts a single float32 sample and returns the int16 value.
func encVal(v float32) int16 {
	lo, hi := enc(v)
	return int16(uint16(lo) | uint16(hi)<<8)
}

func TestF32ToS16LE_Endianness(t *testing.T) {
	// 0.5 → round(0.5*32767+0.5) = round(16383.5+0.5)=16384 → bytes {0x00,0x40}.
	lo, hi := enc(0.5)
	if lo != 0x00 || hi != 0x40 {
		t.Fatalf("0.5: got bytes {%#x,%#x}, want {0x00,0x40}", lo, hi)
	}
	if got := encVal(0.5); got != 16384 {
		t.Fatalf("0.5: got int16 %d, want 16384", got)
	}
}

func TestF32ToS16LE_Clamp(t *testing.T) {
	tests := []struct {
		name string
		in   float32
		want int16
	}{
		// The copied helper uses a SYMMETRIC 32767 scale (verbatim from ../media,
		// P4.3 §3): +1.0 → +32767 (never wraps to +32768) and, symmetrically,
		// -1.0 → -32767 (it does NOT reach the -32768 rail). The decoder uses the
		// full negative rail (1/32768) so an actual -32768 code still decodes to
		// exactly -1.0, but the encoder never emits it. This is the standard
		// audio convention and bounds round-trip error to ~2 LSB near full scale.
		{"high clamp", 1.5, 32767},
		{"low clamp", -1.5, -32767},
		{"full scale +1", 1.0, 32767},
		{"full scale -1", -1.0, -32767},
		{"huge positive", 1e9, 32767},
		{"huge negative", -1e9, -32767},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encVal(tt.in); got != tt.want {
				t.Fatalf("encVal(%v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestF32ToS16LE_RoundToNearest(t *testing.T) {
	// A value just above a code boundary must round up, not truncate down.
	// 1.4/32767 ≈ 4.27e-5; round-to-nearest of 1.4 is 1, truncation also 1 —
	// pick a value whose fractional part exceeds 0.5 to distinguish.
	// v such that v*32767 = 100.7 → round 101, truncate 100.
	v := float32(100.7 / 32767)
	if got := encVal(v); got != 101 {
		t.Fatalf("round-to-nearest: encVal(%v) = %d, want 101 (not truncated 100)", v, got)
	}
	// Negative side: v*32767 = -100.7 → round -101.
	vn := float32(-100.7 / 32767)
	if got := encVal(vn); got != -101 {
		t.Fatalf("round-to-nearest neg: encVal(%v) = %d, want -101", vn, got)
	}
}

// dec converts a single int16 value to float32 via s16LEToF32.
func dec(v int16) float32 {
	u := uint16(v)
	src := []byte{byte(u), byte(u >> 8)}
	dst := make([]float32, 1)
	s16LEToF32(src, dst)
	return dst[0]
}

func TestS16LEToF32_InverseScale(t *testing.T) {
	tests := []struct {
		name string
		in   int16
		want float32
	}{
		{"negative rail", -32768, -1.0},
		{"zero", 0, 0.0},
		{"positive max", 32767, 32767.0 / 32768.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dec(tt.in); got != tt.want {
				t.Fatalf("dec(%d) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
	// Sanity on the +max approximation.
	if got := dec(32767); math.Abs(float64(got)-0.99997) > 1e-4 {
		t.Fatalf("dec(32767) = %v, want ≈0.99997", got)
	}
}

func TestRoundTripFidelity(t *testing.T) {
	// The asymmetric scales (encode 32767, decode 1/32768) are correct and
	// standard (P4.3 §3 / Q4) but compound to at most ~2 LSB near full scale:
	// quantization is ≤0.5 LSB on the 32767 grid, and the 32767-vs-32768 scale
	// mismatch adds up to another LSB at |v|→1. Bound the total to 2 LSB.
	const tol = 2.0 / 32768.0
	const steps = 4096
	for i := 0; i <= steps; i++ {
		v := float32(-1.0 + 2.0*float64(i)/float64(steps))
		dst := make([]byte, 2)
		f32ToS16LE([]float32{v}, dst)
		out := make([]float32, 1)
		s16LEToF32(dst, out)
		if d := math.Abs(float64(out[0] - v)); d > tol+1e-7 {
			t.Fatalf("round-trip v=%v -> %v, err %v exceeds 2 LSB %v", v, out[0], d, tol)
		}
	}
}

func TestConvertAllocs(t *testing.T) {
	src := make([]float32, 960)
	dst := make([]byte, 1920)
	for i := range src {
		src[i] = float32(i%200)/100 - 1
	}
	if n := testing.AllocsPerRun(100, func() { f32ToS16LE(src, dst) }); n != 0 {
		t.Fatalf("f32ToS16LE: %v allocs/op, want 0", n)
	}
	out := make([]float32, 960)
	if n := testing.AllocsPerRun(100, func() { s16LEToF32(dst, out) }); n != 0 {
		t.Fatalf("s16LEToF32: %v allocs/op, want 0", n)
	}
}
