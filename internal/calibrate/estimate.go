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

// maxLoopsScanned caps how many loop repeats contribute to the median (enough
// for a robust estimate without scanning an arbitrarily long recording).
const maxLoopsScanned = 8

// EstimateDelay matched-filters the reference sweep against a recording that
// spans one or more loop periods and returns the median per-loop lag.
//
// The recording starts at an ARBITRARY phase relative to the master's loop, so a
// sweep may straddle a naive period boundary. Instead, a first bounded search
// over one period-plus-template window locates the strongest occurrence; the
// remaining loops are then read off the same loop grid (each window holds exactly
// one full sweep at ~the same lag), giving a median that is robust to a transient
// corrupting a single loop, with the cross-loop spread feeding the confidence.
// The bounded first search also keeps the correlation cost ~one period, not the
// whole recording. Returns ok=false when no loop yields a usable peak.
func (r *Reference) EstimateDelay(rec []float32) (Estimate, bool) {
	period := r.Period
	m := len(r.Sweep)
	if period <= 0 || m == 0 || len(rec) < m {
		return Estimate{}, false
	}

	// one period plus the template length guarantees the window contains exactly
	// one full sweep regardless of phase, and excludes the next loop's repeat
	// from the search range (so it isn't mistaken for a sidelobe).
	win := period + m
	if win > len(rec) {
		win = len(rec)
	}

	g, conf, ok := bestLag(rec[:win], r.Sweep)
	if !ok {
		return Estimate{}, false
	}
	lags := []float64{g}
	confs := []float64{conf}

	// subsequent loops, aligned to the loop grid anchored at the first window.
	for k := 1; len(lags) < maxLoopsScanned; k++ {
		start := k * period
		if start+win > len(rec) {
			break
		}
		lag, c, ok := bestLag(rec[start:start+win], r.Sweep)
		if !ok {
			continue
		}
		lags = append(lags, lag)
		confs = append(confs, c)
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

	return Estimate{
		LagSamples: med,
		Confidence: mean(confs) * consistency,
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
