package source

import (
	"io"
	"math"
	"path/filepath"
	"testing"
)

// TestSeamlessLoop reads well past the clip length and asserts: never io.EOF,
// exact total sample count for N×clip+remainder, and a continuous boundary (no
// zero gap, no duplicated/dropped frame). The clip is a 48000 Hz WAV (resampler
// bypassed) whose length is an integer number of 1 kHz periods, so a clean loop
// reproduces the head sample exactly.
func TestSeamlessLoop(t *testing.T) {
	dir := t.TempDir()
	const rate = 48000
	const clipFrames = 12000 // 0.25 s, 250 whole 1 kHz periods
	samples := sineSamples(clipFrames, rate, 2)
	name := "loop.wav"
	writeWAV(t, filepath.Join(dir, name), samples, rate, 2)

	r, err := openClip(name, dir, rate, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	// Read exactly 2.5 clips worth of frames in whole-frame buffers.
	const loops = 2
	const remainder = clipFrames / 2
	totalFrames := loops*clipFrames + remainder
	got := make([]float32, 0, totalFrames*2)
	buf := make([]float32, 2000) // 1000 frames per read
	for len(got)/2 < totalFrames {
		n, rerr := r.Read(buf[:min(len(buf), (totalFrames-len(got)/2)*2)])
		if rerr == io.EOF {
			t.Fatalf("Read returned io.EOF while looping at frame %d", len(got)/2)
		}
		if rerr != nil {
			t.Fatalf("Read: %v", rerr)
		}
		if n%2 != 0 {
			t.Fatalf("short/odd frame: n=%d", n)
		}
		got = append(got, buf[:n]...)
	}
	if len(got)/2 != totalFrames {
		t.Fatalf("got %d frames want %d", len(got)/2, totalFrames)
	}

	// The decoded clip (16-bit WAV) quantizes the float source; compare the looped
	// stream against itself, period by period: sample[i] ≈ sample[i % clipFrames].
	for f := 0; f < totalFrames; f++ {
		base := (f % clipFrames) * 2
		cur := f * 2
		for c := 0; c < 2; c++ {
			if d := math.Abs(float64(got[cur+c] - got[base+c])); d > 1e-4 {
				t.Fatalf("loop boundary discontinuity at frame %d ch %d: %v vs head %v (Δ=%v)",
					f, c, got[cur+c], got[base+c], d)
			}
		}
	}
}

// TestNoShortFrames pulls odd-sized dst requests that straddle the loop boundary
// and asserts every Read returns a whole-frame multiple (the chunker-facing
// guarantee of 05 §5.3 — no runt slice at the seam).
func TestNoShortFrames(t *testing.T) {
	dir := t.TempDir()
	const rate = 48000
	samples := sineSamples(9000, rate, 2)
	name := "short.flac"
	writeFLAC(t, filepath.Join(dir, name), samples, rate, 2)

	r, err := openClip(name, dir, rate, 2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	// Odd buffer sizes (not multiples of the channel count) over many iterations
	// spanning several loop periods.
	for _, sz := range []int{1, 3, 7, 101, 999, 1234} {
		buf := make([]float32, sz)
		for iter := 0; iter < 200; iter++ {
			n, rerr := r.Read(buf)
			if rerr == io.EOF {
				t.Fatalf("io.EOF while looping (sz=%d)", sz)
			}
			if rerr != nil {
				t.Fatalf("Read sz=%d: %v", sz, rerr)
			}
			if n%2 != 0 {
				t.Fatalf("short frame: sz=%d n=%d (not multiple of 2)", sz, n)
			}
			if n > sz-sz%2 {
				t.Fatalf("returned more than the whole-frame capacity: n=%d sz=%d", n, sz)
			}
		}
	}
}
