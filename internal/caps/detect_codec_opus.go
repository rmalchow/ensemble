//go:build opus

package caps

// Opus-enabled build (`-tags opus`): wire the runtime Opus prober into the
// codec-detection seam (P5.2 §4.4). This is the ONLY place caps imports
// internal/stream/codec, and only under the `opus` tag — the default build adds
// no such edge (detect_codec_opus_stub.go). codec.OpusRuntimeAvailable caches
// its dlopen result, so detectCodecs() calling opusProber() repeatedly is cheap.

import "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/codec"

func init() { opusProber = codec.OpusRuntimeAvailable }
