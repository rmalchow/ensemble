//go:build !opus

package caps

// Default build (no `opus` tag): no Opus prober is wired, so opusProber stays
// nil and detectCodecs() reports "pcm" only for both EncodeCodecs and
// DecodeCodecs — identical to P2.6's documented MVP default (A.11 deferred,
// §5.2). This file deliberately adds NO import edge to internal/stream/codec on
// the default build, keeping caps's default dependency set unchanged.
