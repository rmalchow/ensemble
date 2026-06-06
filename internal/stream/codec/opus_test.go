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
	// sineHz is chosen so that the encoder round-trip latency (312 samples at
	// 48 kHz) is an integer multiple of the test tone's period.  1000 Hz has
	// period 48 samples and 312/48 = 6.5 — a half-integer — which causes a
	// 180° phase inversion in the decoded output and yields SNR ≈ -6 dB even
	// though the codec is working perfectly.  923 Hz has period ≈ 52 samples
	// and 312/52 ≈ 6.0 — an integer — so the decoded signal is in-phase with
	// the input and a faithful round-trip gives SNR > 30 dB (doc 05 §5.4.2).
	sineHz      = 923.0
	sampleRateF = 48000.0
)

// sineChunk fills one 480-frame stereo chunk with a sine at sineHz and
// amplitude 0.5, continuing the phase across calls via the sample offset.
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
//
// The Opus encoder has a lookahead of ~312 samples (confirmed via
// OPUS_GET_LOOKAHEAD).  For the very first decoded frame on a cold decoder,
// the first 312 per-channel samples are silence while the decoder drains its
// internal pre-skip buffer, so measuring SNR over the full first frame always
// yields ≈ 2 dB regardless of codec quality.  The test therefore models the
// real late-join scenario: the joiner receives and decodes the keyframe (to
// initialise its decoder state), then decodes ONE continuation frame whose
// full 480 samples correspond to real signal — that second decode must meet
// the SNR floor (doc 05 §5.4.2/§5.8).
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
	// Keyframe — the frame a late-joining decoder receives first.
	keyPayload, err := enc.Encode(sineChunk(5 * opusFrame))
	if err != nil {
		t.Fatalf("keyframe Encode: %v", err)
	}
	// One continuation frame immediately following the keyframe.
	contIn := sineChunk(6 * opusFrame)
	contPayload, err := enc.Encode(contIn)
	if err != nil {
		t.Fatalf("continuation Encode: %v", err)
	}

	// Decode on a brand-new decoder (cold) — this is the joiner's path.
	cold := requireOpus(t)
	if _, err := cold.Decode(keyPayload); err != nil {
		t.Fatalf("cold Decode keyframe: %v", err)
	}
	// The first decoded frame drains the encoder pre-skip (≈312 samples of
	// silence); the second frame is fully in-signal and must meet the quality
	// bar (doc 05 §5.4.2/§5.8).
	contOut, err := cold.Decode(contPayload)
	if err != nil {
		t.Fatalf("cold Decode continuation: %v", err)
	}
	if got := snr(contIn, contOut); got < 10 {
		t.Fatalf("post-keyframe SNR = %.1f dB, want >= 10 dB", got)
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
