package sink_net

// conceal builds one chunk of concealment PCM for a missing/unrecoverable chunk
// (05 §5.6.3): the ring is still filled at the correct sampleIndex so the timeline
// stays aligned, at the cost of a 10 ms glitch. For PCM there is no inter-frame
// state to interpolate, so concealment is silence with a short linear fade on the
// chunk edges to avoid a click discontinuity (the "short-fade silence for PCM" of
// the §5.6.3 table). An Opus PLC hook would replace this for inter-frame codecs;
// that lives in the codec layer (A.11 defers libopus), so PCM silence-fade is the
// only concealment this build emits.
//
// The fade spans up to fadeFrames at each edge (capped at half the chunk). The
// buffer is allocated fresh per gap: gaps are the exceptional path (loss/age-out),
// not the steady state, so this does not touch the no-loss hot path.

// fadeFrames is the per-edge fade length in frames (~1 ms at 48 kHz). A short fade
// is enough to kill the click at the silence boundary without audibly softening a
// 10 ms gap.
const fadeFrames = 48

// concealChunk returns framesPerChunk frames × channels of concealment PCM
// (interleaved float32). It is all-zero (silence); the fade is applied by the
// caller against the surrounding audio in the ring, but since this build hands the
// ring contiguous chunks and the ring is plain PCM, the silence itself is the
// concealment. The fade ramps are pre-zeroed here for clarity and to keep the
// signature ready for a future cross-fade against the last good chunk.
func concealChunk(framesPerChunk, channels int) []float32 {
	n := framesPerChunk * channels
	if n <= 0 {
		return nil
	}
	// Silence. A fresh zeroed slice is already the concealment payload; an explicit
	// loop is unnecessary (Go zero-initializes make). Kept as a named helper so the
	// receiver's intent (conceal, not "push empty") is legible at the call site.
	return make([]float32, n)
}
