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
	if elapsed <= 0 {
		return s.outPPM
	}
	if elapsed < s.cfg.WarmUp {
		return s.outPPM // 0 until warmed up
	}

	// What master-clock elapsed says the DAC *should* have emitted.
	wantSamples := float64(elapsed) * float64(stream.SampleRate) / 1e9
	gotSamples := emitted - s.baseEmit
	if wantSamples <= 0 {
		return s.outPPM
	}

	skewPPM := 1e6 * (gotSamples - wantSamples) / wantSamples

	dtSeconds := float64(elapsed) / 1e9
	s.integ += skewPPM * dtSeconds

	// Negative: DAC fast (skew>0) => slow production.
	raw := -(s.cfg.Kp*skewPPM + s.cfg.Ki*s.integ)

	// Clamp before slew.
	if raw > s.cfg.ClampPPM {
		raw = s.cfg.ClampPPM
	} else if raw < -s.cfg.ClampPPM {
		raw = -s.cfg.ClampPPM
	}

	// Slew toward raw by at most SlewPPM.
	delta := raw - s.outPPM
	if delta > s.cfg.SlewPPM {
		delta = s.cfg.SlewPPM
	} else if delta < -s.cfg.SlewPPM {
		delta = -s.cfg.SlewPPM
	}
	s.outPPM += delta

	// Slide the window: re-seed the baseline once the span exceeds Window.
	if elapsed >= s.cfg.Window {
		s.baseEmit = emitted
		s.baseMaster = masterNanos
	}

	return s.outPPM
}

// ratePPM returns the last correction (for SinkStats.RatePPM).
func (s *rateServo) ratePPM() float64 { return s.outPPM }

// reset clears baseline + integral for a new session.
func (s *rateServo) reset() {
	s.have = false
	s.baseEmit = 0
	s.baseMaster = 0
	s.integ = 0
	s.outPPM = 0
}
