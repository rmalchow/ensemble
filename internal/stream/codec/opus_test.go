//go:build opus

package codec

import (
	"errors"
	"math"
	"testing"
)

// These tests require BOTH the `opus` build tag AND libopus.so.0 present at
// runtime (a CI variant / dev box, never the release matrix — P5.2 §7.2). If
// libopus is not loadable they skip rather than fail, so an `opus`-tagged build
// on a box without the library still passes (graceful absence, §4.1).

func requireOpus(t *testing.T) Codec {
	t.Helper()
	if !OpusRuntimeAvailable() {
		t.Skip("libopus not loadable at runtime; skipping Opus body tests")
	}
	c, err := NewOpus(canonicalRate, canonicalChannels, canonicalFrameSamples, defaultOpusBitrate)
	if err != nil {
		t.Fatalf("NewOpus: %v", err)
	}
	return c
}

const (
	opusFrame   = canonicalFrameSamples                     // 480 samples/channel
	opusChunk   = canonicalFrameSamples * canonicalChannels // 960 interleaved
	sineHz      = 1000.0
	sampleRateF = 48000.0
)

// sineChunk fills one 480-frame stereo chunk with a 1 kHz sine at amplitude 0.5,
// continuing the phase across calls via the sample offset.
func sineChunk(offsetSamples int) []float32 {
	pcm := make([]float32, opusChunk)
	for i := 0; i < opusFrame; i++ {
		t := float64(offsetSamples+i) / sampleRateF
		v := float32(0.5 * math.Sin(2*math.Pi*sineHz*t))
		pcm[2*i] = v
		pcm[2*i+1] = v
	}
	return pcm
}

// snr computes the signal-to-noise ratio (dB) of got vs want over equal-length
// slices. Higher is better; -Inf-safe via a noise floor.
func snr(want, got []float32) float64 {
	var sig, noise float64
	for i := range want {
		sig += float64(want[i]) * float64(want[i])
		d := float64(got[i]) - float64(want[i])
		noise += d * d
	}
	if noise < 1e-12 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}

func TestOpusID(t *testing.T) {
	c := requireOpus(t)
	if c.ID() != OPUS {
		t.Fatalf("ID() = %d, want %d (OPUS)", c.ID(), OPUS)
	}
}

func TestOpusAvailableAndNew(t *testing.T) {
	if !OpusRuntimeAvailable() {
		t.Skip("libopus not loadable")
	}
	if _, err := New(OPUS); err != nil {
		t.Fatalf("New(OPUS) error on opus build with libopus present: %v", err)
	}
}

func TestOpusFrameSizeGuard(t *testing.T) {
	c := requireOpus(t)
	for _, n := range []int{0, 959, 961, opusChunk + 2} {
		if _, err := c.Encode(make([]float32, n)); !errors.Is(err, ErrChunkAlloc) {
			t.Errorf("Encode(len=%d) err = %v, want ErrChunkAlloc", n, err)
		}
	}
}

