package source

// Adapted from ../media/internal/audio/source.go (the mewkiz/flac streaming
// decode body is reused verbatim — the per-subframe int→float32 conversion, the
// residual-buffer tail handling, the seekable NewSeek open, and the frame-index
// seek). Changes vs. the original: package audio → source; exported Source/OpenFLAC
// become the unexported flacSource/openFLAC returning the internal frameSource
// interface; SeekFrame keeps its name (go vet stdmethods reserves "Seek"); the
// methods are renamed to the lowercase frameSource seam (read/rate/channels/
// seekStart/close). No media imports existed here, so no import rewrite is needed.

import (
	"errors"
	"fmt"
	"io"

	"github.com/mewkiz/flac"
)

// flacSource is a constant-rate decoded PCM source over mewkiz/flac. It streams
// frames lazily, converting each frame's per-channel int samples to interleaved
// float32 in [-1,1). A small residual buffer carries the tail of a decoded frame
// across read calls so the caller can pull arbitrary sample counts.
type flacSource struct {
	rc     io.Closer // underlying file/HTTP body, released on close
	stream *flac.Stream

	rateHz   int
	chans    int
	bps      uint  // bits per sample, from STREAMINFO
	total    int64 // total per-channel frames, -1 if unknown
	seekable bool  // stream supports SeekFrame (data/ file); HTTP bodies do not

	// resid holds interleaved f32 samples decoded but not yet returned by read.
	resid []float32
	off   int // read offset into resid
	eof   bool
}

// openFLAC prepares a streaming f32 decode over rc. If rc is an io.ReadSeeker the
// decoder is opened seekable (so seekStart can reposition by frame for looping);
// otherwise a forward-only decoder is used and seekStart reports unsupported (the
// loop layer falls back to re-opening the input — see input.go / loop.go).
func openFLAC(rc io.ReadCloser) (*flacSource, error) {
	var (
		stream   *flac.Stream
		err      error
		seekable bool
	)
	if rs, ok := rc.(io.ReadSeeker); ok {
		stream, err = flac.NewSeek(rs)
		seekable = true
	} else {
		stream, err = flac.New(rc)
	}
	if err != nil {
		rc.Close()
		return nil, fmt.Errorf("decode flac: %w", err)
	}
	if stream.Info == nil {
		stream.Close()
		rc.Close()
		return nil, errors.New("decode flac: missing streaminfo")
	}
	total := int64(-1)
	if stream.Info.NSamples != 0 {
		total = int64(stream.Info.NSamples)
	}
	return &flacSource{
		rc:       rc,
		stream:   stream,
		rateHz:   int(stream.Info.SampleRate),
		chans:    int(stream.Info.NChannels),
		bps:      uint(stream.Info.BitsPerSample),
		total:    total,
		seekable: seekable,
	}, nil
}

func (s *flacSource) rate() int     { return s.rateHz }
func (s *flacSource) channels() int { return s.chans }

// read fills dst with up to len(dst) interleaved f32 samples, returning n samples
// and io.EOF at end. n is always a multiple of channels(). It decodes further FLAC
// frames as needed and stashes any tail past dst in the residual buffer.
func (s *flacSource) read(dst []float32) (n int, err error) {
	if s.chans <= 0 {
		return 0, errors.New("source: invalid channel count")
	}
	// Round the request down to whole interleaved frames so n stays a multiple of
	// channels() regardless of how the caller sizes dst.
	want := len(dst) - len(dst)%s.chans
	for n < want {
		if s.off >= len(s.resid) {
			if s.eof {
				break
			}
			if err := s.decodeFrame(); err != nil {
				if err == io.EOF {
					s.eof = true
					break
				}
				return n, err
			}
			continue
		}
		c := copy(dst[n:want], s.resid[s.off:])
		s.off += c
		n += c
	}
	if n == 0 && s.eof {
		return 0, io.EOF
	}
	return n, nil
}

// decodeFrame pulls one FLAC frame, converts its subframes to interleaved f32, and
// stores the result in the residual buffer (resetting the read offset).
func (s *flacSource) decodeFrame() error {
	fr, err := s.stream.ParseNext()
	if err != nil {
		return err
	}
	if len(fr.Subframes) != s.chans {
		return fmt.Errorf("source: frame has %d channels, want %d", len(fr.Subframes), s.chans)
	}
	bps := uint(fr.BitsPerSample)
	if bps == 0 {
		bps = s.bps
	}
	// Scale: a bps-bit signed sample spans [-2^(bps-1), 2^(bps-1)-1]; divide by
	// 2^(bps-1) to land in [-1,1).
	scale := float32(1) / float32(int64(1)<<(bps-1))
	nSamp := fr.Subframes[0].NSamples
	if cap(s.resid) < nSamp*s.chans {
		s.resid = make([]float32, nSamp*s.chans)
	} else {
		s.resid = s.resid[:nSamp*s.chans]
	}
	for ch := 0; ch < s.chans; ch++ {
		samp := fr.Subframes[ch].Samples
		for i := 0; i < nSamp; i++ {
			s.resid[i*s.chans+ch] = float32(samp[i]) * scale
		}
	}
	s.off = 0
	return nil
}

// seekStart repositions to per-channel frame 0 for looping. It clears the residual
// buffer and the EOF latch so decoding resumes from the top. It is reported as
// unsupported (errSeekUnsupported) for a non-seekable (HTTP) stream; loop.go then
// re-opens the input instead.
//
// NOTE: the loop primitive is named seekStart (not Seek): go vet's stdmethods
// analyzer reserves "Seek" for io.Seeker's Seek(int64, int) (int64, error).
func (s *flacSource) seekStart() error {
	if !s.seekable {
		return errSeekUnsupported
	}
	if _, err := s.stream.Seek(0); err != nil {
		return fmt.Errorf("source: seek to frame 0: %w", err)
	}
	s.resid = s.resid[:0]
	s.off = 0
	s.eof = false
	return nil
}

// close releases the underlying decoder and the file/HTTP handle.
func (s *flacSource) close() error {
	var err error
	if s.stream != nil {
		err = s.stream.Close()
		s.stream = nil
	}
	if s.rc != nil {
		if cerr := s.rc.Close(); err == nil {
			err = cerr
		}
		s.rc = nil
	}
	return err
}
