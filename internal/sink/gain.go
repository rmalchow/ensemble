package sink

import (
	"encoding/binary"
	"math"
	"sync/atomic"

	"ensemble/internal/stream"
)

// gainStage applies a per-node software volume (D35) as the LAST stage before
// the backend, after the resampler. The target gain is stored atomically (the
// float64 bit pattern in an atomic.Uint64) so SetGain can be called from any
// goroutine with no lock on the hot path. Each output frame is scaled by a
// linear ramp from the gain in force at the start of the frame to the current
// target, spread across the FrameSamples sample-times — so a volume change
// settles within one 20 ms frame with no zipper/step discontinuity.
type gainStage struct {
	target  atomic.Uint64 // math.Float64bits(targetGain), set by SetGain (any goroutine)
	current float64       // gain reached at end of last frame (scheduler goroutine only)
}

func newGainStage(initial float64) *gainStage {
	initial = clampGain(initial)
	g := &gainStage{current: initial}
	g.target.Store(math.Float64bits(initial))
	return g
}

// currentTarget returns the current target gain (for unchanged-guards).
func (g *gainStage) currentTarget() float64 {
	return math.Float64frombits(g.target.Load())
}

// setTarget stores a new target gain atomically (clamped to [0,1]). Lock-free;
// safe from any goroutine. Takes effect on the next frame via the ramp.
func (g *gainStage) setTarget(v float64) {
	g.target.Store(math.Float64bits(clampGain(v)))
}

// apply scales frame in place (interleaved s16le, FrameSamples·Channels
// samples). It reads target once, ramps current → target linearly across the
// frame's FrameSamples sample-times (both channels of a sample-time share the
// same factor), multiplies each int16 sample, rounds, and clamps. After the
// frame, current == target. Scheduler goroutine only.
func (g *gainStage) apply(frame []byte) {
	target := math.Float64frombits(g.target.Load())
	cur := g.current
	if cur == target && target == 1.0 {
		return // unity passthrough fast path (bit-identical)
	}
	const n = stream.FrameSamples
	delta := (target - cur) / float64(n)
	gain := cur
	for i := 0; i < n; i++ {
		for ch := 0; ch < stream.Channels; ch++ {
			off := (i*stream.Channels + ch) * stream.BytesPerSmpl
			s := int32(int16(binary.LittleEndian.Uint16(frame[off : off+2])))
			v := scaleClamp(float64(s) * gain)
			binary.LittleEndian.PutUint16(frame[off:off+2], uint16(v))
		}
		gain += delta
	}
	g.current = target
}

func clampGain(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func scaleClamp(v float64) int16 {
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
