package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// wavFormat tags we understand.
const (
	wavPCM        = 1
	wavIEEEFloat  = 3
	wavExtensible = 0xFFFE
)

// wavSource is a streaming hand-rolled RIFF/WAVE sample reader. It supports PCM
// u8/s16/s24 and IEEE float32 (clamped to s16). It emits native-rate,
// native-channel interleaved int16 samples via read().
type wavSource struct {
	r         io.Reader
	rate      int
	channels  int
	bitsPer   int     // bits per sample
	format    int     // wavPCM | wavIEEEFloat
	bytesPer  int     // bytes per single sample (per channel)
	remaining int64   // bytes left in the data chunk (math.MaxInt64 if unknown)
	durSec    float64 // track length in seconds, 0 when the data size is unknown
	carry     []byte
	eof       bool
}

// newWAVSource parses the RIFF/WAVE header (fmt + data chunks) and returns a
// streaming reader positioned at the first sample of the data chunk.
func newWAVSource(r io.Reader) (*wavSource, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, fmt.Errorf("%w: wav riff header: %v", ErrBadMedia, err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%w: not a RIFF/WAVE file", ErrBadMedia)
	}

	w := &wavSource{r: r, remaining: math.MaxInt64}
	haveFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, fmt.Errorf("%w: wav missing data chunk", ErrBadMedia)
			}
			return nil, fmt.Errorf("%w: wav chunk header: %v", ErrBadMedia, err)
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))

		switch id {
		case "fmt ":
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, fmt.Errorf("%w: wav fmt chunk: %v", ErrBadMedia, err)
			}
			if len(buf) < 16 {
				return nil, fmt.Errorf("%w: wav fmt chunk too short", ErrBadMedia)
			}
			format := int(binary.LittleEndian.Uint16(buf[0:2]))
			w.channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			w.rate = int(binary.LittleEndian.Uint32(buf[4:8]))
			w.bitsPer = int(binary.LittleEndian.Uint16(buf[14:16]))
			if format == wavExtensible && len(buf) >= 26 {
				// SubFormat GUID's first 2 bytes carry the real format tag.
				format = int(binary.LittleEndian.Uint16(buf[24:26]))
			}
			w.format = format
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, fmt.Errorf("%w: wav data before fmt", ErrBadMedia)
			}
			if size != 0 && size != math.MaxUint32 {
				w.remaining = size
			}
			if err := w.validate(); err != nil {
				return nil, err
			}
			if w.remaining != math.MaxInt64 {
				if bytesPerSec := w.rate * w.channels * w.bytesPer; bytesPerSec > 0 {
					w.durSec = float64(w.remaining) / float64(bytesPerSec)
				}
			}
			return w, nil
		default:
			// Skip aux chunks (LIST, fact, …); chunks are word-aligned.
			skip := size
			if skip%2 == 1 {
				skip++
			}
			if _, err := io.CopyN(io.Discard, r, skip); err != nil {
				return nil, fmt.Errorf("%w: wav skip %q: %v", ErrBadMedia, id, err)
			}
		}
	}
}

func (w *wavSource) validate() error {
	if w.channels < 1 || w.channels > 2 {
		return fmt.Errorf("%w: wav unsupported channel count %d", ErrBadMedia, w.channels)
	}
	if w.rate <= 0 {
		return fmt.Errorf("%w: wav bad sample rate", ErrBadMedia)
	}
	switch w.format {
	case wavPCM:
		switch w.bitsPer {
		case 8, 16, 24, 32:
		default:
			return fmt.Errorf("%w: wav unsupported pcm depth %d", ErrBadMedia, w.bitsPer)
		}
	case wavIEEEFloat:
		if w.bitsPer != 32 {
			return fmt.Errorf("%w: wav unsupported float depth %d", ErrBadMedia, w.bitsPer)
		}
	default:
		return fmt.Errorf("%w: wav unsupported format tag %d", ErrBadMedia, w.format)
	}
	w.bytesPer = w.bitsPer / 8
	return nil
}

func (w *wavSource) info() (int, int) { return w.rate, w.channels }

// duration reports the track length in seconds from the data-chunk size.
// ok=false when the size was unknown/streamed (durSec stayed 0).
func (w *wavSource) duration() (float64, bool) {
	if w.durSec <= 0 {
		return 0, false
	}
	return w.durSec, true
}

func (w *wavSource) Close() error { return nil }

// read appends interleaved int16 samples to dst. It reads whole sample-frames
// only; a truncated final frame is dropped (EOF at the last whole frame).
func (w *wavSource) read(dst []int16) ([]int16, error) {
	if w.eof {
		return dst, io.EOF
	}
	frameBytes := w.bytesPer * w.channels
	// Read a bounded block (~8192 samples worth).
	const maxFrames = 4096
	want := maxFrames * frameBytes
	if int64(want) > w.remaining {
		want = int(w.remaining)
	}
	buf := make([]byte, len(w.carry)+want)
	copy(buf, w.carry)
	n, err := io.ReadFull(w.r, buf[len(w.carry):])
	total := len(w.carry) + n
	w.remaining -= int64(n)
	buf = buf[:total]
	w.carry = nil

	whole := (total / frameBytes) * frameBytes
	for off := 0; off+frameBytes <= total; off += frameBytes {
		for c := 0; c < w.channels; c++ {
			s := w.sample(buf[off+c*w.bytesPer:])
			dst = append(dst, s)
		}
	}

	if err == io.EOF || err == io.ErrUnexpectedEOF || w.remaining <= 0 {
		// A truncated final partial frame (buf[whole:]) is dropped.
		w.eof = true
		return dst, io.EOF
	}
	if err != nil {
		return dst, fmt.Errorf("%w: wav data read: %v", ErrBadMedia, err)
	}
	// Keep the (rare) leftover partial frame for the next read.
	if leftover := buf[whole:]; len(leftover) > 0 {
		w.carry = append([]byte(nil), leftover...)
	}
	return dst, nil
}

// sample decodes one sample (w.bytesPer bytes) from b into int16.
func (w *wavSource) sample(b []byte) int16 {
	switch w.format {
	case wavIEEEFloat: // 32-bit float
		bits := binary.LittleEndian.Uint32(b)
		f := math.Float32frombits(bits)
		v := float64(f) * 32767.0
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		return int16(v)
	case wavPCM:
		switch w.bitsPer {
		case 8: // unsigned
			return int16((int(b[0]) - 128) << 8)
		case 16:
			return int16(binary.LittleEndian.Uint16(b))
		case 24:
			u := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
			if u&0x800000 != 0 {
				u |= ^int32(0xFFFFFF) // sign-extend
			}
			return int16(u >> 8)
		case 32:
			u := int32(binary.LittleEndian.Uint32(b))
			return int16(u >> 16)
		}
	}
	return 0
}
