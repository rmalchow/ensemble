package source

// WAV/PCM (RIFF) source decode — stdlib only. We parse the RIFF/WAVE container,
// read the `fmt ` chunk (PCM = format 1 or IEEE float = 3; channels; sample rate;
// bits-per-sample 16/24/32-int or 32-float), then stream the `data` chunk widening
// each sample to interleaved float32 in [-1,1). Looping seeks back to the start of
// the data chunk (seekable inputs only).

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	wavFormatPCM   = 1
	wavFormatFloat = 3
	wavFormatExt   = 0xFFFE // WAVE_FORMAT_EXTENSIBLE; subformat decides PCM vs float
)

// wavSource adapts a streamed RIFF data chunk to the frameSource seam.
type wavSource struct {
	rc       io.ReadCloser
	r        *bufio.Reader
	seeker   io.Seeker // non-nil if the input is seekable (data/ file)

	rateHz   int
	chans    int
	bits     int  // bits per sample
	isFloat  bool // sample format is IEEE float
	bytesPer int  // bytes per single (one-channel) sample

	dataStart int64 // byte offset of the data chunk payload (for looping)
	dataLen   int64 // payload length in bytes
	dataRead  int64 // bytes consumed from the data chunk so far

	// partial holds bytes of an incomplete interleaved sample carried across reads.
	partial []byte
	scratch []byte // reusable read buffer
}

// openWAV parses the RIFF header over rc and positions at the data chunk payload.
func openWAV(rc io.ReadCloser) (*wavSource, error) {
	br := bufio.NewReaderSize(rc, 8192)
	seeker, _ := rc.(io.Seeker)
	s := &wavSource{rc: rc, r: br, seeker: seeker}
	if err := s.parseHeader(); err != nil {
		rc.Close()
		return nil, err
	}
	return s, nil
}

// parseHeader walks the RIFF chunks until the data chunk payload is reached,
// recording the format and the data offset/length.
func (s *wavSource) parseHeader() error {
	var hdr [12]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return fmt.Errorf("decode wav: read RIFF header: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return errors.New("decode wav: not a RIFF/WAVE file")
	}
	// Byte offset of the next chunk header, tracked for the seekable data offset.
	pos := int64(12)
	gotFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(s.r, ch[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return errors.New("decode wav: no data chunk")
			}
			return fmt.Errorf("decode wav: read chunk header: %w", err)
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		pos += 8
		switch id {
		case "fmt ":
			body := make([]byte, size)
			if _, err := io.ReadFull(s.r, body); err != nil {
				return fmt.Errorf("decode wav: read fmt chunk: %w", err)
			}
			if err := s.parseFmt(body); err != nil {
				return err
			}
			gotFmt = true
			pos += size
			if size%2 == 1 { // chunks are word-aligned
				if _, err := s.r.Discard(1); err != nil {
					return fmt.Errorf("decode wav: pad: %w", err)
				}
				pos++
			}
		case "data":
			if !gotFmt {
				return errors.New("decode wav: data chunk before fmt chunk")
			}
			s.dataStart = pos
			s.dataLen = size
			return nil
		default:
			// Skip unknown chunk (word-aligned).
			skip := size
			if skip%2 == 1 {
				skip++
			}
			if _, err := s.r.Discard(int(skip)); err != nil {
				return fmt.Errorf("decode wav: skip chunk %q: %w", id, err)
			}
			pos += skip
		}
	}
}

