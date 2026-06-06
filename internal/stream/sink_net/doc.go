// Package sink_net is the follower-side realtime audio receiver (05 §5.6.2): the
// inverse of stream/origin. It reads datagrams from the group audio UDP socket,
// gates them by the source-IP allowlist (P2.4), unwraps the ESND wire header,
// recovers source packets through FEC, reorders/dedupes by seq, decodes each chunk,
// and pushes the PCM into the jitter ring at its sampleIndex — the hand-off to the
// doc 06 render/drift pieces. It owns streamGen-change flush/re-prime and the
// late/duplicate/missing concealment policy (05 §5.6.2/§5.6.3/§5.8).
//
// Leaf data-plane package (01 §2): imports only sibling stream/* pieces,
// internal/audio/ring (P3.1), and internal/allowlist (P2.4). It never touches the
// AudioSink, the drift loop, or channel/gain (doc 06) — the hand-off is the ring.
package sink_net
