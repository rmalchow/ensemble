package clock

import (
	"errors"
	"testing"
	"time"
)

// modelExchange builds the four timestamps for a given true offset, one-way
// delays, and master processing time, mirroring a real round trip.
func modelExchange(t1, offset, dUp, dDown, proc int64) (a, b, c, d int64) {
	t2 := t1 + offset + dUp
	t3 := t2 + proc
	t4 := t3 - offset + dDown
	return t1, t2, t3, t4
}

func TestComputeSampleSymmetric(t *testing.T) {
	const (
		offset = int64(1_000_000) // 1ms
		oneWay = int64(500_000)   // 0.5ms each direction
		proc   = int64(100_000)
	)
	t1, t2, t3, t4 := modelExchange(1_000, offset, oneWay, oneWay, proc)
	s := computeSample(t1, t2, t3, t4)
	if s.Offset != time.Duration(offset) {
		t.Errorf("offset = %v, want %v", s.Offset, time.Duration(offset))
	}
	if want := time.Duration(2 * oneWay); s.Delay != want {
		t.Errorf("delay = %v, want %v", s.Delay, want)
	}
}

func TestComputeSampleAsymmetricBiasBounded(t *testing.T) {
	// Asymmetric paths bias the offset by at most (dUp-dDown)/2 (NTP fundamental).
	const offset = int64(2_000_000)
	t1, t2, t3, t4 := modelExchange(0, offset, 900_000, 100_000, 0)
	s := computeSample(t1, t2, t3, t4)
	bias := s.Offset - time.Duration(offset)
	if bias != time.Duration((900_000-100_000)/2) {
		t.Errorf("bias = %v, want %v", bias, time.Duration(400_000))
	}
}

func TestComputeSampleNegativeDelayClamp(t *testing.T) {
	// If reported master processing time exceeds the round trip (clock skew /
	// measurement noise), delay would go negative; A.1 clamps it to zero.
	// t4 - t1 = 0, t3 - t2 = proc > 0  ⇒  raw delay < 0  ⇒  clamped to 0.
	s := computeSample(1000, 1000, 1500, 1000)
	if s.Delay != 0 {
		t.Errorf("delay = %v, want 0 (clamped, never negative)", s.Delay)
	}
}

func TestEstimatorMinDelayFilter(t *testing.T) {
	e := NewEstimator(8, 0.5)
	// True offset is 1_000_000; noisy samples have inflated delay AND skewed
	// offset. One clean low-delay sample should dominate.
	noisy := []Sample{
		{Offset: 1_500_000, Delay: 9_000_000},
		{Offset: 700_000, Delay: 8_000_000},
		{Offset: 1_000_000, Delay: 200_000}, // clean
		{Offset: 1_800_000, Delay: 7_000_000},
	}
	for _, s := range noisy {
		e.Add(s)
	}
	off, ok := e.Offset()
	if !ok {
		t.Fatal("expected an estimate")
	}
	// Should be pulled toward the clean 1ms sample, not the noisy ones.
	if off < 900*time.Microsecond || off > 1200*time.Microsecond {
		t.Errorf("offset = %v, want ~1ms (min-delay filter should dominate)", off)
	}
	if md, _ := e.MinDelay(); md != 200*time.Microsecond {
		t.Errorf("min delay = %v, want 200µs", md)
	}
}

func TestEstimatorEWMASlew(t *testing.T) {
	// After the first sample have=true and offset==first; subsequent updates
	// slew toward the new best by alpha·Δ rather than stepping (A.1).
	const alpha = 0.15
	e := NewEstimator(1, alpha) // window 1 ⇒ best is always the latest sample
	first := e.Add(Sample{Offset: 1_000_000, Delay: 100})
	if first != time.Duration(1_000_000) {
		t.Fatalf("first offset = %v, want exactly the first sample", first)
	}
	if _, ok := e.Offset(); !ok {
		t.Fatal("have should be true after first sample")
	}
	// Jump the true offset to 2ms; the applied value must move by ~alpha·Δ only.
	got := e.Add(Sample{Offset: 2_000_000, Delay: 100})
	want := time.Duration(1_000_000 + int64(alpha*float64(2_000_000-1_000_000)))
	if got != want {
		t.Errorf("slewed offset = %v, want %v (alpha·Δ step)", got, want)
	}
	// And it must NOT have stepped all the way to the new sample.
	if got >= 2_000_000 {
		t.Errorf("offset stepped to %v; EWMA must slew, never step", got)
	}
}

func TestEstimatorWindowEviction(t *testing.T) {
	const window = 4
	e := NewEstimator(window, 0.5)
	// Feed window+k samples. The early ones carry a low delay that must be
	// evicted once they fall out of the sliding window.
	for i := 0; i < window; i++ {
		e.Add(Sample{Offset: 1_000_000, Delay: 50}) // clean low delay
	}
	if md, _ := e.MinDelay(); md != 50 {
		t.Fatalf("min delay = %v, want 50 while clean samples are in window", md)
	}
	// Push window more high-delay samples; the clean ones are fully evicted.
	for i := 0; i < window; i++ {
		e.Add(Sample{Offset: 1_000_000, Delay: 9_000})
	}
	if md, _ := e.MinDelay(); md != 9_000 {
		t.Errorf("min delay = %v, want 9000 (only last %d samples remain)", md, window)
	}
	if e.Samples() != 2*window {
		t.Errorf("samples = %d, want %d", e.Samples(), 2*window)
	}
}

func TestEstimatorA12WindowAlpha(t *testing.T) {
	tests := []struct {
		name   string
		window int
		alpha  float64
	}{
		{"wired A.12", 8, 0.15},
		{"wifi A.12", 16, 0.10},
		{"bad window falls back", 0, 0.15},
		{"bad alpha low falls back", 8, 0},
		{"bad alpha high falls back", 8, 1.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEstimator(tc.window, tc.alpha)
			// Feed a steady offset; the estimate must converge to it regardless
			// of the (possibly defaulted) parameters.
			const trueOffset = time.Duration(3_000_000)
			for i := 0; i < 200; i++ {
				e.Add(Sample{Offset: trueOffset, Delay: 1000})
			}
			off, ok := e.Offset()
			if !ok {
				t.Fatal("expected an estimate")
			}
			if d := off - trueOffset; d < -time.Microsecond || d > time.Microsecond {
				t.Errorf("converged offset = %v, want ~%v", off, trueOffset)
			}
		})
	}
}

func TestPacketRoundTrip(t *testing.T) {
	if PacketSize != 40 {
		t.Fatalf("PacketSize = %d, want 40 (README §6.4 frozen wire format)", PacketSize)
	}
	in := packet{kind: kindReply, seq: 0xDEADBEEF, t1: 111, t2: 222, t3: 333}
	out, err := unmarshal(in.marshal())
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestUnmarshalRejects(t *testing.T) {
	good := packet{kind: kindRequest, seq: 1, t1: 1}.marshal()

	short := good[:PacketSize-1]
	if _, err := unmarshal(short); !errors.Is(err, errBadPacket) {
		t.Errorf("short buf err = %v, want errBadPacket", err)
	}

	badMagic := append([]byte(nil), good...)
	badMagic[0] ^= 0xFF
	if _, err := unmarshal(badMagic); !errors.Is(err, errBadPacket) {
		t.Errorf("wrong-magic err = %v, want errBadPacket", err)
	}

	badVersion := append([]byte(nil), good...)
	badVersion[4] = version + 1
	if _, err := unmarshal(badVersion); !errors.Is(err, errBadPacket) {
		t.Errorf("wrong-version err = %v, want errBadPacket", err)
	}
}
