package calibrate

import (
	"math"
	"sort"
)

// Estimate is one node's delay measurement (docs/calibrate.md §4): LagSamples
// is the sweep's position within the recording (the scheduled-emit→ear delay in
// the recording's own sample timeline), and Confidence in [0,1] grades the
// matched-filter peak (peak-to-sidelobe sharpness × consistency across loops).
type Estimate struct {
	LagSamples float64
	Confidence float64
	Loops      int // number of loop periods that contributed a peak
}

// EstimateDelay matched-filters the reference sweep against a recording that
// spans one or more loop periods and returns the median per-loop lag.
//
// The recording is assumed to begin aligned to a loop boundary (the orchestrator
// slices it that way). Each loop window is searched independently for the sweep;
// the per-loop lags are reduced by their median (robust to a transient that
// corrupts a single loop) and the spread feeds the confidence. Returns ok=false
// when no loop yields a usable peak.
func (r *Reference) EstimateDelay(rec []float32) (Estimate, bool) {
	period := r.Period
	if period <= 0 || len(rec) < len(r.Sweep) {
		return Estimate{}, false
	}

	// Search window inside each period: the sweep may arrive late by up to the
	// trailing silence, so scan the whole period (minus the template length).
	var lags []float64
	var confs []float64
	for start := 0; start+len(r.Sweep) <= len(rec); start += period {
		end := start + period
		if end > len(rec) {
			end = len(rec)
		}
		win := rec[start:end]
		lag, conf, ok := bestLag(win, r.Sweep)
		if !ok {
			continue
		}
		lags = append(lags, lag)
		confs = append(confs, conf)
	}
	if len(lags) == 0 {
		return Estimate{}, false
	}

	med := median(lags)
	// Consistency: how tightly the per-loop lags agree (1 sample spread ≈ 21 µs
	// at 48 kHz). A spread under ~1 ms is excellent; degrade linearly past that.
	spread := madAround(lags, med)
	oneMs := float64(r.Cfg.SampleRate) / 1000
	consistency := 1.0
	if oneMs > 0 {
		consistency = math.Max(0, 1-spread/oneMs)
	}
	peakConf := mean(confs)

	return Estimate{
		LagSamples: med,
		Confidence: peakConf * consistency,
		Loops:      len(lags),
	}, true
}

// bestLag slides the template over rec and returns the energy-normalised
// correlation peak's lag (with parabolic sub-sample refinement) and a
// peak-to-sidelobe confidence in [0,1]. Returns ok=false on a flat/degenerate
// correlation (silence, or a template longer than the window).
func bestLag(rec, tmpl []float32) (lag float64, confidence float64, ok bool) {
	m := len(tmpl)
	n := len(rec)
	if m == 0 || n < m {
		return 0, 0, false
	}

	var tmplEnergy float64
	for _, v := range tmpl {
		tmplEnergy += float64(v) * float64(v)
	}
	if tmplEnergy == 0 {
		return 0, 0, false
	}

	maxLag := n - m
	corr := make([]float64, maxLag+1)
	best := -1
	bestVal := math.Inf(-1)
	for k := 0; k <= maxLag; k++ {
		var dot, energy float64
		for i := 0; i < m; i++ {
			x := float64(rec[k+i])
			dot += x * float64(tmpl[i])
			energy += x * x
		}
		var c float64
		if energy > 0 {
			// normalised cross-correlation in [-1,1]; the energy term rejects a
			// loud-but-mismatched window (room noise, a neighbour bleeding in).
			c = dot / math.Sqrt(energy*tmplEnergy)
		}
		corr[k] = c
		if c > bestVal {
			bestVal = c
			best = k
		}
	}
	if best < 0 || bestVal <= 0 {
		return 0, 0, false
	}

	// Parabolic interpolation around the integer peak for sub-sample precision.
	lag = float64(best)
	if best > 0 && best < maxLag {
		y0, y1, y2 := corr[best-1], corr[best], corr[best+1]
		denom := y0 - 2*y1 + y2
		if denom != 0 {
			lag += 0.5 * (y0 - y2) / denom
		}
	}

	// Confidence = peak sharpness: how far the peak stands above the next-best
	// correlation outside its immediate neighbourhood (peak-to-sidelobe ratio,
	// mapped to [0,1]). A clean sweep peak towers over reverberant sidelobes.
	guard := m / 2
	sidelobe := 0.0
	for k := 0; k <= maxLag; k++ {
		if k >= best-guard && k <= best+guard {
			continue
		}
		if corr[k] > sidelobe {
			sidelobe = corr[k]
		}
	}
	confidence = bestVal - sidelobe
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return lag, confidence, true
}

func median(xs []float64) float64 {
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	n := len(c)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return c[n/2]
	}
	return 0.5 * (c[n/2-1] + c[n/2])
}

// madAround returns the median absolute deviation of xs about center.
func madAround(xs []float64, center float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	dev := make([]float64, len(xs))
	for i, x := range xs {
		dev[i] = math.Abs(x - center)
	}
	return median(dev)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
