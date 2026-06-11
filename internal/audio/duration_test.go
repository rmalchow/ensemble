package audio

import (
	"bytes"
	"math"
	"os"
	"testing"
)

// durationer is the optional decoder method openFile uses to fill DurationSec.
type durationer interface {
	duration() (float64, bool)
}

func TestWAVDurationFromDataSize(t *testing.T) {
	// 8000 stereo sample-frames at 8 kHz == exactly 1.0 s.
	const rate, ch, frames = 8000, 2, 8000
	wav := writeWAVs16(rate, ch, make([]int16, frames*ch))
	sr, err := newWAVSource(bytes.NewReader(wav))
	if err != nil {
		t.Fatal(err)
	}
	secs, ok := sr.duration()
	if !ok {
		t.Fatal("duration not reported")
	}
	if math.Abs(secs-1.0) > 0.01 {
		t.Fatalf("duration = %.3fs, want ~1.0s", secs)
	}
}

func TestMP3FLACFixtureDurationPositive(t *testing.T) {
	for _, tc := range []struct{ file, format string }{
		{"tone.mp3", "mp3"},
		{"tone.flac", "flac"},
	} {
		p, skip := maybeFixture(t, tc.file)
		if skip {
			t.Skipf("no testdata/%s fixture", tc.file)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := newDecoder(f, tc.format)
		if err != nil {
			f.Close()
			t.Fatalf("%s: %v", tc.file, err)
		}
		d, ok := dec.(durationer)
		if !ok {
			f.Close()
			t.Fatalf("%s decoder does not report duration", tc.format)
		}
		secs, ok := d.duration()
		f.Close()
		if !ok || secs <= 0 {
			t.Fatalf("%s duration = %.3f ok=%v, want > 0", tc.file, secs, ok)
		}
	}
}
