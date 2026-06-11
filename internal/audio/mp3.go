package audio

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
)

// mp3Source adapts hajimehoshi/go-mp3, which always emits 2-channel 16-bit
// little-endian PCM at the file's sample rate.
type mp3Source struct {
	dec  *mp3.Decoder
	rate int
	odd  []byte // carry for a partial 4-byte sample-frame across reads
	eof  bool
}

func newMP3Source(r io.Reader) (*mp3Source, error) {
	dec, err := mp3.NewDecoder(r)
	if err != nil {
		return nil, fmt.Errorf("%w: mp3 open: %v", ErrBadMedia, err)
	}
	return &mp3Source{dec: dec, rate: dec.SampleRate()}, nil
}

func (m *mp3Source) info() (int, int) { return m.rate, 2 }

// duration reports the track length in seconds from the decoder's total PCM
// length (go-mp3 emits 16-bit stereo at the file's rate, so 4 bytes/sample-frame).
// ok=false for a zero/unknown length.
func (m *mp3Source) duration() (float64, bool) {
	n := m.dec.Length()
	if n <= 0 || m.rate <= 0 {
		return 0, false
	}
	return float64(n) / float64(m.rate*4), true
}

func (m *mp3Source) Close() error { return nil }

func (m *mp3Source) read(dst []int16) ([]int16, error) {
	if m.eof {
		return dst, io.EOF
	}
	const blk = 8192
	buf := make([]byte, len(m.odd)+blk)
	copy(buf, m.odd)
	n, err := m.dec.Read(buf[len(m.odd):])
	total := len(m.odd) + n
	buf = buf[:total]
	m.odd = nil

	whole := (total / 4) * 4
	for off := 0; off+1 < whole; off += 2 {
		dst = append(dst, int16(binary.LittleEndian.Uint16(buf[off:])))
	}
	if rem := buf[whole:]; len(rem) > 0 {
		m.odd = append([]byte(nil), rem...)
	}

	if err == io.EOF || err == io.ErrUnexpectedEOF {
		m.eof = true
		return dst, io.EOF
	}
	if err != nil {
		return dst, fmt.Errorf("%w: mp3 read: %v", ErrBadMedia, err)
	}
	return dst, nil
}
