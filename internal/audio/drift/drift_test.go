package drift

import (
	"math"
	"testing"
)

func TestDefaultDriftParams(t *testing.T) {
	p := DefaultDriftParams()
	if p.Kp != 0.05 || p.Ki != 0.005 || p.MaxPPM != 200 || p.HardErrSamp != 2400 || p.IntegralClamp != 200 {
		t.Fatalf("unexpected defaults: %+v", p)
	}
}

func TestDriftActionString(t *testing.T) {
	if DriftHold.String() != "hold" {
		t.Errorf("DriftHold.String()=%q", DriftHold.String())
	}
	if DriftReseek.String() != "reseek" {
		t.Errorf("DriftReseek.String()=%q", DriftReseek.String())
	}
}

func TestClamp(t *testing.T) {
	p := DefaultDriftParams() // MaxPPM=200
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{100, 100},
		{-100, -100},
		{200, 200},
		{-200, -200},
		{250, 200},
		{-250, -200},
		{1e9, 200},
		{-1e9, -200},
	}
	for _, c := range cases {
		if got := p.Clamp(c.in); got != c.want {
			t.Errorf("Clamp(%g)=%g want %g", c.in, got, c.want)
		}
	}
}

func TestGrossErrorTriggersReseek(t *testing.T) {
	d := NewDriftLoop(DefaultDriftParams())
	// Prime the integral with some normal updates first.
	d.Update(10)
	before := d.integral

	for _, err := range []int{2401, -2401, 50000, -50000} {
		act, ratio := d.Update(err)
		if act != DriftReseek {
			t.Errorf("Update(%d) action=%v want DriftReseek", err, act)
		}
		if ratio != 1.0 {
			t.Errorf("Update(%d) ratio=%g want 1.0", err, ratio)
		}
	}
	// Gross error must NOT integrate.
	if d.integral != before {
		t.Errorf("integral changed on gross error: before=%g after=%g", before, d.integral)
	}

	// Exactly at the threshold is NOT gross (> is the boundary).
	if act, _ := d.Update(2400); act != DriftHold {
		t.Errorf("Update(HardErrSamp) action=%v want DriftHold", act)
	}
}

func TestBoundedActuator(t *testing.T) {
	d := NewDriftLoop(DefaultDriftParams())
	// Any single (non-gross) error must yield ratio within ±MaxPPM => [1-2e-4,1+2e-4].
	lo, hi := 1-2e-4, 1+2e-4
	for err := -2400; err <= 2400; err += 17 {
		d.Reset()
		act, ratio := d.Update(err)
		if act != DriftHold {
			continue
		}
		if ratio < lo-1e-12 || ratio > hi+1e-12 {
			t.Errorf("Update(%d) ratio=%g out of [%g,%g]", err, ratio, lo, hi)
		}
	}
}

func TestSignCorrectness(t *testing.T) {
	d := NewDriftLoop(DefaultDriftParams())

	// Positive error => ahead => must slow down => ratio < 1.
	d.Reset()
	_, ratio := d.Update(100)
	if ratio >= 1.0 {
		t.Errorf("positive error: ratio=%g want <1", ratio)
	}

	// Negative error => behind => must speed up => ratio > 1.
	d.Reset()
	_, ratio = d.Update(-100)
	if ratio <= 1.0 {
		t.Errorf("negative error: ratio=%g want >1", ratio)
	}

	// Zero error => ratio == 1.
	d.Reset()
	_, ratio = d.Update(0)
	if ratio != 1.0 {
		t.Errorf("zero error: ratio=%g want 1.0", ratio)
	}
}

