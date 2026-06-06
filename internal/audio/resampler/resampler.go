// Package resampler is the near-unity ppm-drift resampler. Leaf: imports no
// sibling internal packages.
package resampler

// Resampler is a near-unity variable-ratio interpolator (cubic / 4-tap Farrow).
// ratio = output rate / input rate; the drift loop sets it per block within
// 1 ± MaxPPM. NOT a sample-rate converter for large ratios.
//
// It uses 4-tap cubic Hermite (Catmull-Rom) interpolation per channel on
// interleaved f32. A fractional-phase accumulator and the trailing three input
// frames are carried across Process calls so block boundaries are seamless: the
// interpolator always sees a continuous input stream regardless of how the caller
// chops it.
type Resampler struct {
	channels int
	step     float64 // input frames advanced per output frame = 1/ratio

	// hist holds the three most recent input frames (interleaved), oldest first:
	// [frame n-2 .. frame n]. Combined with the next pending input frame it forms
	// the 4 taps centred on the current integer position. nhist counts how many of
	// the three slots are primed (0..3) after a Reset.
	hist  []float32
	nhist int

	// phase is the fractional position in [0,1) between the current integer input
	// frame (the newest history frame) and the next pending input frame.
	phase float64
}

const MaxPPM = 200 // ±0.02% ratio clamp

// NewResampler builds a resampler for `channels` interleaved channels.
func NewResampler(channels int) *Resampler {
	if channels < 1 {
		channels = 1
	}
	r := &Resampler{
		channels: channels,
		step:     1.0, // ratio 1.0
		hist:     make([]float32, 3*channels),
	}
	return r
}

// SetRatio sets the resampling ratio for subsequent Process calls. Values are
// clamped to [1-MaxPPM*1e-6, 1+MaxPPM*1e-6]; MaxPPM defaults to 200.
func (r *Resampler) SetRatio(ratio float64) {
	lo := 1.0 - MaxPPM*1e-6
	hi := 1.0 + MaxPPM*1e-6
	if ratio < lo {
		ratio = lo
	} else if ratio > hi {
		ratio = hi
	}
	r.step = 1.0 / ratio
}

// ratio returns the currently configured (clamped) ratio. Test-only accessor.
func (r *Resampler) ratio() float64 { return 1.0 / r.step }

// Reset clears phase + history (used on a hard reseek).
func (r *Resampler) Reset() {
	for i := range r.hist {
		r.hist[i] = 0
	}
	r.nhist = 0
	r.phase = 0
}

// Process consumes interleaved input and appends interleaved output to dst,
// returning the extended slice and the number of INPUT samples consumed. It keeps
// the fractional phase + tap history across calls so block boundaries are seamless.
//
// The 4 taps for output position p (relative to the newest history frame) are the
// three history frames plus pending input frames; the integer cursor advances
// through `in` as phase wraps past 1. consumed is always a multiple of channels.
func (r *Resampler) Process(dst, in []float32) (out []float32, consumed int) {
	ch := r.channels
	if ch <= 0 || len(in) < ch {
		return dst, 0
	}
	frames := len(in) / ch

	// Prime history first so the interpolator has a full 4-tap window. After a
	// Reset we need the first three input frames seeded before producing output;
	// we seed lazily as frames are consumed below.

	// cur is the index of the next *unprimed* input frame: interpolation happens
	// between the newest history frame (cur-1) and input frame cur. We model the
	// continuous stream as: [hist0, hist1, hist2, in0, in1, ...]. The "current
	// integer position" is the newest available frame; phase is the offset toward
	// the next frame.
	//
	// We use a sliding 4-tap window [s0,s1,s2,s3] where s1 is the integer position
	// and phase in [0,1) interpolates between s1 and s2. s0=s1-1 (prev), s3=s2+1.
	//
	// Maintain the window in r.hist as the three frames before the next pending
	// input frame. Concretely hist = [s0, s1, s2] and the next input frame is s3.
	cur := 0 // next input frame to fold into history (becomes s3)

	for {
		// Need a full window: three primed history frames + one pending input frame.
		if r.nhist < 3 {
			if cur >= frames {
				break // out of input; carry partial history to next call
			}
			r.pushHist(in[cur*ch : cur*ch+ch])
			cur++
			continue
		}
		// s3 is the pending input frame at cur. If we have none, we cannot form the
		// forward tap; stop and resume next call.
		if cur >= frames {
			break
		}
		// Emit output frames while phase stays within the current window.
		for r.phase < 1.0 {
			dst = r.emit(dst, in[cur*ch:cur*ch+ch])
			r.phase += r.step
		}
		// Advance the window by one input frame: s3 becomes the new s2.
		r.phase -= 1.0
		r.pushHist(in[cur*ch : cur*ch+ch])
		cur++
	}

	return dst, cur * ch
}

// pushHist shifts the per-channel history left by one frame and appends frame.
func (r *Resampler) pushHist(frame []float32) {
	ch := r.channels
	copy(r.hist[0:2*ch], r.hist[ch:3*ch])
	copy(r.hist[2*ch:3*ch], frame)
	if r.nhist < 3 {
		r.nhist++
	}
}

// emit interpolates one output frame at the current phase. The 4 taps are the
// three history frames (s0,s1,s2) and the pending input frame s3; phase
// interpolates between s1 and s2.
func (r *Resampler) emit(dst, s3 []float32) []float32 {
	ch := r.channels
	t := r.phase
	for c := 0; c < ch; c++ {
		y0 := r.hist[0*ch+c] // s0 = position -1
		y1 := r.hist[1*ch+c] // s1 = position 0 (integer cursor)
		y2 := r.hist[2*ch+c] // s2 = position +1
		y3 := s3[c]          // s3 = position +2
		dst = append(dst, cubicHermite(y0, y1, y2, y3, float32(t)))
	}
	return dst
}

// cubicHermite is the 4-tap Catmull-Rom interpolation between y1 and y2 at
// fractional position t in [0,1). It reproduces constant and linear inputs exactly
// (DC passthrough) and is C1-continuous across frame boundaries.
func cubicHermite(y0, y1, y2, y3, t float32) float32 {
	// Catmull-Rom: tangents m1=(y2-y0)/2, m2=(y3-y1)/2.
	c0 := y1
	c1 := 0.5 * (y2 - y0)
	c2 := y0 - 2.5*y1 + 2*y2 - 0.5*y3
	c3 := 0.5*(y3-y0) + 1.5*(y1-y2)
	return ((c3*t+c2)*t+c1)*t + c0
}
