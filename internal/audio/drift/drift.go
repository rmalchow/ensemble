// Package drift is the corrected content-domain PI drift loop: it maps a playout
// error (in canonical-rate samples) to a near-unity resample ratio (the actuator)
// or a DriftReseek signal on gross error.
//
// It is a leaf package (stdlib only). The P4.4 renderer (internal/audio/render)
// owns the plant — it assembles the content-domain errSamples (06 §3.3) and applies
// the returned ratio to the resampler — and drives this loop once per control tick.
// See doc 06 §3 and Appendix A.4 (algorithm) / A.12 (tunables).
package drift

import "math"

// DriftParams tunes the audio drift PI controller. The actuator is the resample
// ratio, never playback speed. Defaults are canonical in Appendix A.12; see
// doc 06 §3 / A.4 for the control law.
type DriftParams struct {
	Kp            float64 // proportional gain, samples->ppm. Default 0.05
	Ki            float64 // integral gain, samples->ppm per tick. Default 0.005
	MaxPPM        float64 // ratio-trim clamp, ppm. Default 200
	HardErrSamp   int     // |error| above this triggers reseek. Default 2400 (=50ms@48k)
	IntegralClamp float64 // anti-windup clamp on the integral term, ppm. Default 200
}

// DefaultDriftParams returns the tuned defaults. The gains are chosen so a steady
// tens-of-ppm crystal drift nulls in a few seconds at a 20 ms tick without
// overshoot; the ±200 ppm clamp guarantees the actuator can never make an audible
// pitch shift. Raise Kp for faster convergence at the cost of more ratio chatter;
// Ki removes the standing offset; IntegralClamp keeps Ki*integral well under MaxPPM.
func DefaultDriftParams() DriftParams {
	return DriftParams{
		Kp:            0.05,
		Ki:            0.005,
		MaxPPM:        200,
		HardErrSamp:   2400,
		IntegralClamp: 200,
	}
}

// DriftAction is what the control law selects for an error.
type DriftAction int

const (
	DriftHold   DriftAction = iota // apply ratio trim, keep integrating
	DriftReseek                    // gross error: caller must Seek + refill + Reset
)

func (a DriftAction) String() string {
	if a == DriftReseek {
		return "reseek"
	}
	return "hold"
}

// DriftLoop is the stateful PI controller (holds the integral term). One per renderer.
type DriftLoop struct {
	p        DriftParams
	integral float64 // accumulated error in samples
}

// NewDriftLoop builds a PI loop with the given params. Zero/invalid params fall
// back to DefaultDriftParams so a partially-filled struct stays sane.
func NewDriftLoop(p DriftParams) *DriftLoop {
	if p.MaxPPM <= 0 {
		p.MaxPPM = 200
	}
	if p.IntegralClamp <= 0 {
		p.IntegralClamp = 200
	}
	if p.HardErrSamp <= 0 {
		p.HardErrSamp = 2400
	}
	return &DriftLoop{p: p}
}

// Update takes the current error in SAMPLES (error = playedContent - wantContent;
// positive => we are ahead, must slow down => ratio < 1) and returns the action
// plus the resample ratio to apply (1 ± clamp). On DriftReseek the ratio is 1.0 and
// the caller must reseek; the loop does NOT integrate the gross error. The caller
// resets the integral on the next reseek via Reset().
func (d *DriftLoop) Update(errSamples int) (DriftAction, float64) {
	// Gross error first: do not integrate, hand the reseek to the caller.
	if abs(errSamples) > d.p.HardErrSamp {
		return DriftReseek, 1.0
	}

	// Integrate, then clamp the integral term (Ki*integral) to ±IntegralClamp ppm
	// for anti-windup.
	d.integral += float64(errSamples)
	if d.p.Ki != 0 {
		maxInt := d.p.IntegralClamp / d.p.Ki
		if d.integral > maxInt {
			d.integral = maxInt
		} else if d.integral < -maxInt {
			d.integral = -maxInt
		}
	}

	// Negative sign: positive error => ahead => negative ppm => ratio < 1.
	ppm := -(d.p.Kp*float64(errSamples) + d.p.Ki*d.integral)
	ppm = d.p.Clamp(ppm)
	ratio := 1 + ppm*1e-6
	return DriftHold, ratio
}

// Reset zeroes the integral term (after a reseek / underrun).
func (d *DriftLoop) Reset() {
	d.integral = 0
}

// IntegralPPM returns the integral term's current contribution to the ratio trim,
// in ppm (Ki·integral). It is the diagnostic the convergence harness asserts on
// (doc 06 §3.4/§3.6): with the corrected content-domain loop it settles to a
// FINITE, NON-clamped value equal to the standing crystal drift; under the mpvsync
// output-domain bug it pinned at IntegralClamp. Also surfaced to the UI status path.
func (d *DriftLoop) IntegralPPM() float64 {
	return d.p.Ki * d.integral
}

// Clamp limits a ppm trim to ±MaxPPM. Exported helper, table-tested.
func (p DriftParams) Clamp(ppm float64) float64 {
	return math.Max(-p.MaxPPM, math.Min(p.MaxPPM, ppm))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