// realPlantSim runs the corrected content-domain plant of doc 06 §3.2/§3.6 for one
// constant crystal offset and returns the steady-state diagnostics. It is the
// harness, NOT production code.
//
// Plant model (06 §3.6 / A.4):
//   - The DAC drains output at a FIXED crystal rate f_out = Rate*(1+crystalPPM*1e-6),
//     independent of the commanded ratio (the crystal is the disturbance to reject).
//   - The resampler consumes `ratio` content frames per output frame, so source content
//     is consumed at ratio*f_out per second.
//   - The buffered backlog (the source-equivalent of the ring+device backlog) evolves
//     from the producer/consumer rate difference, and is subtracted from sourceConsumed
//     to form playedContent exactly as the renderer does (06 §3.3), closing the loop.
//   - wantContent advances at the NOMINAL Rate per tick (the group timeline), 960@48k.
//
// This models the regulated variable (playedContent) as genuinely controllable by the
// ratio — d(playedContent)/dt = ratio*f_out — while the DAC drain f_out is the fixed,
// uncontrollable disturbance. TestRejectOutputDomainModel proves the discriminating
// property: feeding the loop the OUTPUT-domain played (the mpvsync §3.1 bug, derivative
// = f_out, ratio-independent) fails to converge here, so this harness — unlike a toy
// that lets the actuator drive the regulated variable directly — actually catches it.
func realPlantSim(d *DriftLoop, crystalPPM float64, ticks int) (finalErr int, finalRatio float64, finalIntegral float64, reseek bool) {
	const (
		rate    = 48000.0
		tickSec = 0.020
		nominal = rate * tickSec // 960 frames per tick at the nominal group rate
		lead    = 0.300 * rate   // LeadMs=300 => target OUTPUT-domain backlog (ring+device)
	)
	fOut := rate * (1 + crystalPPM*1e-6) // FIXED DAC drain rate (frames/sec), the disturbance

	// outBacklog is the OUTPUT-domain backlog (ringFrames+deviceDelayFrames) of 06 §3.3.
	// sourceConsumed is the CONTENT-domain cumulative input the resampler has eaten.
	// The renderer baselines playedContent and wantContent to share an origin at the
	// reseek (06 §3.3) so the pre-buffered LeadMs is NOT a standing error: at startup
	// played == sourceConsumed - outBacklog*ratio == -lead, and wantContent's origin
	// carries the same -lead (the hwDelayOffset/lead places the timeline at the speaker).
	outBacklog := lead
	sourceConsumed := 0.0
	want := -lead // shared origin: zero drift => zero steady error
	ratio := 1.0

	for i := 0; i < ticks; i++ {
		// The DAC drains output at its fixed crystal rate, independent of ratio.
		drainedOut := fOut * tickSec
		outBacklog -= drainedOut

		// The renderer keeps the output ring fed: it produces output frames to refill
		// what the DAC drained (steady refill toward `lead`). Producing one output frame
		// costs `ratio` content frames at the resampler input — so the ratio sits in the
		// time-derivative of sourceConsumed (06 §3.2), which is what makes the regulated
		// variable controllable. Refill output 1:1 with the drain so the output backlog
		// is held near LeadMs (producer rate = consumer rate = fOut in output frames),
		// matching d(sourceEquivalentBuffered)/dt ≈ 0 at steady state.
		producedOut := drainedOut
		outBacklog += producedOut
		sourceConsumed += ratio * producedOut // content frames consumed to make that output

		// 06 §3.3: source-equivalent of the downstream output backlog, content domain.
		// ratio_content_per_output is taken as 1.0 — doc 06 §3.2 explicitly sanctions this
		// ("using 1.0 here is acceptable since |ratio-1| <= 200 ppm makes the conversion
		// error on a few-hundred-ms backlog sub-sample"). It also keeps the regulated
		// variable's only ratio dependence in d(sourceConsumed)/dt = ratio*f_out, which is
		// exactly the controllability the loop relies on.
		sourceEquivalentBuffered := outBacklog * 1.0
		played := sourceConsumed - sourceEquivalentBuffered
		want += nominal
		finalErr = int(math.Round(played - want))

		act, r := d.Update(finalErr)
		if act == DriftReseek {
			reseek = true
			d.Reset() // mimic the caller's 06 §6.1 reseek
		}
		ratio = r
	}
	return finalErr, ratio, d.integral, reseek
}

