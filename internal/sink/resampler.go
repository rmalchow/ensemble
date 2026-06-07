package sink

import (
	"encoding/binary"

	"ensemble/internal/stream"
)

// resampler converts a stream of input PCM frames into output frames at a
// fractional playback rate (≈ 1.0, nudged by the servo's ppm correction). It is
// a 4-tap Catmull-Rom interpolator with a fractional read cursor that persists
// across frame boundaries, run independently per channel (interleaved L/R).
//
// Catmull-Rom for fractional position t∈[0,1) between samples p1 and p2, with
// neighbours p0 (before) and p3 (after):
//
//	y(t) = 0.5 * ( (2*p1)
//	             + (-p0 + p2)*t
//	             + (2*p0 - 5*p1 + 4*p2 - p3)*t^2
//	             + (-p0 + 3*p1 - 3*p2 + p3)*t^3 )
//
// Output sample k reads input position cursor += step, step = 1/rate,
// rate = 1 + ppm/1e6.
//
// Bookkeeping (§3.4): the resampler keeps a small carry of the most recent input
// samples plus a fractional cursor. Each call it appends the new input frame to
// the carry, emits exactly FrameSamples interpolated outputs, then drops the
// whole input samples it consumed and keeps the leftover fraction as the new
// cursor — so the cursor stays bounded in [0,1) and the per-frame ±ppm
// correction is realized as the occasional extra/fewer input sample consumed at
// a boundary, glitch-free because the carry samples are always contiguous reals.
type resampler struct {
	// carry holds leftover input samples per channel from prior frames, ahead of
	// the current frame's samples. We keep at least leadPad leading samples so the
	// first interpolations of each frame have valid p0..p3 neighbours.
	carry  [stream.Channels][]int32
	cursor float64 // fractional read position within carry (samples/ch)
	rate   float64 // current playback rate (1 + ppm/1e6)
	primed bool

	out  []byte
	work [stream.Channels][]int32 // reusable per-channel input scratch
}

// leadPad is how many leading carry samples we always keep so p0 (cursor-1) and
// the Catmull-Rom window are valid at the seam.
const leadPad = 3

func newResampler() *resampler {
	r := &resampler{rate: 1, out: make([]byte, stream.FrameBytes)}
	for ch := 0; ch < stream.Channels; ch++ {
		r.carry[ch] = make([]int32, 0, stream.FrameSamples+leadPad+4)
		r.work[ch] = make([]int32, stream.FrameSamples)
	}
	return r
}

// setRate sets the resampling ratio from a ppm correction.
func (r *resampler) setRate(ppm float64) {
	r.rate = 1 + ppm/1e6
}

