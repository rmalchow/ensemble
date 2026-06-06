// Package calib is the built-in, in-process calibration signal of an Ensemble
// node (A.10b). It owns (a) a pure, deterministic signal GENERATOR — a fixed
// 1-second period of interleaved float32 frames (a sharp full-scale click, a
// 1 kHz tone, then silence) that every node synthesizes BIT-IDENTICALLY at the
// canonical rate, and (b) a local PLAYER that emits that period into an audio
// sink sample-aligned to the group timeline so the click coincides across nodes.
//
// The generator depends only on SignalParams (the same default on every node):
// no seed, no time, no per-node state. Two nodes calling Fill(dst, S) with the
// same group sample S produce byte-identical dst — the property that makes the
// click coincident and that the future /calibrate/measure path (08 §F2.2) will
// cross-correlate against. The generator is therefore stable API (08 §8 Exposes).
//
// Layering (doc 01 §2): this package is leaf-ish audio. It MAY import
// internal/audio/sink (the AudioSink it writes to) and internal/group (the
// Timeline interface for alignment) and stdlib only. It MUST NOT import web,
// stream/*, state, cluster, or calibctl — the arrow points calibctl -> calib via
// a function-value seam, never the reverse.
package calib

import "math"

// SignalParams describes the built-in calibration waveform (A.10b). All durations
// are derived from the canonical Rate so the produced samples are deterministic
// and identical across nodes. The defaults (DefaultSignalParams) are the
// A.10b/A.12 values; callers should not invent alternatives.
type SignalParams struct {
	Rate     int     // canonical sample rate, Hz. A.12 = 48000.
	Channels int     // output channels. A.12 = 2 (the click/tone are mono, fanned to all channels).
	ClickMs  float64 // full-scale click window. A.10b "~1 ms".
	ToneMs   float64 // 1 kHz tone length. A.10b "~200 ms".
	ToneHz   float64 // tone frequency. A.10b "1 kHz".
	Amp      float32 // tone amplitude (the click is full-scale +1.0 regardless). 0<Amp<=1.
}

// Canonical A.10b / A.12 signal constants. These are the single source of truth
// for the default waveform; do not invent alternatives (P6.2 §5.4). Amp (tone
// amplitude) is the proposed-for-ratification 0.5 (R2) — ear-safe and well below
// the full-scale click transient.
const (
	defaultRate     = 48000  // A.12 canonical rate (Hz)
	defaultChannels = 2      // A.12 canonical channels
	defaultClickMs  = 1.0    // A.10b "~1 ms" click window
	defaultToneMs   = 200.0  // A.10b "~200 ms" tone
	defaultToneHz   = 1000.0 // A.10b "1 kHz" tone
	defaultAmp      = 0.5    // proposed tone amplitude (R2, not pinned in A.12)
)

// DefaultSignalParams returns the A.10b signal at the canonical rate (A.12):
// {Rate:48000, Channels:2, ClickMs:1, ToneMs:200, ToneHz:1000, Amp:0.5}.
func DefaultSignalParams() SignalParams {
	return SignalParams{
		Rate:     defaultRate,
		Channels: defaultChannels,
		ClickMs:  defaultClickMs,
		ToneMs:   defaultToneMs,
		ToneHz:   defaultToneHz,
		Amp:      defaultAmp,
	}
}

// Signal is one fully-precomputed 1-second period of the calibration waveform,
// interleaved float32 in [-1,1]. It is immutable after construction and safe for
// concurrent reads (Fill/Period never mutate it).
type Signal struct {
	period       []float32 // interleaved period, len == periodFrames*channels
	rate         int
	channels     int
	periodFrames int // == rate (exactly 1 s)
	clickFrames  int
	toneFrames   int
}