// parseFmt decodes a `fmt ` chunk body.
func (s *wavSource) parseFmt(b []byte) error {
	if len(b) < 16 {
		return errors.New("decode wav: short fmt chunk")
	}
	format := int(binary.LittleEndian.Uint16(b[0:2]))
	s.chans = int(binary.LittleEndian.Uint16(b[2:4]))
	s.rateHz = int(binary.LittleEndian.Uint32(b[4:8]))
	s.bits = int(binary.LittleEndian.Uint16(b[14:16]))
	if format == wavFormatExt {
		// WAVE_FORMAT_EXTENSIBLE: the real format tag is the first 2 bytes of the
		// SubFormat GUID in the extension.
		if len(b) < 26 {
			return errors.New("decode wav: short extensible fmt chunk")
		}
		format = int(binary.LittleEndian.Uint16(b[24:26]))
	}
	switch format {
	case wavFormatPCM:
		s.isFloat = false
	case wavFormatFloat:
		s.isFloat = true
	default:
		return fmt.Errorf("decode wav: unsupported format tag %d", format)
	}
	if s.chans <= 0 {
		return fmt.Errorf("decode wav: invalid channel count %d", s.chans)
	}
	switch {
	case s.isFloat && s.bits == 32:
	case !s.isFloat && (s.bits == 16 || s.bits == 24 || s.bits == 32):
	default:
		return fmt.Errorf("decode wav: unsupported bits/format %d/%v", s.bits, s.isFloat)
	}
	s.bytesPer = s.bits / 8
	return nil
}

func (s *wavSource) rate() int     { return s.rateHz }
func (s *wavSource) channels() int { return s.chans }

// read fills dst with whole interleaved frames widened from the data chunk. n is
// always a multiple of channels().
func (s *wavSource) read(dst []float32) (n int, err error) {
	frameBytes := s.bytesPer * s.chans
	want := len(dst) - len(dst)%s.chans
	if want == 0 {
		return 0, nil
	}
	remaining := s.dataLen - s.dataRead + int64(len(s.partial))
	if remaining <= 0 {
		return 0, io.EOF
	}
	// Cap the request to whole interleaved frames available in the data chunk.
	maxSamp := int(remaining / int64(s.bytesPer))
	maxSamp -= maxSamp % s.chans
	if want > maxSamp {
		want = maxSamp
	}
	if want == 0 {
		return 0, io.EOF
	}
	need := want * s.bytesPer
	if cap(s.scratch) < need {
		s.scratch = make([]byte, need)
	}
	buf := s.scratch[:need]
	// Reuse any partial bytes carried from a previous call.
	pc := copy(buf, s.partial)
	s.partial = s.partial[:0]
	got := pc
	for got < need {
		avail := need - got
		if rem := s.dataLen - s.dataRead; int64(avail) > rem {
			avail = int(rem)
		}
		if avail == 0 {
			break
		}
		m, rerr := io.ReadFull(s.r, buf[got:got+avail])
		s.dataRead += int64(m)
		got += m
		if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
	}
	// Whole interleaved frames only; stash any partial sample tail.
	usable := got - got%frameBytes
	if usable < got {
		s.partial = append(s.partial[:0], buf[usable:got]...)
	}
	for i := 0; i < usable; i += s.bytesPer {
		dst[n] = s.sample(buf[i : i+s.bytesPer])
		n++
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// sample widens one little-endian source sample to float32 in [-1,1).
func (s *wavSource) sample(b []byte) float32 {
	if s.isFloat { // 32-bit IEEE float
		bits := binary.LittleEndian.Uint32(b)
		return math.Float32frombits(bits)
	}
	switch s.bits {
	case 16:
		v := int16(uint16(b[0]) | uint16(b[1])<<8)
		return float32(v) / 32768
	case 24:
		// sign-extend 24-bit little-endian into int32
		u := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if u&0x800000 != 0 {
			u |= 0xFF000000
		}
		return float32(int32(u)) / 8388608
	case 32:
		v := int32(binary.LittleEndian.Uint32(b))
		return float32(float64(v) / 2147483648.0)
	}
	return 0
}

// seekStart rewinds to the data chunk start for looping (seekable inputs only).
func (s *wavSource) seekStart() error {
	if s.seeker == nil {
		return errSeekUnsupported
	}
	if _, err := s.seeker.Seek(s.dataStart, io.SeekStart); err != nil {
		return fmt.Errorf("source: wav seek to data start: %w", err)
	}
	s.r.Reset(s.rc)
	s.dataRead = 0
	s.partial = s.partial[:0]
	return nil
}

func (s *wavSource) close() error {
	if s.rc == nil {
		return nil
	}
	err := s.rc.Close()
	s.rc = nil
	return err
}
