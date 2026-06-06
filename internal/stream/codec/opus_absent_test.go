//go:build !opus

package codec

import (
	"errors"
	"testing"
)

// These tests pin the load-bearing graceful-absence contract (P5.2 §7.1): on the
// default CGO_ENABLED=0 build (no `opus` tag) Opus is completely absent — the
// constructor gate is closed, the runtime probe reports false, yet the name
// layer still round-trips "opus" so negotiation/state are unaffected. They are
// tagged `!opus` so the `opus`-build (where New(OPUS) succeeds) does not run
// them; the `opus`-tagged opus_test.go covers that build.

func TestOpusNewGatingAbsent(t *testing.T) {
	if _, err := New(OPUS); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("New(OPUS) err = %v, want ErrUnsupportedCodec on the default build", err)
	}
}

func TestOpusRuntimeAvailableAbsent(t *testing.T) {
	if OpusRuntimeAvailable() {
		t.Fatal("OpusRuntimeAvailable() must be false on the default build (no opus tag)")
	}
}

// TestOpusNameLayerIntact: even with no codec body, the string↔id registry knows
// "opus" so profile negotiation and ConfigDoc state can round-trip the name
// (P4.3 §5.3, P5.2 §4.3).
func TestOpusNameLayerIntact(t *testing.T) {
	id, ok := FromName("opus")
	if !ok || id != OPUS {
		t.Fatalf("FromName(\"opus\") = %d,%v; want %d,true", id, ok, OPUS)
	}
	name, ok := NameOf(OPUS)
	if !ok || name != "opus" {
		t.Fatalf("NameOf(OPUS) = %q,%v; want \"opus\",true", name, ok)
	}
}

// TestPCMNotExtensionCodec: the PCM codec implements NEITHER extension
// interface, so the origin/receiver type-asserts return ok=false and fall back
// to the codec-agnostic PCM path (keyframe-every-chunk / silence-fade). This is
// what keeps the extension interfaces always-compiled yet PCM-safe (P5.2 §4.2).
func TestPCMNotExtensionCodec(t *testing.T) {
	c := NewPCM(canonicalChannels)
	if _, ok := c.(KeyframeEncoder); ok {
		t.Error("PCM codec must NOT implement KeyframeEncoder")
	}
	if _, ok := c.(PLCDecoder); ok {
		t.Error("PCM codec must NOT implement PLCDecoder")
	}
}
