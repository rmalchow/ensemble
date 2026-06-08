package audio

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"

	"ensemble/internal/calibrate"
	"ensemble/internal/stream"
)

// TestInputSourceDiag exercises the EXACT recorder path (the ensemble input:
// source + pre-roll discard) and runs the estimator, to compare against a
// direct pw-record. Gated on IDIAG=1.
//
//	IDIAG=1 IDIAG_DEVICE="<dev>" go test ./internal/audio/ -run TestInputSourceDiag -v
func TestInputSourceDiag(t *testing.T) {
	if os.Getenv("IDIAG") == "" {
		t.Skip("set IDIAG=1")
	}
	device := os.Getenv("IDIAG_DEVICE")
	ref := calibrate.NewReference(calibrate.Config{})

	cap, err := OpenRawCapture(context.Background(), device)
	if err != nil {
		t.Fatalf("open raw capture: %v", err)
	}
	defer cap.Close()

	bps := stream.SampleRate * stream.Channels * 2
	// discard ~1.2 s pre-roll
	if _, err := io.CopyN(io.Discard, cap, int64(bps*6/5)); err != nil {
		t.Fatalf("preroll: %v", err)
	}
	// read ~7 s continuously
	raw := make([]byte, bps*7)
	if _, err := io.ReadFull(cap, raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	mono := make([]float32, len(raw)/4)
	for i := range mono {
		l := int16(binary.LittleEndian.Uint16(raw[i*4:]))
		r := int16(binary.LittleEndian.Uint16(raw[i*4+2:]))
		mono[i] = (float32(l) + float32(r)) * 0.5 / 32768
	}

	var sum, peak float64
	for _, v := range mono {
		sum += float64(v) * float64(v)
		if a := math.Abs(float64(v)); a > peak {
			peak = a
		}
	}
	rms := math.Sqrt(sum / float64(len(mono)))
	t.Logf("raw-capture: samples=%d rms=%.5f peak=%.5f", len(mono), rms, peak)

	est, ok := ref.EstimateDelay(mono)
	if ok {
		t.Logf("SWEEP conf=%.3f lag=%.1f loops=%d", est.Confidence, est.LagSamples, est.Loops)
	} else {
		t.Logf("SWEEP NOT DETECTED")
	}
}
