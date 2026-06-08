package calibrate

import (
	"math"
	"testing"

	"ensemble/internal/id"
)

// ---- reference signal -------------------------------------------------------

func TestReferenceGeometryAndEdges(t *testing.T) {
	r := NewReference(Config{SampleRate: 48000, SweepMs: 100, PeriodMs: 500, WindowMs: 5})
	if got, want := r.SweepLen(), 4800; got != want {
		t.Fatalf("sweep len = %d, want %d", got, want)
	}
	if got, want := r.Period, 24000; got != want {
		t.Fatalf("period = %d, want %d", got, want)
	}
	// raised-cosine edges → the sweep starts and ends at (near) zero, no click.
	if math.Abs(float64(r.Sweep[0])) > 1e-6 {
		t.Errorf("sweep[0] = %v, want ~0 (windowed)", r.Sweep[0])
	}
	if math.Abs(float64(r.Sweep[len(r.Sweep)-1])) > 1e-6 {
		t.Errorf("sweep[end] = %v, want ~0 (windowed)", r.Sweep[len(r.Sweep)-1])
	}
	// Loop is the sweep followed by silence to the period.
	loop := r.Loop()
	if len(loop) != r.Period {
		t.Fatalf("loop len = %d, want %d", len(loop), r.Period)
	}
	for i := r.SweepLen(); i < len(loop); i++ {
		if loop[i] != 0 {
			t.Fatalf("loop[%d] = %v, want silence after the sweep", i, loop[i])
		}
	}
}

func TestDefaultsFillZeroFields(t *testing.T) {
	r := NewReference(Config{}) // all zero → DefaultConfig
	if r.Cfg.SampleRate != 48000 || r.Cfg.F0 != 500 || r.Cfg.F1 != 8000 {
		t.Fatalf("defaults not applied: %+v", r.Cfg)
	}
}

// ---- delay estimation -------------------------------------------------------

// placeSweep builds a recording of `loops` periods with the sweep delayed by
// `delaySamples` within each period and optional white-ish noise added.
func placeSweep(r *Reference, loops, delaySamples int, noise float64) []float32 {
	rec := make([]float32, loops*r.Period)
	for k := 0; k < loops; k++ {
		base := k*r.Period + delaySamples
		for i, s := range r.Sweep {
			if base+i < len(rec) {
				rec[base+i] += s
			}
		}
	}
	if noise > 0 {
		// deterministic pseudo-noise (no rand import; stable test).
		seed := uint32(2463534242)
		for i := range rec {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			n := (float64(seed)/float64(math.MaxUint32) - 0.5) * 2 * noise
			rec[i] += float32(n)
		}
	}
	return rec
}

func TestEstimateRecoversKnownDelay(t *testing.T) {
	r := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	const delay = 1234
	rec := placeSweep(r, 4, delay, 0)

	est, ok := r.EstimateDelay(rec)
	if !ok {
		t.Fatal("EstimateDelay failed on a clean signal")
	}
	if math.Abs(est.LagSamples-float64(delay)) > 0.5 {
		t.Errorf("lag = %.3f, want ~%d", est.LagSamples, delay)
	}
	if est.Confidence < 0.8 {
		t.Errorf("confidence = %.3f, want high (clean signal)", est.Confidence)
	}
	if est.Loops < 3 {
		t.Errorf("loops = %d, want ≥3", est.Loops)
	}
}

func TestEstimateStraddlingPhase(t *testing.T) {
	// A sweep that begins near the very end of a loop period (straddling a naive
	// period boundary) must still be found — the arbitrary mic recording phase
	// makes this the common case, not the exception.
	r := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	delay := r.Period - r.SweepLen()/3 // sweep tail crosses into the next period
	rec := placeSweep(r, 5, delay, 0.02)
	est, ok := r.EstimateDelay(rec)
	if !ok {
		t.Fatal("estimate failed on a straddling sweep")
	}
	if math.Abs(est.LagSamples-float64(delay)) > 2 {
		t.Errorf("lag = %.2f, want ~%d", est.LagSamples, delay)
	}
}

func TestEstimateSubSamplePrecision(t *testing.T) {
	// A clean integer-delay signal should refine to within a small fraction of a
	// sample via parabolic interpolation (no bias away from the true integer).
	r := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	rec := placeSweep(r, 3, 800, 0)
	est, ok := r.EstimateDelay(rec)
	if !ok {
		t.Fatal("estimate failed")
	}
	if math.Abs(est.LagSamples-800) > 0.2 {
		t.Errorf("lag = %.4f, want ~800 (sub-sample)", est.LagSamples)
	}
}

func TestEstimateRobustToNoise(t *testing.T) {
	r := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	const delay = 950
	rec := placeSweep(r, 6, delay, 0.05) // 5% noise vs 0.5 sweep amplitude
	est, ok := r.EstimateDelay(rec)
	if !ok {
		t.Fatal("estimate failed under noise")
	}
	if math.Abs(est.LagSamples-float64(delay)) > 2 {
		t.Errorf("lag = %.2f, want ~%d (±2 under noise)", est.LagSamples, delay)
	}
}