func TestConvergence(t *testing.T) {
	// The central P4.5 deliverable: the corrected content-domain loop must null a
	// steady crystal disturbance to a sub-ms standing offset, settling the commanded
	// ratio at 1-crystalPPM*1e-6 (NOT at the ±MaxPPM clamp), with a finite integrator
	// and no recurring reseek. See doc 06 §3.6 / A.4. A sign-flipped loop or an
	// output-domain `played` MUST fail this (TestRejectOutputDomainModel documents why).
	// 300k ticks of a pure float loop runs in tens of ms even on a Pi-class core. The
	// horizon is wide (not the bound loose) per the impl doc §9.3: the integrator needs
	// O(crystalPPM/Ki) ticks to wind to the exact trim for the larger ±100 ppm cases.
	const ticks = 300_000

	cases := []struct {
		name       string
		crystalPPM float64
	}{
		{"plus40", +40},
		{"minus40", -40},
		{"plus100", +100},
		{"minus100", -100},
		{"zero", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewDriftLoop(DefaultDriftParams())
			finalErr, finalRatio, integral, reseek := realPlantSim(d, c.crystalPPM, ticks)

			// 1. Converges to sub-ms (<48 samples @48k).
			if abs(finalErr) >= 48 {
				t.Fatalf("did not converge: final error=%d samples (want <48)", finalErr)
			}

			// 2. Ratio settles to oppose the crystal at a NON-clamped value.
			wantRatio := 1 - c.crystalPPM*1e-6
			if math.Abs(finalRatio-wantRatio) >= 5e-6 {
				t.Fatalf("ratio settled at %.9f want ~%.9f (1-crystalPPM)", finalRatio, wantRatio)
			}
			// Guard: must NOT be riding the clamp (±MaxPPM => ratio 1±2e-4).
			if math.Abs(finalRatio-1) >= 2e-4-1e-9 {
				t.Fatalf("ratio %.9f is at/near the ±MaxPPM clamp; drift not nulled by the integrator", finalRatio)
			}

			// 3. Integrator finite and below the anti-windup clamp at steady state.
			p := DefaultDriftParams()
			if !math.IsInf(integral, 0) && !math.IsNaN(integral) {
				if term := math.Abs(p.Ki * integral); term >= p.IntegralClamp {
					t.Fatalf("Ki*integral=%g at/over IntegralClamp=%g (integrator pinned, not settled)", term, p.IntegralClamp)
				}
			} else {
				t.Fatalf("integrator non-finite: %g", integral)
			}

			// 4. No reseek after settling.
			if reseek {
				t.Fatalf("unexpected reseek during steady-state run (crystalPPM=%g)", c.crystalPPM)
			}

			// 5. Sign sanity.
			switch {
			case c.crystalPPM > 0 && finalRatio >= 1.0:
				t.Fatalf("crystalPPM>0 but steady ratio=%.9f >= 1 (must slow down)", finalRatio)
			case c.crystalPPM < 0 && finalRatio <= 1.0:
				t.Fatalf("crystalPPM<0 but steady ratio=%.9f <= 1 (must speed up)", finalRatio)
			case c.crystalPPM == 0 && math.Abs(finalRatio-1) >= 5e-6:
				t.Fatalf("crystalPPM==0 but steady ratio=%.9f != 1", finalRatio)
			}
		})
	}
}

// TestRejectOutputDomainModel is the executable negative control demanded by 06 §3.6.
//
// The §3.1 bug is one of CONTROLLABILITY, not sign: when the regulated variable is the
// OUTPUT-domain playout position, its time-derivative is the DAC drain rate f_out — which
// is fixed by the crystal and entirely OUTSIDE the controller's authority (the resample
// ratio cannot move how fast the DAC pulls frames). The loop has zero loop gain over that
// variable: the ratio actuator does nothing, the error keeps growing with the crystal
// offset, and the system "recovers" only by RECURRING reseeks — the §3.5 regression signal.
//
// The corrected fix (§3.2/§3.3) regulates the CONTENT-domain playedContent, whose
// derivative is ratio·f_out — the ratio sits directly in it, so the actuator has authority
// and the loop converges with no reseeks. This test runs the REAL drift loop against BOTH
// formulations of `played` on the SAME fixed-crystal plant and asserts:
//
//	output-domain  => never converges, keeps reseeking          (rejected)
//	content-domain => converges sub-ms, never reseeks            (accepted)
//
// proving the harness is discriminating: it would FAIL a loop fed an output-domain error.
func TestRejectOutputDomainModel(t *testing.T) {
	const (
		rate    = 48000.0
		tickSec = 0.020
		nominal = rate * tickSec
		lead    = 0.300 * rate
		// At +40 ppm the uncontrollable output-domain error grows ~0.0384 frame/tick, so it
		// crosses HardErrSamp (2400) near tick ~62.5k and then reseeks repeatedly. Run well
		// past that so the recurring-reseek regression signal is observable.
		ticks = 200_000
		drift = 40.0 // ppm; positive crystal
	)

	// run drives the loop for `ticks`, choosing the regulated variable by `contentDomain`.
	// It returns the final error magnitude and how many reseeks the plant forced.
	run := func(contentDomain bool) (finalErr, reseeks int) {
		d := NewDriftLoop(DefaultDriftParams())
		fOut := rate * (1 + drift*1e-6)
		outBacklog := lead
		sourceConsumed := 0.0
		outputPlayed := -lead // cumulative OUTPUT frames played, baselined to the shared origin
		want := -lead         // shared origin (see realPlantSim)
		ratio := 1.0
		for i := 0; i < ticks; i++ {
			drainedOut := fOut * tickSec
			outputPlayed += drainedOut             // advances at f_out, ratio-INDEPENDENT
			outBacklog += drainedOut - drainedOut  // steady refill: held at lead
			sourceConsumed += ratio * drainedOut   // content eaten to make that output

			var played float64
			if contentDomain {
				played = sourceConsumed - outBacklog*1.0 // §3.3 (controllable)
			} else {
				played = outputPlayed // §3.1 bug: output-domain (uncontrollable)
			}
			want += nominal
			finalErr = int(math.Round(played - want))
			act, r := d.Update(finalErr)
			if act == DriftReseek {
				reseeks++
				// Mimic the caller's reseek: re-baseline progress + reset the integral. In
				// the output-domain plant the disturbance is untouched, so it reseeks again.
				outputPlayed = want
				sourceConsumed = outBacklog
				d.Reset()
			}
			ratio = r
		}
		return abs(finalErr), reseeks
	}

	// Output-domain formulation: the §3.1 bug. The actuator has no authority, so the loop
	// keeps reseeking and never holds a sub-ms offset.
	if err, reseeks := run(false); reseeks == 0 || err < 48 {
		t.Fatalf("output-domain model unexpectedly behaved: |finalErr|=%d reseeks=%d "+
			"(expected recurring reseeks and no convergence — the uncontrollable §3.1 bug)", err, reseeks)
	}

	// Content-domain formulation: the §3.2 fix. Converges sub-ms with zero reseeks.
	if err, reseeks := run(true); err >= 48 || reseeks != 0 {
		t.Fatalf("content-domain model failed: |finalErr|=%d reseeks=%d "+
			"(expected sub-ms convergence with no reseeks)", err, reseeks)
	}
}

