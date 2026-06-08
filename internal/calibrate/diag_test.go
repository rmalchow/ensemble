package calibrate

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestDiagCapture is an on-hardware diagnostic (NOT a unit test): it records
// from a capture device and runs the real estimator against it, reporting level
// and sweep detection. Gated on DIAG=1 so normal `go test` skips it.
//
//	DIAG=1 DIAG_DEVICE="<pw target>" DIAG_SECS=6 [DIAG_PLAY=1] \
//	  go test ./internal/calibrate/ -run TestDiagCapture -v
//
// DIAG_PLAY=1 also plays the sweep through the default OUTPUT (pw-play) for a
// self-contained acoustic loopback on this machine.
func TestDiagCapture(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1 to run the capture diagnostic")
	}
	dev := os.Getenv("DIAG_DEVICE")
	secs := atoiDefault(os.Getenv("DIAG_SECS"), 6)
	ref := NewReference(Config{})

	if os.Getenv("DIAG_PLAY") == "1" {
		wav := writeSweepWav(t, ref, secs+3)
		pctx, pcancel := context.WithTimeout(context.Background(), time.Duration(secs+3)*time.Second)
		defer pcancel()
		play := exec.CommandContext(pctx, "pw-play", wav)
		if err := play.Start(); err != nil {
			t.Fatalf("pw-play: %v", err)
		}
		defer play.Wait()
		time.Sleep(400 * time.Millisecond) // let playback start
	}

	// record to a raw s16le stream on stdout for `secs`.
	args := []string{"--rate", "48000", "--channels", "2", "--format", "s16"}
	if dev != "" {
		args = append(args, "--target", dev)
	}
	args = append(args, "-")
	rctx, rcancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer rcancel()
	rec := exec.CommandContext(rctx, "pw-record", args...)
	out, err := rec.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Start(); err != nil {
		t.Fatalf("pw-record start: %v", err)
	}
	buf := make([]byte, 0, 48000*2*2*secs)
	tmp := make([]byte, 64*1024)
	for {
		n, e := out.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil {
			break
		}
	}
	rec.Wait()

	mono := s16leToMono(buf)
	if skip := atoiDefault(os.Getenv("DIAG_SKIP_MS"), 0); skip > 0 {
		n := skip * 48 // samples
		if n < len(mono) {
			mono = mono[n:]
		}
		t.Logf("discarded first %d ms of capture (pre-roll)", skip)
	}
	rms, peak := levels(mono)
	t.Logf("device=%q secs=%d samples=%d (%.2fs) rms=%.5f peak=%.5f",
		dev, secs, len(mono), float64(len(mono))/48000, rms, peak)
	if len(mono) == 0 {
		t.Fatalf("NO AUDIO CAPTURED from device %q", dev)
	}

	// DC offset + how much of the signal is clipped at the rails.
	var dc float64
	clipped := 0
	for _, v := range mono {
		dc += float64(v)
		if math.Abs(float64(v)) > 0.98 {
			clipped++
		}
	}
	dc /= float64(len(mono))
	t.Logf("dc-offset=%.5f clipped=%.2f%%", dc, 100*float64(clipped)/float64(len(mono)))

	// per-second RMS (spot a startup transient vs a steady loud signal).
	var secsRMS []string
	for s := 0; s*48000 < len(mono); s++ {
		end := (s + 1) * 48000
		if end > len(mono) {
			end = len(mono)
		}
		r, _ := levels(mono[s*48000 : end])
		secsRMS = append(secsRMS, strconv.FormatFloat(r, 'f', 4, 64))
	}
	t.Logf("per-second rms: %v", secsRMS)

	// crude band energy via Goertzel at hum + sweep frequencies.
	t.Logf("band energy: 50Hz=%.4f 60Hz=%.4f 120Hz=%.4f 1kHz=%.4f 4kHz=%.4f",
		goertzel(mono, 50), goertzel(mono, 60), goertzel(mono, 120),
		goertzel(mono, 1000), goertzel(mono, 4000))

	if out := os.Getenv("DIAG_OUT"); out != "" {
		writeMonoWav(t, out, mono)
		t.Logf("wrote %s", out)
	}

	est, ok := ref.EstimateDelay(mono)
	if ok {
		t.Logf("SWEEP DETECTED: lag=%.1f samples (%.2f ms) confidence=%.3f loops=%d",
			est.LagSamples, est.LagSamples/48.0, est.Confidence, est.Loops)
	} else {
		t.Logf("SWEEP NOT DETECTED (no usable correlation peak)")
	}
}

func atoiDefault(s string, d int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return d
}

func levels(mono []float32) (rms, peak float64) {
	var sum float64
	for _, v := range mono {
		f := float64(v)
		sum += f * f
		if a := math.Abs(f); a > peak {
			peak = a
		}
	}
	if len(mono) > 0 {
		rms = math.Sqrt(sum / float64(len(mono)))
	}
	return
}

// goertzel returns the normalized magnitude of frequency f in mono.
func goertzel(mono []float32, f float64) float64 {
	if len(mono) == 0 {
		return 0
	}
	w := 2 * math.Pi * f / 48000
	cw := 2 * math.Cos(w)
	var s0, s1, s2 float64
	for _, v := range mono {
		s0 = float64(v) + cw*s1 - s2
		s2 = s1
		s1 = s0
	}
	mag := math.Sqrt(s1*s1 + s2*s2 - cw*s1*s2)
	return mag / float64(len(mono))
}

func writeMonoWav(t *testing.T, path string, mono []float32) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dataBytes := len(mono) * 2
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+dataBytes))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], 48000)
	binary.LittleEndian.PutUint32(hdr[28:], 48000*2)
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(dataBytes))
	f.Write(hdr)
	b := make([]byte, 2)
	for _, s := range mono {
		binary.LittleEndian.PutUint16(b, uint16(int16(s*32767)))
		f.Write(b)
	}
}

func s16leToMono(b []byte) []float32 {
	n := len(b) / 4 // stereo frames
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		l := int16(binary.LittleEndian.Uint16(b[i*4:]))
		r := int16(binary.LittleEndian.Uint16(b[i*4+2:]))
		out[i] = (float32(l) + float32(r)) * 0.5 / 32768
	}
	return out
}

// writeSweepWav writes `secs` of the looping reference as a 48k stereo s16le WAV.
func writeSweepWav(t *testing.T, ref *Reference, secs int) string {
	loop := ref.Loop()
	total := 48000 * secs
	path := filepath.Join(t.TempDir(), "sweep.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dataBytes := total * 2 * 2 // stereo s16
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+dataBytes))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1) // PCM
	binary.LittleEndian.PutUint16(hdr[22:], 2) // channels
	binary.LittleEndian.PutUint32(hdr[24:], 48000)
	binary.LittleEndian.PutUint32(hdr[28:], 48000*2*2)
	binary.LittleEndian.PutUint16(hdr[32:], 4)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(dataBytes))
	f.Write(hdr)

	frame := make([]byte, 4)
	for i := 0; i < total; i++ {
		s := loop[i%len(loop)]
		v := int16(s * 32767)
		binary.LittleEndian.PutUint16(frame[0:], uint16(v))
		binary.LittleEndian.PutUint16(frame[2:], uint16(v))
		f.Write(frame)
	}
	return path
}
