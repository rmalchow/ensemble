package source

import (
	"io"
	"math"
	"testing"
)

// fakeFinite is a finite in-memory frameSource of interleaved float32 used to
// exercise the resampler in isolation (no decode/IO).
type fakeFinite struct {
	data   []float32
	rateHz int
	chans  int
	off    int
}

func (f *fakeFinite) rate() int     { return f.rateHz }
func (f *fakeFinite) channels() int { return f.chans }
func (f *fakeFinite) read(dst []float32) (int, error) {
	if f.off >= len(f.data) {
		return 0, io.EOF
	}
	want := len(dst) - len(dst)%f.chans
	n := copy(dst[:want], f.data[f.off:])
	f.off += n
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}
func (f *fakeFinite) seekStart() error { f.off = 0; return nil }
func (f *fakeFinite) close() error     { return nil }

func TestResampleRatio(t *testing.T) {
	tests := []struct {
		name   string
		src    int
		bypass bool
	}{
		{"44100->48000", 44100, false},
		{"48000->48000 bypass", 48000, true},
		{"22050->48000", 22050, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const inFrames = 4096
			in := sineSamples(inFrames, tc.src, 2)
			fs := &fakeFinite{data: append([]float32(nil), in...), rateHz: tc.src, chans: 2}

			if tc.bypass {
				// Open's bypass path uses the raw source; assert sample-identity.
				out := drainAll(t, fs, 2)
				if len(out) != len(in) {
					t.Fatalf("bypass length %d want %d", len(out), len(in))
				}
				for i := range out {
					if out[i] != in[i] {
						t.Fatalf("bypass not sample-identical at %d: %v != %v", i, out[i], in[i])
					}
				}
				return
			}

			rs := newResampler(fs, canonRate)
			if rs.rate() != canonRate || rs.channels() != 2 {
				t.Fatalf("resampler reports %d/%d", rs.rate(), rs.channels())
			}
			out := drainAll(t, rs, 2)
			outFrames := len(out) / 2

			// Output frame count ≈ in * dst/src within a couple frames of slack
			// (the kernel needs a few input frames of lead-in).
			want := int(math.Round(float64(inFrames) * float64(canonRate) / float64(tc.src)))
			if d := outFrames - want; d < -4 || d > 4 {
				t.Errorf("frames=%d want≈%d (Δ=%d)", outFrames, want, d)
			}
			for _, v := range out {
				if v < -1.001 || v > 1.001 || math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					t.Fatalf("bad resampled sample %v", v)
				}
			}
		})
	}
}

// TestResampleStateAcrossReads verifies the resampler phase is continuous when
// pulled in small blocks vs. one big read (state survives multiple read calls).
func TestResampleStateAcrossReads(t *testing.T) {
	const inFrames = 8192
	in := sineSamples(inFrames, 44100, 2)

	big := drainAll(t, newResampler(&fakeFinite{data: append([]float32(nil), in...), rateHz: 44100, chans: 2}, canonRate), 2)

	rs := newResampler(&fakeFinite{data: append([]float32(nil), in...), rateHz: 44100, chans: 2}, canonRate)
	var small []float32
	buf := make([]float32, 130) // odd, not a multiple of 2*kernel
	for {
		n, err := rs.read(buf)
		small = append(small, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n == 0 {
			break
		}
	}
	if len(small) != len(big) {
		t.Fatalf("block-read length %d != single-read length %d", len(small), len(big))
	}
	for i := range big {
		if math.Abs(float64(big[i]-small[i])) > 1e-6 {
			t.Fatalf("phase discontinuity at %d: %v vs %v", i, big[i], small[i])
		}
	}
}

// drainAll reads a frameSource to EOF and returns all samples.
func drainAll(t *testing.T, fs frameSource, ch int) []float32 {
	t.Helper()
	var out []float32
	buf := make([]float32, 1024*ch)
	for {
		n, err := fs.read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n == 0 {
			break
		}
	}
	return out
}