// NewSignal builds the 1 s period from p, filling any unset (non-positive) field
// from DefaultSignalParams. periodFrames = Rate (exactly 1 s). Layout per period
// (A.10b), all fanned to every channel:
//
//	[0, clickFrames)                       : a single full-scale impulse (+1.0 on
//	                                         frame 0, the rest of the ~1 ms window 0)
//	[clickFrames, clickFrames+toneFrames)  : sin(2π·ToneHz·t)·Amp
//	[clickFrames+toneFrames, Rate)         : silence (0)
func NewSignal(p SignalParams) *Signal {
	d := DefaultSignalParams()
	if p.Rate <= 0 {
		p.Rate = d.Rate
	}
	if p.Channels <= 0 {
		p.Channels = d.Channels
	}
	if p.ClickMs <= 0 {
		p.ClickMs = d.ClickMs
	}
	if p.ToneMs <= 0 {
		p.ToneMs = d.ToneMs
	}
	if p.ToneHz <= 0 {
		p.ToneHz = d.ToneHz
	}
	if p.Amp <= 0 {
		p.Amp = d.Amp
	}

	periodFrames := p.Rate // exactly 1 s
	clickFrames := int(math.Round(p.ClickMs * 1e-3 * float64(p.Rate)))
	toneFrames := int(math.Round(p.ToneMs * 1e-3 * float64(p.Rate)))
	// Clamp pathological params so click+tone never overruns the 1 s period.
	if clickFrames < 1 {
		clickFrames = 1
	}
	if clickFrames > periodFrames {
		clickFrames = periodFrames
	}
	if toneFrames < 0 {
		toneFrames = 0
	}
	if clickFrames+toneFrames > periodFrames {
		toneFrames = periodFrames - clickFrames
	}

	s := &Signal{
		period:       make([]float32, periodFrames*p.Channels),
		rate:         p.Rate,
		channels:     p.Channels,
		periodFrames: periodFrames,
		clickFrames:  clickFrames,
		toneFrames:   toneFrames,
	}

	// Frame 0: a single +1.0 full-scale impulse (the sharpest, ear-safest, most
	// localizable transient — A.10b "single full-amplitude sample", R2). The rest
	// of the click window stays at 0 (the make() zero value).
	for ch := 0; ch < p.Channels; ch++ {
		s.period[ch] = 1.0
	}

	// Tone: phase 0 at the tone onset so every node's tone is phase-identical.
	twoPiFOverR := 2 * math.Pi * p.ToneHz / float64(p.Rate)
	for f := 0; f < toneFrames; f++ {
		v := float32(float64(p.Amp) * math.Sin(twoPiFOverR*float64(f)))
		base := (clickFrames + f) * p.Channels
		for ch := 0; ch < p.Channels; ch++ {
			s.period[base+ch] = v
		}
	}

	// Silence (the remainder) is already 0.
	return s
}

// Rate returns the canonical sample rate in Hz.
func (s *Signal) Rate() int { return s.rate }

// Channels returns the interleaved channel count.
func (s *Signal) Channels() int { return s.channels }

// PeriodFrames returns the per-channel frame count of one period (== Rate, 1 s).
func (s *Signal) PeriodFrames() int { return s.periodFrames }

// Period returns the precomputed interleaved 1 s period. The slice is the
// signal's own backing store: it is read-only by contract (callers must not
// mutate it; the signal is shared and immutable).
func (s *Signal) Period() []float32 { return s.period }

// Fill writes len(dst) interleaved samples into dst starting at absolute frame
// index fromSample, wrapping the 1 s period modulo PeriodFrames. len(dst) must be
// a multiple of Channels (it panics otherwise — a programmer error, mirroring the
// AudioSink Write contract). fromSample is the GROUP timeline sample, so the same
// group instant maps to the same point in the period on every node (this is what
// makes the click coincident). Pure, allocation-free, O(len(dst)) — the hot path
// during playback (no per-call Sin; the period is computed once in NewSignal).
func (s *Signal) Fill(dst []float32, fromSample int64) {
	if len(dst)%s.channels != 0 {
		panic("calib: Fill dst length not a multiple of Channels")
	}
	frames := len(dst) / s.channels
	pf := int64(s.periodFrames)
	// Reduce the absolute frame index into [0,pf) once; Go's % keeps the sign of
	// the dividend, so normalize a negative fromSample (defensive — group samples
	// are non-negative in practice).
	off := fromSample % pf
	if off < 0 {
		off += pf
	}
	src := int(off) * s.channels
	end := s.periodFrames * s.channels
	for i := 0; i < frames; i++ {
		copy(dst[i*s.channels:(i+1)*s.channels], s.period[src:src+s.channels])
		src += s.channels
		if src >= end {
			src = 0
		}
	}
}
