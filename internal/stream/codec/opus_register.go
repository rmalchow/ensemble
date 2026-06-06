//go:build opus

package codec

// Opus-enabled build (`-tags opus`): fill P4.3's reserved OPUS constructor slot
// by wiring the in-package opusFactory hook (P5.2 §4.3). New(OPUS) then routes
// to NewOpus; NewOpus still returns ErrUnsupportedCodec if libopus fails to
// dlopen at runtime, so even an `opus`-tagged binary on a box without libopus
// behaves like the default build. Under the default (`!opus`) build this file is
// excluded, opusFactory stays nil, and New(OPUS) returns ErrUnsupportedCodec.

func init() {
	opusFactory = func(rate, channels, frameSamples, bitrate int) (Codec, error) {
		return NewOpus(rate, channels, frameSamples, bitrate)
	}
}