func TestEstimateSilenceFails(t *testing.T) {
	r := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	rec := make([]float32, 3*r.Period) // pure silence
	if _, ok := r.EstimateDelay(rec); ok {
		t.Error("EstimateDelay should fail on silence")
	}
}

// ---- delay solve ------------------------------------------------------------

func idN(n byte) id.ID {
	var x id.ID
	x[15] = n
	return x
}

func appliedFor(sol Solution, n id.ID) (int, bool) {
	for _, a := range sol.Applied {
		if a.Node == n {
			return a.OutputDelayMs, true
		}
	}
	return 0, false
}

func TestSolveAdvanceWhenSpreadFitsBuffer(t *testing.T) {
	a, b, c := idN(1), idN(2), idN(3)
	delays := []NodeDelay{
		{Node: a, DelayMs: 10},
		{Node: b, DelayMs: 25},
		{Node: c, DelayMs: 40},
	}
	sol := Solve(delays, 150, 10) // spread 30 ≤ 150-10 → advance
	if sol.Mode != "advance" {
		t.Fatalf("mode = %q, want advance", sol.Mode)
	}
	// out = D − min(D): the slowest (c) advances most, fastest (a) unchanged.
	if v, _ := appliedFor(sol, a); v != 0 {
		t.Errorf("a = %d, want 0", v)
	}
	if v, _ := appliedFor(sol, b); v != 15 {
		t.Errorf("b = %d, want 15", v)
	}
	if v, _ := appliedFor(sol, c); v != 30 {
		t.Errorf("c = %d, want 30", v)
	}
}

func TestSolveDelayWhenSpreadExceedsBuffer(t *testing.T) {
	a, b := idN(1), idN(2)
	delays := []NodeDelay{
		{Node: a, DelayMs: 0},
		{Node: b, DelayMs: 300},
	}
	sol := Solve(delays, 150, 10) // spread 300 > 140 → delay
	if sol.Mode != "delay" {
		t.Fatalf("mode = %q, want delay", sol.Mode)
	}
	// out = D − max(D): slowest (b) unchanged, fastest (a) delayed by −spread.
	if v, _ := appliedFor(sol, a); v != -300 {
		t.Errorf("a = %d, want -300", v)
	}
	if v, _ := appliedFor(sol, b); v != 0 {
		t.Errorf("b = %d, want 0", v)
	}
}

func TestSolveClampsAndFlags(t *testing.T) {
	a, b := idN(1), idN(2)
	sol := Solve([]NodeDelay{{Node: a, DelayMs: 0}, {Node: b, DelayMs: 900}}, 150, 10)
	if !sol.Clamped {
		t.Error("expected Clamped=true for a 900 ms spread")
	}
	if v, _ := appliedFor(sol, a); v != -500 {
		t.Errorf("a = %d, want clamped -500", v)
	}
}

// ---- end-to-end math: generate → estimate → solve ---------------------------

func TestPipelineRecoversRelativeAlignment(t *testing.T) {
	r := NewReference(Config{SampleRate: 48000, SweepMs: 50, PeriodMs: 200})
	// three nodes with distinct true acoustic delays, in samples.
	trueDelay := map[id.ID]int{idN(1): 400, idN(2): 700, idN(3): 520}

	var delays []NodeDelay
	for n, ds := range trueDelay {
		rec := placeSweep(r, 5, ds, 0.02)
		est, ok := r.EstimateDelay(rec)
		if !ok {
			t.Fatalf("estimate failed for node %v", n)
		}
		ms := est.LagSamples / float64(r.Cfg.SampleRate) * 1000
		delays = append(delays, NodeDelay{Node: n, DelayMs: ms, Confidence: est.Confidence})
	}

	sol := Solve(delays, 150, 10)
	// The applied output delays must preserve the TRUE pairwise differences:
	// node with the larger acoustic delay gets the larger advance, and the
	// difference matches (true sample delta → ms) within a fraction of a ms.
	d1, _ := appliedFor(sol, idN(1))
	d2, _ := appliedFor(sol, idN(2))
	d3, _ := appliedFor(sol, idN(3))
	toMs := func(s int) float64 { return float64(s) / 48000 * 1000 }
	want12 := toMs(trueDelay[idN(2)] - trueDelay[idN(1)])
	got12 := float64(d2 - d1)
	if math.Abs(got12-want12) > 1 {
		t.Errorf("d2-d1 = %d ms, want ~%.2f ms", d2-d1, want12)
	}
	want13 := toMs(trueDelay[idN(3)] - trueDelay[idN(1)])
	got13 := float64(d3 - d1)
	if math.Abs(got13-want13) > 1 {
		t.Errorf("d3-d1 = %d ms, want ~%.2f ms", d3-d1, want13)
	}
}