func TestOpusRoundTripSNR(t *testing.T) {
	enc := requireOpus(t)
	dec := requireOpus(t)
	// Warm the encoder/decoder over a few chunks (Opus needs lookahead/warmup),
	// then measure SNR on a steady-state chunk.
	var last float64
	for k := 0; k < 6; k++ {
		in := sineChunk(k * opusFrame)
		payload, err := enc.Encode(in)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		out, err := dec.Decode(payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != opusChunk {
			t.Fatalf("Decode len = %d, want %d", len(out), opusChunk)
		}
		last = snr(in, out)
	}
	// Opus is lossy; a 1 kHz tone @128k should round-trip well above this floor.
	if last < 20 {
		t.Fatalf("steady-state SNR = %.1f dB, want >= 20 dB (Opus@128k, doc 05 §5.4.2)", last)
	}
}

// TestOpusKeyframeReset proves ResetEncoder makes the next frame cold-decodable
// on a FRESH decoder (no prior frames), matching the late-join/new-generation
// path (doc 05 §5.4.2/§5.8).
func TestOpusKeyframeReset(t *testing.T) {
	enc := requireOpus(t)
	// Encode several chunks to build inter-frame state.
	for k := 0; k < 5; k++ {
		if _, err := enc.Encode(sineChunk(k * opusFrame)); err != nil {
			t.Fatalf("warmup Encode: %v", err)
		}
	}
	ke, ok := enc.(KeyframeEncoder)
	if !ok {
		t.Fatal("opusCodec must implement KeyframeEncoder")
	}
	ke.ResetEncoder()
	in := sineChunk(5 * opusFrame)
	payload, err := enc.Encode(in)
	if err != nil {
		t.Fatalf("keyframe Encode: %v", err)
	}
	// Decode on a brand-new decoder (cold) — this is the joiner's first frame.
	cold := requireOpus(t)
	out, err := cold.Decode(payload)
	if err != nil {
		t.Fatalf("cold Decode: %v", err)
	}
	if got := snr(in, out); got < 10 {
		t.Fatalf("cold-decoded keyframe SNR = %.1f dB, want >= 10 dB", got)
	}
}

// TestOpusPLCConceal: ConcealLoss returns exactly one chunk and advances PLC
// state so a following real frame decodes without an outright failure (doc 05
// §5.6.3).
func TestOpusPLCConceal(t *testing.T) {
	enc := requireOpus(t)
	dec := requireOpus(t)
	plc, ok := dec.(PLCDecoder)
	if !ok {
		t.Fatal("opusCodec must implement PLCDecoder")
	}
	// Prime the decoder with one good frame.
	p0, _ := enc.Encode(sineChunk(0))
	if _, err := dec.Decode(p0); err != nil {
		t.Fatalf("prime Decode: %v", err)
	}
	concealed, err := plc.ConcealLoss()
	if err != nil {
		t.Fatalf("ConcealLoss: %v", err)
	}
	if len(concealed) != opusChunk {
		t.Fatalf("ConcealLoss len = %d, want %d", len(concealed), opusChunk)
	}
	// A subsequent real frame must still decode (PLC state advanced cleanly).
	p2, _ := enc.Encode(sineChunk(2 * opusFrame))
	if _, err := dec.Decode(p2); err != nil {
		t.Fatalf("post-conceal Decode: %v", err)
	}
}

func TestOpusExtensionInterfaces(t *testing.T) {
	c := requireOpus(t)
	if _, ok := c.(KeyframeEncoder); !ok {
		t.Error("opusCodec must implement KeyframeEncoder")
	}
	if _, ok := c.(PLCDecoder); !ok {
		t.Error("opusCodec must implement PLCDecoder")
	}
}

// TestOpusPerInstanceState: two instances encode independently — driving one
// does not perturb the other (each owns its own libopus handles, §4.1/§9 Q6).
func TestOpusPerInstanceState(t *testing.T) {
	a := requireOpus(t)
	b := requireOpus(t)
	// Heavily exercise a's encoder state.
	for k := 0; k < 20; k++ {
		if _, err := a.Encode(sineChunk(k * opusFrame)); err != nil {
			t.Fatalf("a.Encode: %v", err)
		}
	}
	// b, fresh, must produce the same first-frame bytes as a fresh reference.
	ref := requireOpus(t)
	in := sineChunk(0)
	bOut, _ := b.Encode(in)
	refOut, _ := ref.Encode(in)
	if string(bOut) != string(refOut) {
		t.Fatal("b's first frame differs from a fresh reference — per-instance state leaked")
	}
}

// TestOpusLifecycle: construct/destroy many times leaks no handles (best-effort:
// availability stays true, no panic/error growth).
func TestOpusLifecycle(t *testing.T) {
	if !OpusRuntimeAvailable() {
		t.Skip("libopus not loadable")
	}
	for i := 0; i < 1000; i++ {
		c, err := NewOpus(canonicalRate, canonicalChannels, canonicalFrameSamples, defaultOpusBitrate)
		if err != nil {
			t.Fatalf("iter %d NewOpus: %v", i, err)
		}
		if cl, ok := c.(*opusCodec); ok {
			_ = cl.Close()
		}
	}
	if !OpusRuntimeAvailable() {
		t.Fatal("OpusRuntimeAvailable() flipped false after 1000 construct/destroy cycles")
	}
}
