package audio

import (
	"fmt"
	"io"

	"github.com/mewkiz/flac"
)

// flacSource adapts mewkiz/flac. ParseNext returns whole frames whose
// per-channel int32 samples already have inter-channel correlation undone, so
// we read plain L/R (or mono) and scale to int16 by the bit depth.
type flacSource struct {
	stream   *flac.Stream
	rate     int
	channels int
	shiftR   uint   // right shift to reach 16-bit (when bps > 16)
	shiftL   uint   // left shift to reach 16-bit (when bps < 16)
	samples  uint64 // total inter-channel samples (per channel), 0 when unknown
	eof      bool
}

func newFLACSource(r io.Reader) (*flacSource, error) {
	st, err := flac.New(r)
	if err != nil {
		return nil, fmt.Errorf("%w: flac open: %v", ErrBadMedia, err)
	}
	info := st.Info
	if info == nil {
		return nil, fmt.Errorf("%w: flac missing stream info", ErrBadMedia)
	}
	ch := int(info.NChannels)
	if ch < 1 || ch > 2 {
		return nil, fmt.Errorf("%w: flac unsupported channel count %d", ErrBadMedia, ch)
	}
	f := &flacSource{
		stream:   st,
		rate:     int(info.SampleRate),
		channels: ch,
		samples:  info.NSamples,
	}
	bps := int(info.BitsPerSample)
	if bps > 16 {
		f.shiftR = uint(bps - 16)
	} else if bps < 16 {
		f.shiftL = uint(16 - bps)
	}
	return f, nil
}

func (f *flacSource) info() (int, int) { return f.rate, f.channels }

// duration reports the track length in seconds from the FLAC stream-info total
// sample count. ok=false when the header omits it (NSamples == 0).
func (f *flacSource) duration() (float64, bool) {
	if f.samples == 0 || f.rate <= 0 {
		return 0, false
	}
	return float64(f.samples) / float64(f.rate), true
}

func (f *flacSource) Close() error { return nil }

func (f *flacSource) scale(s int32) int16 {
	if f.shiftR > 0 {
		s >>= f.shiftR
	} else if f.shiftL > 0 {
		s <<= f.shiftL
	}
	if s > 32767 {
		s = 32767
	} else if s < -32768 {
		s = -32768
	}
	return int16(s)
}

func (f *flacSource) read(dst []int16) ([]int16, error) {
	if f.eof {
		return dst, io.EOF
	}
	// One ParseNext yields one frame block; that's a reasonable read granularity.
	fr, err := f.stream.ParseNext()
	if err != nil {
		if err == io.EOF {
			f.eof = true
			return dst, io.EOF
		}
		// flac truncation at the very end normalizes to a graceful EOF.
		f.eof = true
		return dst, io.EOF
	}
	if len(fr.Subframes) == 0 {
		return dst, nil
	}
	n := len(fr.Subframes[0].Samples)
	for i := 0; i < n; i++ {
		for c := 0; c < f.channels; c++ {
			dst = append(dst, f.scale(fr.Subframes[c].Samples[i]))
		}
	}
	return dst, nil
}
