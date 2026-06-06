package source

// mp3 source decode via github.com/hajimehoshi/go-mp3 (pure-Go, A.11). go-mp3
// always decodes to 16-bit little-endian, 2-channel PCM at the file's sample rate
// (a mono mp3 is upmixed by the library), exposing an io.ReadSeeker of raw S16LE
// bytes and SampleRate(). We widen S16LE → float32 in [-1,1) on the fly and seek
// to byte 0 for looping (when the underlying reader is seekable).

import (
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
)

// mp3 is fixed stereo, 16-bit: 4 bytes per interleaved frame.
const (
	mp3Channels = 2
	mp3BytesPerSample = 2
)

// mp3Source adapts a go-mp3 decoder to the frameSource seam.
type mp3Source struct {
	rc      io.Closer // file/HTTP body to release on close
	dec     *mp3.Decoder
	rateHz  int
	seekable bool

	// buf holds raw S16LE bytes read from the decoder but not yet widened. It is
	// reused across read calls so the steady state allocates nothing.
	buf []byte
}

// openMP3 prepares a streaming float32 decode over rc. If rc is an io.ReadSeeker,
// looping uses Decoder.Seek(0); otherwise seekStart reports unsupported and the
// loop layer re-opens the input.
func openMP3(rc io.ReadCloser) (*mp3Source, error) {
	dec, err := mp3.NewDecoder(rc)
	if err != nil {
		rc.Close()
		return nil, fmt.Errorf("decode mp3: %w", err)
	}
	_, seekable := rc.(io.ReadSeeker)
	return &mp3Source{
		rc:       rc,
		dec:      dec,
		rateHz:   dec.SampleRate(),
		seekable: seekable,
	}, nil
}

func (s *mp3Source) rate() int     { return s.rateHz }
func (s *mp3Source) channels() int { return mp3Channels }

// read fills dst with whole interleaved frames widened from S16LE. n is always a
// multiple of mp3Channels.
func (s *mp3Source) read(dst []float32) (n int, err error) {
	want := len(dst) - len(dst)%mp3Channels
	if want == 0 {
		return 0, nil
	}
	// Need want*2 source bytes (S16LE). Size the scratch once and keep it.
	need := want * mp3BytesPerSample
	if cap(s.buf) < need {
		s.buf = make([]byte, need)
	}
	b := s.buf[:need]
	// go-mp3's Read may short-read at frame boundaries; loop until we have a whole
	// number of samples or hit EOF.
	got := 0
	for got < need {
		m, rerr := s.dec.Read(b[got:])
		got += m
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
		if m == 0 {
			break
		}
	}
	// Drop a trailing odd byte (cannot happen on clean S16LE, but be defensive).
	got -= got % mp3BytesPerSample
	for i := 0; i < got; i += mp3BytesPerSample {
		// little-endian int16 → float32 in [-1,1)
		v := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		dst[n] = float32(v) / 32768
		n++
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// seekStart rewinds to the start for looping (seekable inputs only).
func (s *mp3Source) seekStart() error {
	if !s.seekable {
		return errSeekUnsupported
	}
	if _, err := s.dec.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("source: mp3 seek to start: %w", err)
	}
	return nil
}

// close releases the underlying file/HTTP handle. go-mp3's Decoder has no Close;
// closing the source reader is sufficient.
func (s *mp3Source) close() error {
	if s.rc == nil {
		return nil
	}
	err := s.rc.Close()
	s.rc = nil
	s.dec = nil
	return err
}
