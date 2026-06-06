//go:build opus

package codec

// opusCodec is the libopus-backed wire codec (CodecID = OPUS = 1), present only
// under the `opus` build tag (P5.2 §4.1). Each instance owns ONE libopus encoder
// and ONE decoder handle, both carrying inter-frame state, so it is NOT safe for
// concurrent Encode/Decode on the same instance (P5.2 §9 Q6). This matches the
// pipeline: the origin encodes once per chunk on a single goroutine (doc 05
// §5.2.3) and the receiver decodes on a single goroutine — neither shares a
// codec across goroutines. The codec is constructed once per streamGen and
// destroyed when the generation ends (no per-chunk C-handle allocation,
// §5.5).

type opusCodec struct {
	enc      uintptr // *OpusEncoder
	dec      uintptr // *OpusDecoder
	channels int     // interleaved channels (canonical 2)
	frame    int     // samples/channel per chunk (canonical 480 = 10 ms)

	// Reusable scratch so the hot path is allocation-free in steady state
	// (encScratch sized for the worst-case Opus frame; both Encode and Decode
	// return freshly-sized result slices only because callers may retain them).
	encScratch []byte    // opus_encode_float output staging
	decScratch []float32 // opus_decode_float output staging (frame*channels)
}

// maxOpusFrameBytes bounds a single 10 ms Opus frame. Opus frames are tens to
// ~160 B at typical music bitrates (doc 05 §5.11); 1500 B (≈ one MTU) is a safe
// ceiling that never truncates and stays well under the wire MTU budget.
const maxOpusFrameBytes = 1500

// NewOpus constructs the Opus codec for the canonical rate/channels/frame size
// (A.12) and the negotiated bitrate. It returns ErrUnsupportedCodec if libopus
// is not loadable at runtime (P5.2 §4.1) so callers treat "no Opus" uniformly
// with the default build. The encoder is configured for OPUS_APPLICATION_AUDIO
// with VBR and the given bitrate (doc 05 §5.4.2/§5.11).
func NewOpus(rate, channels, frameSamples, bitrate int) (Codec, error) {
	if !opusAvailableBinding() {
		return nil, ErrUnsupportedCodec
	}
	enc, cerr := opusEncoderCreate(rate, channels, opusApplicationAudio)
	if enc == 0 || cerr != opusOK {
		opusEncoderDestroy(enc)
		return nil, ErrUnsupportedCodec
	}
	dec, cerr := opusDecoderCreate(rate, channels)
	if dec == 0 || cerr != opusOK {
		opusEncoderDestroy(enc)
		opusDecoderDestroy(dec)
		return nil, ErrUnsupportedCodec
	}
	if bitrate > 0 {
		opusEncoderCtl1(enc, opusSetBitrateRequest, int32(bitrate))
	}
	opusEncoderCtl1(enc, opusSetVBRRequest, 1) // VBR on (music, doc 05 §5.11)

	return &opusCodec{
		enc:        enc,
		dec:        dec,
		channels:   channels,
		frame:      frameSamples,
		encScratch: make([]byte, maxOpusFrameBytes),
		decScratch: make([]float32, frameSamples*channels),
	}, nil
}

// ID reports OPUS (1).
func (c *opusCodec) ID() CodecID { return OPUS }

// Encode turns one 10 ms chunk (frame*channels interleaved float32) into one
// Opus frame. len(pcm) MUST equal frame*channels (the chunker feeds exactly
// FramesPerChunk, doc 05 §5.1); otherwise ErrChunkAlloc. The returned slice is a
// fresh copy of the staging buffer so the caller may retain it past the next
// Encode. The keyframe FLAG is set by the wire/origin layer (P4.8 §5.4), not
// here; Encode only resets prediction when the origin called ResetEncoder first.
func (c *opusCodec) Encode(pcm []float32) ([]byte, error) {
	if len(pcm) != c.frame*c.channels {
		return nil, ErrChunkAlloc
	}
	n := opusEncodeFloat(c.enc, pcm, c.frame, c.encScratch)
	if n < 0 {
		return nil, ErrChunkAlloc
	}
	out := make([]byte, n)
	copy(out, c.encScratch[:n])
	return out, nil
}

// Decode turns one Opus frame into frame*channels interleaved float32. A bad or
// short Opus frame (negative libopus return, or a sample count != frame) yields
// ErrShortPayload. The returned slice is a fresh copy so the caller may retain
// it past the next Decode.
func (c *opusCodec) Decode(payload []byte) ([]float32, error) {
	if len(payload) == 0 {
		return nil, ErrShortPayload
	}
	got := opusDecodeFloat(c.dec, payload, c.decScratch, c.frame)
	if got != c.frame {
		return nil, ErrShortPayload
	}
	out := make([]float32, c.frame*c.channels)
	copy(out, c.decScratch)
	return out, nil
}

// ResetEncoder discards inter-frame prediction state so the NEXT Encode produces
// a cold-decodable frame (KeyframeEncoder, P5.2 §4.2). It maps to
// opus_encoder_ctl(OPUS_RESET_STATE) (doc 05 §5.4.2/§5.8).
func (c *opusCodec) ResetEncoder() {
	opusEncoderCtl0(c.enc, opusResetState)
}

// ConcealLoss synthesizes one chunk of PLC concealment for a single
// unrecoverable lost packet (PLCDecoder, P5.2 §4.2): opus_decode_float with a
// NULL frame advances the decoder's PLC state and returns exactly frame*channels
// interleaved float32 (doc 05 §5.6.3). A negative libopus return is reported as
// ErrShortPayload so the receiver can fall back to the silence fade.
func (c *opusCodec) ConcealLoss() ([]float32, error) {
	got := opusDecodeFloat(c.dec, nil, c.decScratch, c.frame)
	if got != c.frame {
		return nil, ErrShortPayload
	}
	out := make([]float32, c.frame*c.channels)
	copy(out, c.decScratch)
	return out, nil
}

// Close destroys the libopus encoder/decoder handles. The origin/receiver call
// it (if they hold a *opusCodec) when tearing down a generation; the codec is
// constructed once per streamGen, so there is no per-chunk handle churn (§5.5).
func (c *opusCodec) Close() error {
	opusEncoderDestroy(c.enc)
	opusDecoderDestroy(c.dec)
	c.enc, c.dec = 0, 0
	return nil
}

// OpusRuntimeAvailable reports whether libopus dlopens AND a test encoder/decoder
// pair constructs (P5.2 §4.4). The caps prober consumes this; it is cached via
// the binding's sync.Once plus a one-shot construction probe.
func OpusRuntimeAvailable() bool {
	if !opusAvailableBinding() {
		return false
	}
	c, err := NewOpus(canonicalRate, canonicalChannels, canonicalFrameSamples, defaultOpusBitrate)
	if err != nil {
		return false
	}
	if cl, ok := c.(*opusCodec); ok {
		_ = cl.Close()
	}
	return true
}

// Compile-time assertions: opusCodec satisfies the spine Codec plus BOTH
// extension interfaces (P5.2 §4.2).
var (
	_ Codec           = (*opusCodec)(nil)
	_ KeyframeEncoder = (*opusCodec)(nil)
	_ PLCDecoder      = (*opusCodec)(nil)
)
