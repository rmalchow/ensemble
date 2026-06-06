package source

// Content resampler: source-native rate → canonical rate (e.g. 44100→48000).
// This is a real arbitrary-ratio conversion done once at decode, DISTINCT from the
// near-unity ±200 ppm drift resampler in internal/audio/resampler (06, A.3) which
// trims clock skew at playout. If the native rate already equals the canonical
// rate the resampler is bypassed entirely (see Open in source.go).
//
// Algorithm: per-channel 4-tap Catmull-Rom cubic interpolation keyed by the ratio
// srcRate/dstRate. A 3-sample history per channel is carried across read calls so
// phase is continuous and monotonic over block boundaries. The steady state
// allocates nothing: the input scratch and per-channel history are sized once.

import "io"

// resampler wraps a frameSource, converting its native rate to dstRate. It honors
// the frameSource seam so loopReader can wrap it transparently; seekStart resets
// the interpolation phase/history so the loop seam stays continuous.
type resampler struct {
	src      frameSource
	chans    int
	srcRate  int
	dstRate  int
	step     float64 // input samples advanced per output sample = src/dst

	// Per-channel ring of the four most recent input frames used by the cubic
	// kernel: hist[ch] = {x[-1], x[0], x[1], x[2]} relative to the fractional
	// read cursor `pos` (pos in [0,1)). Initialized to silence so the very first
	// outputs ramp cleanly from zero.
	hist [][4]float32
	pos  float64 // fractional position within the [x0,x1] interval, in [0,1)

	in     []float32 // reusable input scratch (interleaved native samples)
	inLen  int       // valid interleaved samples currently in `in`
	inOff  int       // interleaved read offset into `in`
	srcEOF bool      // underlying source returned io.EOF
}

// newResampler builds a resampler from src to dstRate. Callers must ensure
// src.rate() != dstRate (the unity case is bypassed by Open).
func newResampler(src frameSource, dstRate int) *resampler {
	ch := src.channels()
	r := &resampler{
		src:     src,
		chans:   ch,
		srcRate: src.rate(),
		dstRate: dstRate,
		step:    float64(src.rate()) / float64(dstRate),
		hist:    make([][4]float32, ch),
	}
	return r
}

func (r *resampler) rate() int     { return r.dstRate }
func (r *resampler) channels() int { return r.chans }

// fillInput tops up the input scratch with at least one fresh interleaved frame
// from the underlying source, returning false at true source EOF (no more input).
func (r *resampler) fillInput() (bool, error) {
	if r.inOff < r.inLen {
		return true, nil
	}
	if r.srcEOF {
		return false, nil
	}
	const blockFrames = 1024
	need := blockFrames * r.chans
	if cap(r.in) < need {
		r.in = make([]float32, need)
	}
	n, err := r.src.read(r.in[:need])
	r.inLen = n
	r.inOff = 0
	if err == io.EOF {
		r.srcEOF = true
	} else if err != nil {
		return false, err
	}
	return n > 0, nil
}

// advance shifts the per-channel history by one input frame, pulling the next
// frame from the input scratch. Returns false when no further input is available.
func (r *resampler) advance() (bool, error) {
	ok, err := r.fillInput()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	for ch := 0; ch < r.chans; ch++ {
		h := &r.hist[ch]
		h[0], h[1], h[2] = h[1], h[2], h[3]
		h[3] = r.in[r.inOff+ch]
	}
	r.inOff += r.chans
	return true, nil
}

// read produces up to len(dst) interleaved float32 output samples at dstRate. n is
// always a multiple of channels(). It returns io.EOF only when the underlying
// source is exhausted and no output remains (looping is the wrapping layer's job).
func (r *resampler) read(dst []float32) (n int, err error) {
	want := len(dst) - len(dst)%r.chans
	for n < want {
		// Whenever the fractional cursor crosses into the next input interval,
		// pull fresh input frames to keep the 4-tap window centered on [x0,x1].
		for r.pos >= 1 {
			ok, aerr := r.advance()
			if aerr != nil {
				return n, aerr
			}
			if !ok {
				if n == 0 {
					return 0, io.EOF
				}
				return n, nil
			}
			r.pos--
		}
		t := float32(r.pos)
		for ch := 0; ch < r.chans; ch++ {
			h := r.hist[ch]
			dst[n] = catmullRom(h[0], h[1], h[2], h[3], t)
			n++
		}
		r.pos += r.step
	}
	return n, nil
}

// seekStart rewinds the underlying source and resets interpolation state so the
// loop boundary stays phase-continuous (history re-primes from the loop head).
func (r *resampler) seekStart() error {
	if err := r.src.seekStart(); err != nil {
		return err
	}
	for ch := range r.hist {
		r.hist[ch] = [4]float32{}
	}
	r.pos = 0
	r.inLen, r.inOff = 0, 0
	r.srcEOF = false
	return nil
}

func (r *resampler) close() error { return r.src.close() }

// catmullRom evaluates the Catmull-Rom cubic through (p1,p2) using neighbors
// p0,p3 at fractional position t in [0,1) between p1 and p2.
func catmullRom(p0, p1, p2, p3, t float32) float32 {
	t2 := t * t
	t3 := t2 * t
	return 0.5 * (2*p1 +
		(-p0+p2)*t +
		(2*p0-5*p1+4*p2-p3)*t2 +
		(-p0+3*p1-3*p2+p3)*t3)
}
