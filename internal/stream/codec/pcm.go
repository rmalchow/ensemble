package codec

// pcmCodec is the S16LE baseline codec (doc 05 §5.4.1, A.10 "Wire PCM format
// (m5)"). PCM on the wire is fixed S16LE: Encode narrows interleaved float32 to
// little-endian int16, Decode widens it back. There is no bit-depth profile field
// and no f32-vs-int16 selection. The codec holds no inter-frame state — every
// chunk is self-contained (an implicit keyframe; the wire/origin layer sets the
// keyframe header flag, not the codec). Being immutable, one instance is safe for
// concurrent Encode/Decode.
type pcmCodec struct {
	channels int // interleaved channels per frame (always 2 this build, doc 05 §5.1)
}

// NewPCM returns the S16LE baseline codec configured for `channels` interleaved
// channels (always 2 for this build, doc 05 §5.1) so Decode can size its output
// and Encode/Decode can validate frame alignment.
func NewPCM(channels int) Codec {
	return pcmCodec{channels: channels}
}

// ID reports PCM (0).
func (c pcmCodec) ID() CodecID { return PCM }

// Encode narrows one chunk of interleaved float32 PCM to an S16LE wire payload.
// len(pcm) must be a whole number of frames (len % channels == 0), else
// ErrChunkAlloc. The output is exactly 2*len(pcm) bytes (one int16 per sample);
// at the canonical 480-frame stereo profile that is 1920 bytes (doc 05 §5.10).
// P4.3 does not enforce FramesPerChunk — the codec is chunk-size-agnostic and
// only requires frame alignment, keeping it reusable for the join/keyframe path.
func (c pcmCodec) Encode(pcm []float32) ([]byte, error) {
	if c.channels <= 0 || len(pcm)%c.channels != 0 {
		return nil, ErrChunkAlloc
	}
	dst := make([]byte, len(pcm)*2)
	f32ToS16LE(pcm, dst)
	return dst, nil
}

// Decode widens an S16LE wire payload back to interleaved float32 PCM. len(payload)
// must be even and a whole number of frames (len % (2*channels) == 0), else
// ErrShortPayload. An empty payload decodes to a zero-length slice with no error.
func (c pcmCodec) Decode(payload []byte) ([]float32, error) {
	if c.channels <= 0 || len(payload)%(2*c.channels) != 0 {
		return nil, ErrShortPayload
	}
	dst := make([]float32, len(payload)/2)
	s16LEToF32(payload, dst)
	return dst, nil
}
