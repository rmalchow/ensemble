package sink

import "ensemble/internal/stream"

// servoConfig holds the PI gains and limits. Defaults are tuned for a ~3 s skew
// window and gentle correction; tests override for fast convergence.
type servoConfig struct {
	Window   int64   // skew-averaging window, ns (default 3e9 = 3 s)
	WarmUp   int64   // min elapsed before emitting non-zero correction, ns
	Kp       float64 // proportional gain (ppm per ppm of measured skew)
	Ki       float64 // integral gain (ppm per (ppm·s) of accumulated skew)
	ClampPPM float64 // output clamp, ± (default 500, §8.5)
	SlewPPM  float64 // max |Δoutput| per update, ppm
}

func defaultServoConfig() servoConfig {
	return servoConfig{
		Window:   3_000_000_000, // 3 s
		WarmUp:   200_000_000,   // 200 ms
		Kp:       0.6,           // user-requested: more aggressive correction
		Ki:       0.1,
		ClampPPM: 2000, // ±0.2% — still far below audibility
		SlewPPM:  20,   // reaches the clamp in ~2s of slots instead of ~20s
	}
}

// rateServo turns a stream of (samplesConsumed, masterElapsed[, deviceDelay])
// observations into a playback-rate correction in ppm. One per session; reset
// on generation change. Pure: no goroutine, no clock, no locking (the Playout
// mutex guards it).
type rateServo struct {
	cfg        servoConfig
	have       bool    // a baseline has been established
	baseEmit   float64 // cumulative emitted (queue-adjusted) samples at window start
	baseMaster int64   // master-clock ns at window start
	integ      float64 // PI integral accumulator, ppm·s
	seeded     bool    // the window has fully slid once: startup transient aged out
	skewEMA    float64 // smoothed windowed skew (Wi-Fi clock jitter damping)
	lastSkew   float64 // most recent measured skew, ppm (debug telemetry)
	lastGot    float64 // window emitted samples (debug)
	lastWant   float64 // window expected samples (debug)
	outPPM     float64 // last emitted correction, ppm (slew-limited, clamped)
}

func newRateServo(cfg servoConfig) *rateServo {
	if cfg.Window <= 0 {
		cfg = defaultServoConfig()
	}
	return &rateServo{cfg: cfg}
}

// observe folds one measurement and returns the updated correction in ppm.
//
//	consumedSamples : cumulative samples per channel the backend has consumed
//	                  since session start (monotonic).
//	masterNanos     : current master-clock time (ns) for the same instant.
//	deviceDelayNs,ok: queued audio between Write and speaker if the backend is a
//	                  DelayReporter; ok=false => backpressure inference.
//
// A positive return means "produce samples faster" (resample ratio > 1).
func (s *rateServo) observe(consumedSamples, masterNanos, deviceDelayNs int64, ok bool) float64 {
	// "emitted" is what the speaker has actually heard: written samples minus the
	// still-queued audio (when the backend reports its delay). Baselining on
	// emitted (not raw consumed) keeps a constant device queue from biasing the
	// skew — only the rate matters (§3.5).
	emitted := float64(consumedSamples)
	if ok {
		emitted -= float64(deviceDelayNs) * float64(stream.SampleRate) / 1e9
	}

	if !s.have {
		s.baseEmit = emitted
		s.baseMaster = masterNanos
		s.have = true
		return s.outPPM // 0
	}

	elapsed := masterNanos - s.baseMaster
	if elapsed < s.cfg.Window {
		return s.outPPM // accumulate; one PI step per full window (see below)
	}

	// One servo step per full window. Early-window skew is garbage — with a
	// small denominator, single-slot quantization reads as tens of thousands
	// of ppm and (clamp-asymmetrically) poisoned the integral. Over the full
	// 3 s span the slot quantization is < ±7 ppm. A crystal-drift servo needs
	// 0.33 Hz updates, not 50 Hz.
	wantSamples := float64(elapsed) * float64(stream.SampleRate) / 1e9
	gotSamples := emitted - s.baseEmit
	rawSkew := 1e6 * (gotSamples - wantSamples) / wantSamples
	// Outlier rejection: no crystal is >800ppm off — a spike that size is a
	// SCHEDULER artifact (late-skipped slots remove whole frames from 'got';
	// e.g. 4 skips in one window read as −9300ppm and whipsawed the output).
	// Measure-and-discard: re-seed the window, keep the controller state.
	if s.seeded && (rawSkew > 800 || rawSkew < -800) {
		s.lastSkew = rawSkew
		s.lastGot = gotSamples
		s.lastWant = wantSamples
		s.baseEmit = emitted
		s.baseMaster = masterNanos
		return s.outPPM
	}
	// EMA across window steps: on high-RTT links (Wi-Fi) the clock offset
	// jitters by hundreds of µs, which reads as ±200ppm of fake per-window
	// skew. True crystal skew is constant — smooth before the PI step.
	if !s.seeded {
		s.skewEMA = rawSkew
	} else {
		s.skewEMA += 0.35 * (rawSkew - s.skewEMA)
	}
	skewPPM := s.skewEMA
	s.lastSkew = skewPPM
	s.lastGot = gotSamples
	s.lastWant = wantSamples

	// Re-seed the window.
	s.baseEmit = emitted
	s.baseMaster = masterNanos

	// The FIRST window contains the session-start transient (deadline hold,
	// catch-up burst): measure-and-discard.
	if !s.seeded {
		s.seeded = true
		return s.outPPM
	}

	// PI step over this window, with: input clamp (transients must not dump
	// charge), anti-windup, and a slow integral leak so any residual
	// poisoning self-heals instead of holding the output off-zero forever.
	dtSeconds := float64(elapsed) / 1e9
	in := skewPPM
	if in > s.cfg.ClampPPM {
		in = s.cfg.ClampPPM
	} else if in < -s.cfg.ClampPPM {
		in = -s.cfg.ClampPPM
	}
	const leakTau = 60.0 // seconds (slow heal; small steady-state residual)
	s.integ += in*dtSeconds - s.integ*(dtSeconds/leakTau)
	if maxInteg := s.cfg.ClampPPM / s.cfg.Ki; s.integ > maxInteg {
		s.integ = maxInteg
	} else if s.integ < -s.cfg.ClampPPM/s.cfg.Ki {
		s.integ = -s.cfg.ClampPPM / s.cfg.Ki
	}

	// Negative: DAC fast (skew>0) => slow production.
	raw := -(s.cfg.Kp*skewPPM + s.cfg.Ki*s.integ)
	if raw > s.cfg.ClampPPM {
		raw = s.cfg.ClampPPM
	} else if raw < -s.cfg.ClampPPM {
		raw = -s.cfg.ClampPPM
	}

	// Slew toward raw — per WINDOW step now, so allow a meaningful move.
	maxStep := s.cfg.SlewPPM * float64(elapsed) / 50_000_000 // SlewPPM per 50ms-slot-equivalent
	delta := raw - s.outPPM
	if delta > maxStep {
		delta = maxStep
	} else if delta < -maxStep {
		delta = -maxStep
	}
	s.outPPM += delta

	return s.outPPM
}

// ratePPM returns the last correction (for SinkStats.RatePPM).
func (s *rateServo) ratePPM() float64 { return s.outPPM }

// reset clears baseline + integral for a new session.
func (s *rateServo) reset() {
	s.have = false
	s.baseEmit = 0
	s.baseMaster = 0
	s.seeded = false
	s.skewEMA = 0
	s.integ = 0
	s.outPPM = 0
}
