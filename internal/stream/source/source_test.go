package source

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const (
	canonRate = 48000
	canonCh   = 2
)

// openClip resolves name under dir (URLs pass through) and opens it. It mirrors
// how cmd/web turn a ConfigDoc Media.File value into the Open path argument.
func openClip(name, dir string, rate, ch int) (Reader, error) {
	p, err := ResolveDataPath(name, dir)
	if err != nil {
		return nil, err
	}
	return Open(p, rate, ch)
}

// makeClips writes WAV+FLAC sine fixtures at the given rate into dir and returns
// their data/-relative names. mp3 fixtures are committed under testdata/.
func makeClips(t *testing.T, dir string, rate int) (wav, flac string) {
	t.Helper()
	frames := rate / 4 // 0.25 s
	samples := sineSamples(frames, rate, 2)
	wav = "sine.wav"
	flac = "sine.flac"
	writeWAV(t, filepath.Join(dir, wav), samples, rate, 2)
	writeFLAC(t, filepath.Join(dir, flac), samples, rate, 2)
	return wav, flac
}

func TestOpenDispatch(t *testing.T) {
	dir := t.TempDir()
	wav44, flac44 := makeClips(t, dir, 44100)
	wav48, flac48 := makeClips48(t, dir)

	// Copy committed mp3 fixtures into the data dir (by ext and exercised by magic too).
	mp344 := copyFixture(t, dir, "sine_44100.mp3", "tone44.mp3")
	mp348 := copyFixture(t, dir, "sine_48000.mp3", "tone48.mp3")
	// A file with a .bin extension but valid FLAC magic — magic must win.
	magicOnly := copyData(t, dir, filepath.Join(dir, flac44), "blob.bin")

	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"wav44", wav44, true},
		{"wav48", wav48, true},
		{"flac44", flac44, true},
		{"flac48", flac48, true},
		{"mp3_44", mp344, true},
		{"mp3_48", mp348, true},
		{"magic-over-ext", magicOnly, true},
		{"missing", "nope.wav", false},
		{"unknown-ext", writeBlob(t, dir, "x.txt", []byte("not media")), false},
		{"truncated-flac", truncate(t, dir, flac44, "trunc.flac"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := openClip(tc.path, dir, canonRate, canonCh)
			if tc.ok {
				if err != nil {
					t.Fatalf("Open(%q): unexpected error %v", tc.path, err)
				}
				defer r.Close()
				if r.Rate() != canonRate {
					t.Errorf("Rate()=%d want %d", r.Rate(), canonRate)
				}
				if r.Channels() != canonCh {
					t.Errorf("Channels()=%d want %d", r.Channels(), canonCh)
				}
				// Pull one buffer to confirm decode actually runs.
				buf := make([]float32, 960)
				if n, rerr := r.Read(buf); rerr != nil || n == 0 {
					t.Errorf("Read: n=%d err=%v", n, rerr)
				}
			} else if err == nil {
				r.Close()
				t.Fatalf("Open(%q): expected error, got nil", tc.path)
			}
		})
	}
}

func TestDecodeBounds(t *testing.T) {
	dir := t.TempDir()
	wav, flac := makeClips(t, dir, 44100)
	mp3 := copyFixture(t, dir, "sine_44100.mp3", "tone.mp3")
	floatWav := "float.wav"
	writeFloatWAV(t, filepath.Join(dir, floatWav), sineSamples(11025, 44100, 2), 44100, 2)

	for _, tc := range []struct{ name, path string }{
		{"wav", wav}, {"flac", flac}, {"mp3", mp3}, {"floatwav", floatWav},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := openClip(tc.path, dir, canonRate, canonCh)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer r.Close()
			buf := make([]float32, 4801) // deliberately not a multiple of channels
			var sumSq, peak float64
			var count int
			// Read about one clip length (the 0.25 s clips are ~12000 frames at
			// 48k); stop well before a loop so leading codec priming silence on a
			// second pass cannot skew the RMS window.
			const frameBudget = 9000
			frames := 0
			for frames < frameBudget {
				n, rerr := r.Read(buf)
				if rerr != nil && rerr != io.EOF {
					t.Fatalf("Read: %v", rerr)
				}
				if n%canonCh != 0 {
					t.Fatalf("n=%d not a multiple of channels %d", n, canonCh)
				}
				frames += n / canonCh
				for i := 0; i < n; i++ {
					v := float64(buf[i])
					if v < -1.001 || v > 1.001 {
						t.Fatalf("sample %v out of [-1,1]", v)
					}
					if math.IsNaN(v) || math.IsInf(v, 0) {
						t.Fatalf("non-finite sample %v", v)
					}
					if a := math.Abs(v); a > peak {
						peak = a
					}
					// Skip near-silent samples (mp3 encoder priming/padding) from
					// the RMS window so the tone level is measured, not the gaps.
					if math.Abs(v) > 0.05 {
						sumSq += v * v
						count++
					}
				}
				if n == 0 {
					break
				}
			}
			if count == 0 {
				t.Fatal("decoded no tone samples")
			}
			// A 0.5-amplitude sine peaks at ~0.5 and has RMS ~0.354 over its
			// active region; lossy mp3 widens the band.
			rms := math.Sqrt(sumSq / float64(count))
			if rms < 0.25 || rms > 0.45 {
				t.Errorf("active RMS %.3f out of expected [0.25,0.45] for a 0.5 sine", rms)
			}
			if peak < 0.35 || peak > 0.7 {
				t.Errorf("peak %.3f out of expected [0.35,0.7] for a 0.5 sine", peak)
			}
		})
	}
}

func TestCloseReleases(t *testing.T) {
	dir := t.TempDir()
	_, flac := makeClips(t, dir, 44100)
	r, err := openClip(flac, dir, canonRate, canonCh)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close is safe.
	if err := r.Close(); err != nil {
		t.Errorf("double Close: %v", err)
	}
	// Read after close errors cleanly (no panic).
	if _, err := r.Read(make([]float32, 64)); err == nil {
		t.Error("Read after Close: expected error")
	}
}

// --- helpers ---

func makeClips48(t *testing.T, dir string) (wav, flac string) {
	t.Helper()
	frames := 48000 / 4
	samples := sineSamples(frames, 48000, 2)
	wav, flac = "sine48.wav", "sine48.flac"
	writeWAV(t, filepath.Join(dir, wav), samples, 48000, 2)
	writeFLAC(t, filepath.Join(dir, flac), samples, 48000, 2)
	return wav, flac
}

func copyFixture(t *testing.T, dir, fixture, dst string) string {
	t.Helper()
	return copyData(t, dir, mp3Fixture(fixture), dst)
}

func copyData(t *testing.T, dir, src, dst string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

func writeBlob(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatalf("writeBlob: %v", err)
	}
	return name
}

func truncate(t *testing.T, dir, src, dst string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, src))
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	// Keep the fLaC magic (so the .flac dispatch still selects the FLAC decoder)
	// but cut the stream short so STREAMINFO/decoder init fails.
	if len(b) > 8 {
		b = b[:8]
	}
	return writeBlob(t, dir, dst, b)
}
