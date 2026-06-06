package source

// Test fixture generators: synthesize a 1 kHz sine tone in WAV and FLAC at a
// given rate/channels, written to a temp dir. mp3 fixtures are committed under
// testdata/ (no pure-Go mp3 encoder is in the dependency set). These mirror the
// ../media source_test fixture style: tiny generated clips rather than large
// committed binaries.

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

const toneFreq = 1000.0 // 1 kHz reference tone

// sineSamples returns nframes of an interleaved float32 1 kHz sine at rate, with
// `channels` identical channels, amplitude 0.5.
func sineSamples(nframes, rate, channels int) []float32 {
	out := make([]float32, nframes*channels)
	for i := 0; i < nframes; i++ {
		v := float32(0.5 * math.Sin(2*math.Pi*toneFreq*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			out[i*channels+c] = v
		}
	}
	return out
}

// writeWAV writes a 16-bit PCM WAV of the given interleaved float32 samples.
func writeWAV(t *testing.T, path string, samples []float32, rate, channels int) {
	t.Helper()
	nframes := len(samples) / channels
	dataLen := nframes * channels * 2 // 16-bit
	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	w(uint32(36 + dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(channels))
	w(uint32(rate))
	w(uint32(rate * channels * 2)) // byte rate
	w(uint16(channels * 2))        // block align
	w(uint16(16))                  // bits
	buf.WriteString("data")
	w(uint32(dataLen))
	for _, s := range samples {
		w(int16(clampF(s) * 32767))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
}

// writeFloatWAV writes a 32-bit IEEE-float WAV (format tag 3).
func writeFloatWAV(t *testing.T, path string, samples []float32, rate, channels int) {
	t.Helper()
	nframes := len(samples) / channels
	dataLen := nframes * channels * 4
	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	w(uint32(36 + dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	w(uint32(16))
	w(uint16(3)) // IEEE float
	w(uint16(channels))
	w(uint32(rate))
	w(uint32(rate * channels * 4))
	w(uint16(channels * 4))
	w(uint16(32))
	buf.WriteString("data")
	w(uint32(dataLen))
	for _, s := range samples {
		w(math.Float32bits(s))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writeFloatWAV: %v", err)
	}
}

// writeFLAC writes a 16-bit FLAC of the given interleaved float32 samples using
// verbatim subframes (deterministic, fast for tiny clips).
func writeFLAC(t *testing.T, path string, samples []float32, rate, channels int) {
	t.Helper()
	nframes := len(samples) / channels
	info := &meta.StreamInfo{
		BlockSizeMin:  4096,
		BlockSizeMax:  4096,
		SampleRate:    uint32(rate),
		NChannels:     uint8(channels),
		BitsPerSample: 16,
		NSamples:      uint64(nframes),
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create flac: %v", err)
	}
	defer f.Close()
	enc, err := flac.NewEncoder(f, info)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	chSel := frame.ChannelsLR
	if channels == 1 {
		chSel = frame.ChannelsMono
	}
	const block = 4096
	for off := 0; off < nframes; off += block {
		bs := block
		if off+bs > nframes {
			bs = nframes - off
		}
		fr := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(bs),
			SampleRate:        uint32(rate),
			Channels:          chSel,
			BitsPerSample:     16,
		}}
		fr.Subframes = make([]*frame.Subframe, channels)
		for c := 0; c < channels; c++ {
			cs := make([]int32, bs)
			for i := 0; i < bs; i++ {
				cs[i] = int32(clampF(samples[(off+i)*channels+c]) * 32767)
			}
			fr.Subframes[c] = &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
				Samples:   cs,
				NSamples:  bs,
			}
		}
		if err := enc.WriteFrame(fr); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("flac Close: %v", err)
	}
}

func clampF(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// mp3Fixture returns the path to a committed mp3 fixture under testdata/.
func mp3Fixture(name string) string { return filepath.Join("testdata", name) }
