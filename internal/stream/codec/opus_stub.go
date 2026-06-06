//go:build !opus

package codec

// Default build (no `opus` tag): the optional libopus-backed codec is absent
// (P5.2 §2, A.11/R7 "kept off the default/critical path"). This file registers
// NOTHING — opusFactory (codec.go) stays nil, so New(OPUS) returns
// ErrUnsupportedCodec and the capability prober reports no "opus"; negotiation
// cleanly falls back to the PCM baseline (doc 04 §4.3.2). It exists only to make
// the build-tag split explicit and to host OpusRuntimeAvailable for the default
// build so callers (caps) can reference one symbol under both tags.
//
// The name↔id registry (NameOf/FromName, codec.go) still knows "opus" in BOTH
// builds so negotiation/state can round-trip the string regardless of whether
// the codec body is linked (P4.3 §5.3, P5.2 §4.3).

// OpusRuntimeAvailable reports whether the libopus binding is usable at runtime.
// On the default build there is no binding, so it is always false. The
// `opus`-tagged opus.go provides the real dlopen-backed probe (P5.2 §4.4).
func OpusRuntimeAvailable() bool { return false }
