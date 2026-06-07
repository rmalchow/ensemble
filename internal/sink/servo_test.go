package sink

import (
	"math"
	"testing"

	"ensemble/internal/stream"
)

// fastServoCfg converges quickly for tests.
func fastServoCfg() servoConfig {
	return servoConfig{
		Window:   3_000_000_000,
		WarmUp:   100_000_000,
		Kp:       0.5,
		Ki:       0.2,
		ClampPPM: 500,
		SlewPPM:  50,
	}
}

// driveServo runs a CLOSED-LOOP simulation: a DAC consumes output samples at
// dacPPM ppm off nominal, while the servo's correction sets the rate at which
// the resampler PRODUCES output from the master-paced input. The quantity the
// servo measures (cumulative output samples vs master-clock elapsed) therefore
// reflects production, which the correction drives — so in steady state the
// correction settles at the value that makes production track master time
// against the DAC's crystal error. Returns the final correction after `steps`.
//
// Model: each frame, master advances FrameNanos. The producer emits
// FrameSamples·(1+corr/1e6) output samples (the resampler stretches/compresses).
// `consumed` is what the servo observes — cumulative produced output, which is
// what the backend has written. The DAC crystal error enters via the device
// delay (queued = production minus DAC consumption) when delay reporting is on;
// with delay off, the producer rate IS the consumed rate (backpressure: a full
// pipe paces Write at the DAC rate), so consumed grows at the DAC rate adjusted
// by the correction.
func driveServo(s *rateServo, dacPPM float64, steps int, delay bool) float64 {
	var master int64
	var producedAcc float64 // cumulative output samples written (servo observes this)
	var out float64
	for i := 0; i < steps; i++ {
		master += stream.FrameNanos
		// The backend writes at the DAC rate (backpressure), nudged by the servo's
		// correction applied to the resampler: effective consumed-rate per frame.
		rate := (1 + dacPPM/1e6) * (1 + out/1e6)
		producedAcc += float64(stream.FrameSamples) * rate
		out = s.observe(int64(producedAcc), master, 0, delay)
	}
	return out
}

func TestServoConvergesOnFastDAC(t *testing.T) {
	s := newRateServo(fastServoCfg())
	out := driveServo(s, 200, 2000, false)
	if math.Abs(out-(-200)) > 30 {
		t.Fatalf("expected correction ≈ -200 ppm, got %.1f", out)
	}
}

func TestServoConvergesNegative(t *testing.T) {
	s := newRateServo(fastServoCfg())
	out := driveServo(s, -150, 2000, false)
	if math.Abs(out-150) > 30 {
		t.Fatalf("expected correction ≈ +150 ppm, got %.1f", out)
	}
}

func TestServoClampsPPM(t *testing.T) {
	// 700ppm: above the ±500 test clamp but below the 800ppm outlier
	// rejection (skew beyond that is treated as a scheduler artifact, not a
	// crystal — see TestServoRejectsOutlierWindows).
	s := newRateServo(fastServoCfg())
	out := driveServo(s, 700, 2000, false)
	if out < -500.0001 || out > 500.0001 {
		t.Fatalf("output %.1f outside ±500 clamp", out)
	}
	// extreme positive skew → negative correction near clamp
	if out > -400 {
		t.Fatalf("expected correction near -500, got %.1f", out)
	}
}

func TestServoSlewLimited(t *testing.T) {
	cfg := fastServoCfg()
	cfg.SlewPPM = 3
	s := newRateServo(cfg)
	// warm up baseline
	var consumed int64
	var master int64
	consPerFrame := float64(stream.FrameSamples) * (1 + 5000/1e6)
	var consAcc float64
	prev := 0.0
	for i := 0; i < 50; i++ {
		master += stream.FrameNanos
		consAcc += consPerFrame
		consumed = int64(consAcc)
		out := s.observe(consumed, master, 0, false)
		if math.Abs(out-prev) > 3.0001 {
			t.Fatalf("step %d: |Δout|=%.3f exceeds slew 3", i, math.Abs(out-prev))
		}
		prev = out
	}
}

func TestServoZeroBeforeWarmup(t *testing.T) {
	s := newRateServo(fastServoCfg())
	// First observe sets baseline → 0.
	if out := s.observe(0, 0, 0, false); out != 0 {
		t.Fatalf("first observe should be 0, got %.3f", out)
	}
	// Before WarmUp elapsed → still 0.
	if out := s.observe(stream.FrameSamples, stream.FrameNanos, 0, false); out != 0 {
		t.Fatalf("pre-warmup observe should be 0, got %.3f", out)
	}
}

func TestServoResetClearsIntegral(t *testing.T) {
	s := newRateServo(fastServoCfg())
	driveServo(s, 200, 1000, false)
	if s.ratePPM() == 0 {
		t.Fatal("expected non-zero correction before reset")
	}
	s.reset()
	if s.ratePPM() != 0 {
		t.Fatalf("reset should zero ratePPM, got %.3f", s.ratePPM())
	}
	// After reset, first observe re-baselines to 0.
	if out := s.observe(0, 0, 0, false); out != 0 {
		t.Fatalf("post-reset first observe should be 0, got %.3f", out)
	}
}

func TestServoUsesDeviceDelay(t *testing.T) {
	// With a constant device queue, the write-only inference would over-count
	// consumed samples; subtracting the queued delay yields the true emitted
	// skew. Feed a perfectly-paced DAC (0 ppm) plus a constant 30 ms queue:
	// with delay subtracted the skew is ~0; without it the constant queue looks
	// like extra consumption.
	s := newRateServo(fastServoCfg())
	const queueNs = 30_000_000
	queueSamples := float64(queueNs) * float64(stream.SampleRate) / 1e9
	var master int64
	var out float64
	for i := 0; i < 1000; i++ {
		master += stream.FrameNanos
		// consumed = perfectly paced + a fixed queue offset
		consumed := int64(float64(stream.FrameSamples)*float64(i+1) + queueSamples)
		out = s.observe(consumed, master, queueNs, true)
	}
	if math.Abs(out) > 20 {
		t.Fatalf("with device delay subtracted, expected ≈0 ppm, got %.1f", out)
	}
}

// TestServoRejectsOutlierWindows pins the artifact guard: a window whose raw
// skew exceeds ±800ppm (late-skipped slots removing whole frames from the
// consumed count) must be measured-and-discarded — controller output
// unchanged — instead of whipsawing the rate.
func TestServoRejectsOutlierWindows(t *testing.T) {
	s := newRateServo(fastServoCfg())
	out := driveServo(s, 100, 600, false) // settle near -100
	before := out
	// One poisoned window: a burst of "late skips" — consumed freezes while
	// master time advances a full window.
	s.observe(int64(600*960), int64(600)*stream.FrameNanos+3_000_000_000, 0, false)
	if got := s.ratePPM(); got != before {
		t.Fatalf("outlier window changed output: %v -> %v", before, got)
	}
}
