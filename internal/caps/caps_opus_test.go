//go:build opus

package caps

import (
	"slices"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"
)

// Opus-enabled build (`-tags opus`). Requires libopus.so.0 at runtime; skips
// otherwise so an opus-tagged build on a box without the library still passes
// (graceful absence, P5.2 §7.3). The opus-tagged init() (detect_codec_opus.go)
// has wired opusProber == codec.OpusRuntimeAvailable.

func TestOpusPresentInDetect(t *testing.T) {
	if !codec.OpusRuntimeAvailable() {
		t.Skip("libopus not loadable; opus capability path not exercised")
	}
	if !opusAvailable() {
		t.Fatal("opusAvailable() must be true on the opus build with libopus present")
	}
	enc, dec := detectCodecs()
	if !slices.Contains(enc, codecOpus) {
		t.Errorf("EncodeCodecs %v missing %q", enc, codecOpus)
	}
	if !slices.Contains(dec, codecOpus) {
		t.Errorf("DecodeCodecs %v missing %q", dec, codecOpus)
	}
}

// TestOpusDisableMask: config can disable a present Opus path (D16 / P2.6 §5) —
// Mask.DisableCodecs=["opus"] removes it from the effective Caps even when the
// prober reports it available.
func TestOpusDisableMask(t *testing.T) {
	if !codec.OpusRuntimeAvailable() {
		t.Skip("libopus not loadable")
	}
	enc, dec := detectCodecs()
	d := Detected{EncodeCodecs: canonical(enc), DecodeCodecs: canonical(dec), FEC: detectFEC()}
	got := Compute(d, Mask{DisableCodecs: []string{"opus"}})
	if slices.Contains(got.EncodeCodecs, codecOpus) || slices.Contains(got.DecodeCodecs, codecOpus) {
		t.Errorf("DisableCodecs=[opus] left opus present: enc=%v dec=%v", got.EncodeCodecs, got.DecodeCodecs)
	}
	if !slices.Contains(got.EncodeCodecs, codecPCM) {
		t.Errorf("PCM floor must survive masking: %v", got.EncodeCodecs)
	}
}