// process consumes input frame `in` (exactly FrameBytes) and returns exactly
// one output frame (FrameBytes). The persistent fractional cursor and carry make
// the seam continuous and the 1-in/1-out cadence exact.
func (r *resampler) process(in []byte) []byte {
	const n = stream.FrameSamples

	// Decode the input frame into per-channel scratch.
	for i := 0; i < n; i++ {
		base := i * stream.Channels * stream.BytesPerSmpl
		for ch := 0; ch < stream.Channels; ch++ {
			off := base + ch*stream.BytesPerSmpl
			r.work[ch][i] = int32(int16(binary.LittleEndian.Uint16(in[off : off+2])))
		}
	}

	if !r.primed {
		// First frame: there is no lookahead yet, so we cannot interpolate its
		// tail (p2/p3 would need the *next* frame and clamping them clicks every
		// seam — the 50 Hz buzz). Seed [leadPad history][this frame] as the
		// HELD lookahead and pass this frame through unchanged (rate ≈ 1 during
		// the servo's warmup anyway). From the next call on, each emitted frame
		// has a full real 4-tap window — one frame (20 ms) of resampler latency,
		// well within bufferMs.
		for ch := 0; ch < stream.Channels; ch++ {
			r.carry[ch] = r.carry[ch][:0]
			s0 := r.work[ch][0]
			for j := 0; j < leadPad; j++ {
				r.carry[ch] = append(r.carry[ch], s0)
			}
			r.carry[ch] = append(r.carry[ch], r.work[ch]...)
		}
		r.cursor = float64(leadPad)
		r.primed = true
		// No lookahead frame yet → emit one frame of silence (a one-time 20 ms
		// startup latency, masked by the playout buffer). From the next call on
		// each output frame is the PREVIOUS input frame, fully resampled with a
		// real 4-tap window at every seam.
		for i := range r.out {
			r.out[i] = 0
		}
		return r.out
	}

	// Steady state: carry holds [leadPad][previous frame]. Append the new frame
	// so the previous frame's tail now has real p2/p3 from this frame, then emit
	// the previous frame's worth of output. The new frame becomes next call's
	// held lookahead.
	for ch := 0; ch < stream.Channels; ch++ {
		r.carry[ch] = append(r.carry[ch], r.work[ch]...)
	}

	step := 1.0 / r.rate

	for ch := 0; ch < stream.Channels; ch++ {
		buf := r.carry[ch]
		last := len(buf) - 1
		for k := 0; k < n; k++ {
			pos := r.cursor + float64(k)*step
			idx := int(pos) // index of p1 in carry
			t := pos - float64(idx)
			p0 := atIdx(buf, idx-1, last)
			p1 := atIdx(buf, idx, last)
			p2 := atIdx(buf, idx+1, last)
			p3 := atIdx(buf, idx+2, last)
			y := catmullRom(p0, p1, p2, p3, t)
			v := clampInt16(y)
			off := (k*stream.Channels + ch) * stream.BytesPerSmpl
			binary.LittleEndian.PutUint16(r.out[off:off+2], uint16(v))
		}
	}

	// Advance the cursor by one output frame's worth of reading, then drop the
	// whole input samples consumed, keeping the fractional leftover. We retain
	// leadPad samples before the new cursor so p0..p3 stay valid at the seam.
	r.cursor += float64(n) * step
	consumed := int(r.cursor) - leadPad // whole samples we can safely discard
	if consumed < 0 {
		consumed = 0
	}
	for ch := 0; ch < stream.Channels; ch++ {
		buf := r.carry[ch]
		if consumed > len(buf) {
			consumed = len(buf)
		}
		// shift down by `consumed`
		copy(buf, buf[consumed:])
		r.carry[ch] = buf[:len(buf)-consumed]
	}
	r.cursor -= float64(consumed)

	// Carry bound. With a sustained rate ≠ 1 and a strict one-frame-in /
	// one-frame-out contract, input accumulates (rate>1) — the integral of the
	// rate error. Cap it: when the held lookahead exceeds two frames, skip one
	// whole frame forward (drop the oldest held frame, keep leadPad history +
	// the newest frame). At a real crystal's tens of ppm this fires every
	// several minutes — a single sub-frame resync, vastly preferable to the
	// per-frame seam clamp it replaces. (The fully glitch-free answer is a
	// variable-input-consumption resampler driven by the jitter-buffer level;
	// tracked as a follow-up.)
	if len(r.carry[0]) > leadPad+2*stream.FrameSamples {
		for ch := 0; ch < stream.Channels; ch++ {
			buf := r.carry[ch]
			copy(buf[leadPad:], buf[leadPad+stream.FrameSamples:])
			r.carry[ch] = buf[:len(buf)-stream.FrameSamples]
		}
	}

	// Carry UNDERFLOW guard. A sustained rate < 1 (outPPM < 0) consumes slightly
	// more input than is fed, so the held lookahead shrinks. The NEXT call needs
	// at least 2 held samples so the last output's p2/p3 are real (else the seam
	// clamp returns and cascades — the user's "degrades until nothing is
	// recognizable"). Normal held ≈ one frame, so this floor (well below it)
	// only fires on a genuine underflow transient; the real cure is keeping the
	// servo off its rail (setpoint calibration). Repeat the newest sample.
	const heldFloor = leadPad + 8
	for ch := 0; ch < stream.Channels; ch++ {
		buf := r.carry[ch]
		for len(buf) < heldFloor {
			buf = append(buf, buf[len(buf)-1])
		}
		r.carry[ch] = buf
	}

	return r.out
}

// atIdx returns buf[i], clamping to the buffer ends (the cursor never strays
// more than ~0.5 sample past the data because rate ≈ 1 and we keep leadPad
// leading samples).
func atIdx(buf []int32, i, last int) int32 {
	if i < 0 {
		i = 0
	} else if i > last {
		i = last
	}
	return buf[i]
}

func catmullRom(p0, p1, p2, p3 int32, t float64) float64 {
	f0 := float64(p0)
	f1 := float64(p1)
	f2 := float64(p2)
	f3 := float64(p3)
	t2 := t * t
	t3 := t2 * t
	return 0.5 * (2*f1 +
		(-f0+f2)*t +
		(2*f0-5*f1+4*f2-f3)*t2 +
		(-f0+3*f1-3*f2+f3)*t3)
}

func clampInt16(v float64) int16 {
	if v >= 0 {
		v += 0.5
	} else {
		v -= 0.5
	}
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// reset clears history + cursor (new session / gen).
func (r *resampler) reset() {
	for ch := 0; ch < stream.Channels; ch++ {
		r.carry[ch] = r.carry[ch][:0]
	}
	r.cursor = 0
	r.primed = false
	r.rate = 1
}
