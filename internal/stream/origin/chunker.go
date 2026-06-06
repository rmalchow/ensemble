package origin

import "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/source"

// chunker slices a looping source.Reader into fixed FramesPerChunk-frame chunks of
// interleaved float32 PCM (05 §5.2.1). The source.Reader already fills a whole dst
// across the loop seam and never returns io.EOF while looping, padding the partial
// final chunk with the loop's leading frames (source/loop.go), so every chunk this
// emits is exactly FramesPerChunk frames — no short frame ever reaches the wire.
//
// The chunk buffer is reused across reads: codec.Encode copies it into a fresh
// payload, so the origin's hot path allocates nothing here in steady state (the
// per-chunk allocation is inside codec.Encode, which is the codec layer's
// contract, not this loop's).
type chunker struct {
	src      source.Reader
	channels int
	samples  int       // FramesPerChunk * channels — the float32 count per chunk
	buf      []float32 // reused chunk buffer, len == samples
}

// newChunker builds a chunker emitting framesPerChunk-frame chunks at the reader's
// channel count.
func newChunker(src source.Reader, framesPerChunk, channels int) *chunker {
	samples := framesPerChunk * channels
	return &chunker{
		src:      src,
		channels: channels,
		samples:  samples,
		buf:      make([]float32, samples),
	}
}

// next fills the internal buffer with exactly one chunk and returns it. The
// returned slice aliases the reusable buffer — the caller must consume it (encode)
// before calling next again. A non-nil error is a hard decode/IO failure or an
// empty/un-loopable source (source.Reader never returns io.EOF while looping).
func (c *chunker) next() ([]float32, error) {
	n, err := c.src.Read(c.buf)
	if err != nil {
		return nil, err
	}
	// loop.go guarantees a full fill; defensively zero-pad any short tail so the
	// chunk is always exactly FramesPerChunk frames (the wire invariant, 05 §5.2.1).
	for i := n; i < c.samples; i++ {
		c.buf[i] = 0
	}
	return c.buf, nil
}