func TestAntiWindup(t *testing.T) {
	p := DefaultDriftParams()
	d := NewDriftLoop(p)

	// A long run of same-sign error must keep Ki*integral clamped at ±IntegralClamp.
	for i := 0; i < 100000; i++ {
		d.Update(100) // constant positive error
	}
	term := p.Ki * d.integral
	if math.Abs(term) > p.IntegralClamp+1e-9 {
		t.Fatalf("Ki*integral=%g exceeds IntegralClamp=%g", term, p.IntegralClamp)
	}
	// It should be pinned right at the clamp (positive error integrates positive).
	if math.Abs(term-p.IntegralClamp) > 1e-6 {
		t.Fatalf("Ki*integral=%g not pinned at clamp %g", term, p.IntegralClamp)
	}

	// Negative direction.
	d.Reset()
	for i := 0; i < 100000; i++ {
		d.Update(-100)
	}
	term = p.Ki * d.integral
	if math.Abs(term) > p.IntegralClamp+1e-9 {
		t.Fatalf("Ki*integral=%g exceeds IntegralClamp=%g (neg)", term, p.IntegralClamp)
	}
	if math.Abs(term+p.IntegralClamp) > 1e-6 {
		t.Fatalf("Ki*integral=%g not pinned at -clamp %g", term, p.IntegralClamp)
	}
}

func TestReset(t *testing.T) {
	d := NewDriftLoop(DefaultDriftParams())
	for i := 0; i < 50; i++ {
		d.Update(200)
	}
	if d.integral == 0 {
		t.Fatal("integral did not accumulate")
	}
	d.Reset()
	if d.integral != 0 {
		t.Fatalf("integral=%g after Reset, want 0", d.integral)
	}
	// After reset, a zero error yields exactly ratio 1.0 (no residual integral term).
	if _, ratio := d.Update(0); ratio != 1.0 {
		t.Fatalf("post-reset zero-error ratio=%g want 1.0", ratio)
	}
}

func TestNewDriftLoopDefaultsBadParams(t *testing.T) {
	// A partially-filled / zeroed struct must fall back to sane clamps so it
	// cannot divide-by-zero or produce an unbounded actuator.
	d := NewDriftLoop(DriftParams{Kp: 0.1, Ki: 0.01})
	if d.p.MaxPPM != 200 || d.p.IntegralClamp != 200 || d.p.HardErrSamp != 2400 {
		t.Fatalf("bad-param fallback not applied: %+v", d.p)
	}
	// Update must still be bounded.
	_, ratio := d.Update(1000)
	if ratio < 1-2e-4-1e-12 || ratio > 1+2e-4+1e-12 {
		t.Fatalf("ratio=%g out of bounds with fallback params", ratio)
	}
}

func TestZeroKiNoWindup(t *testing.T) {
	// With Ki=0 the integral term must not affect output (pure-P), and no NaN/Inf
	// from a divide in the anti-windup clamp.
	d := NewDriftLoop(DriftParams{Kp: 0.05, Ki: 0, MaxPPM: 200, HardErrSamp: 2400, IntegralClamp: 200})
	for i := 0; i < 1000; i++ {
		d.Update(100)
	}
	_, ratio := d.Update(0)
	if ratio != 1.0 {
		t.Fatalf("Ki=0 with zero error: ratio=%g want 1.0 (integral must not act)", ratio)
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		t.Fatalf("non-finite ratio with Ki=0")
	}
}

func TestAbsHelper(t *testing.T) {
	cases := map[int]int{0: 0, 5: 5, -5: 5, math.MaxInt32: math.MaxInt32, -math.MaxInt32: math.MaxInt32}
	for in, want := range cases {
		if got := abs(in); got != want {
			t.Errorf("abs(%d)=%d want %d", in, got, want)
		}
	}
}
