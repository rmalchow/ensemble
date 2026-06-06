package codec

// Extension interfaces for inter-frame codecs (P5.2 §4.2). These are ALWAYS
// compiled (no build tag) so the origin (P4.8/P5.1) and the receiver's conceal
// path can type-assert the negotiated Codec regardless of whether the optional
// Opus body is present in this build. The stateless PCM codec (P4.3) implements
// NEITHER, so a type-assert against it returns ok=false and the caller takes the
// codec-agnostic PCM path (keyframe-every-chunk, silence-fade concealment). When
// the `opus`-tagged Opus codec is present it implements BOTH, giving the origin/
// receiver a uniform, codec-agnostic keyframe/concealment driver (doc 05 §5.4).

// KeyframeEncoder is implemented by codecs that carry inter-frame encoder state
// (Opus: prediction + PLC warm-up). The origin (P4.8 §5.4) type-asserts the
// negotiated Codec to this and, when satisfied, calls ResetEncoder before
// encoding the first chunk of a new streamGen, on a late-join, and on the
// periodic ~50-chunk keyframe (doc 05 §5.4.2/§5.6.4/§5.8). PCM does not
// implement it: every PCM chunk is already independently decodable (doc 05
// §5.4.1), so no reset is needed and the origin's keyframe logic stays uniform.
type KeyframeEncoder interface {
	// ResetEncoder discards inter-frame prediction state so the NEXT Encode
	// produces a frame decodable cold (no prior frames). It does not emit a
	// frame itself; the caller resets, then encodes the keyframe chunk.
	ResetEncoder()
}

// PLCDecoder is implemented by codecs with native packet-loss concealment
// (Opus). The receiver's conceal path (P4.8 conceal.go) type-asserts the
// negotiated Codec to this and, when satisfied, fills exactly one chunk at the
// missing sampleIndex via the codec's PLC (doc 05 §5.6.3) instead of the PCM
// silence-fade. PCM does not implement it (it has no inter-frame state to
// interpolate from), so the receiver uses the short-fade silence chunk.
type PLCDecoder interface {
	// ConcealLoss synthesizes ONE chunk of concealment PCM for a single
	// unrecoverable lost packet, advancing the decoder's PLC state so the next
	// real frame decodes without a discontinuity. It returns exactly
	// FramesPerChunk*channels interleaved float32 — one chunk, never shifting
	// subsequent audio (the cardinal rule, doc 05 §5.6.3).
	ConcealLoss() ([]float32, error)
}
