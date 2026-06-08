// Package calibrate implements acoustic auto-calibration of per-node output
// delays (docs/calibrate.md). It is split into a pure, deterministic core —
// the reference signal (this file), the matched-filter delay estimator
// (estimate.go), and the delay solver (solve.go) — and an orchestration layer
// (run.go) that drives a group through measurement with an injectable mic
// recorder and group controller. The core has no I/O and is unit-tested against
// synthetic recordings with known delays.
package calibrate

import (
	"math"

	"ensemble/internal/stream"
)

// Config parameterises the reference signal (docs/calibrate.md §3): a short,
// windowed logarithmic sine sweep ("chirp") followed by silence to the loop
// period. The sweep's near-impulsive autocorrelation gives a sharp, noise- and
// reverb-robust correlation peak.
type Config struct {
	SampleRate int     // Hz; defaults to stream.SampleRate (48 kHz)
	F0         float64 // sweep start frequency, Hz
	F1         float64 // sweep end frequency, Hz
	SweepMs    float64 // sweep duration, ms
	PeriodMs   float64 // loop period (sweep + trailing silence), ms
	WindowMs   float64 // raised-cosine edge length applied to each sweep end, ms
	Amplitude  float64 // peak sample amplitude in [0,1]
}

// DefaultConfig is the calibration signal used in practice: a 200 ms 500 Hz→
// 8 kHz sweep looped every 1000 ms at high scale, with 5 ms raised-cosine edges.
// The longer sweep raises matched-filter gain (better SNR in a real room); the
// 1 s period gives a clear gap between repeats.
func DefaultConfig() Config {
	return Config{
		SampleRate: stream.SampleRate,
		F0:         500,
		F1:         8000,
		SweepMs:    200,
		PeriodMs:   1000,
		WindowMs:   5,
		Amplitude:  0.7,
	}
}

// Reference is a generated reference signal: the windowed sweep template (used
// as the matched filter) plus the full-period loop geometry.
type Reference struct {
	Cfg    Config
	Sweep  []float32 // the windowed sweep — the matched-filter template
	Period int       // samples in one loop period (sweep + silence)
}

// NewReference builds the reference for cfg, filling unset fields from
// DefaultConfig. The sweep is a logarithmic chirp with raised-cosine edges so
// it starts and ends at zero (no click), which keeps its autocorrelation clean.
func NewReference(cfg Config) *Reference {
	d := DefaultConfig()
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = d.SampleRate
	}
	if cfg.F0 <= 0 {
		cfg.F0 = d.F0
	}
	if cfg.F1 <= 0 {
		cfg.F1 = d.F1
	}
	if cfg.SweepMs <= 0 {
		cfg.SweepMs = d.SweepMs
	}
	if cfg.PeriodMs <= 0 {
		cfg.PeriodMs = d.PeriodMs
	}
	if cfg.WindowMs < 0 {
		cfg.WindowMs = d.WindowMs
	}
	if cfg.Amplitude <= 0 {
		cfg.Amplitude = d.Amplitude
	}

	sr := float64(cfg.SampleRate)
	nSweep := int(math.Round(cfg.SweepMs / 1000 * sr))
	if nSweep < 2 {
		nSweep = 2
	}
	period := int(math.Round(cfg.PeriodMs / 1000 * sr))
	if period < nSweep {
		period = nSweep
	}
	wEdge := int(math.Round(cfg.WindowMs / 1000 * sr))
	if wEdge*2 > nSweep {
		wEdge = nSweep / 2
	}

	T := cfg.SweepMs / 1000 // sweep length, seconds
	L := math.Log(cfg.F1 / cfg.F0)
	k := 2 * math.Pi * cfg.F0 * T / L // instantaneous-phase constant

	sweep := make([]float32, nSweep)
	for i := range sweep {
		t := float64(i) / sr
		// logarithmic-sweep instantaneous phase (Farina): the frequency rises
		// exponentially from F0 to F1 across [0,T].
		phase := k * (math.Exp(t/T*L) - 1)
		s := cfg.Amplitude * math.Sin(phase)
		// raised-cosine (Hann) edges → zero start/end, no click transient.
		if wEdge > 0 {
			if i < wEdge {
				s *= 0.5 * (1 - math.Cos(math.Pi*float64(i)/float64(wEdge)))
			} else if i >= nSweep-wEdge {
				j := nSweep - 1 - i
				s *= 0.5 * (1 - math.Cos(math.Pi*float64(j)/float64(wEdge)))
			}
		}
		sweep[i] = float32(s)
	}

	return &Reference{Cfg: cfg, Sweep: sweep, Period: period}
}

// Loop returns one full period of the reference as mono float32 (the sweep
// followed by silence to the period length).
func (r *Reference) Loop() []float32 {
	out := make([]float32, r.Period)
	copy(out, r.Sweep)
	return out
}

// SweepLen reports the matched-filter template length in samples.
func (r *Reference) SweepLen() int { return len(r.Sweep) }
