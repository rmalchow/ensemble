package audio

import (
	"context"
	"encoding/binary"

	"ensemble/internal/calibrate"
	"ensemble/internal/stream"
)

// SchemeCalibrate is the generated, looping acoustic-calibration reference
// (docs/calibrate.md §3): a windowed logarithmic sweep emitted as a never-ending
// loop so the group master streams it like any live source while the mic node
// measures each speaker. The signal is byte-for-byte the same as what the
// estimator matched-filters against (both come from calibrate.NewReference with
// the default config) — that identity is what makes the correlation exact.
const SchemeCalibrate = "calibrate"

// openCalibrate builds the looping reference source (the URI carries no
// parameters; the default reference config is authoritative on both ends).
func openCalibrate(_ context.Context, _, _ string) (Source, error) {
	ref := calibrate.NewReference(calibrate.Config{})
	return newLoopSource(ref.Loop()), nil
}

// loopSource emits a fixed mono float32 buffer as canonical stereo s16le frames,
// repeating forever with live-paced semantics (never EOF). The mono signal is
// duplicated to both channels.
type loopSource struct {
	pcm []byte // pre-rendered canonical stereo s16le for one loop period
	pos int
}

func newLoopSource(mono []float32) *loopSource {
	pcm := make([]byte, len(mono)*stream.Channels*2)
	for i, s := range mono {
		v := int16(clampUnit(s) * 32767)
		off := i * stream.Channels * 2
		binary.LittleEndian.PutUint16(pcm[off:], uint16(v))
		binary.LittleEndian.PutUint16(pcm[off+2:], uint16(v))
	}
	return &loopSource{pcm: pcm}
}

func clampUnit(s float32) float32 {
	if s > 1 {
		return 1
	}
	if s < -1 {
		return -1
	}
	return s
}

// ReadFrame fills one canonical frame, wrapping at the loop boundary. Both the
// loop buffer and the frame are whole-sample (4-byte) sized, and pos only ever
// advances/resets on 4-byte boundaries, so a sample is never split across the
// wrap.
func (l *loopSource) ReadFrame(dst []byte) error {
	d := dst[:stream.FrameBytes]
	for n := 0; n < len(d); {
		if l.pos >= len(l.pcm) {
			l.pos = 0
		}
		c := copy(d[n:], l.pcm[l.pos:])
		l.pos += c
		n += c
	}
	return nil
}

func (l *loopSource) Live() bool   { return true }
func (l *loopSource) Close() error { return nil }
