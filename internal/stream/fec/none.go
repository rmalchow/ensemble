package fec

import "gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"

// noneFEC is the identity scheme (doc 05 §5.5): Protect returns [pkt], Recover
// returns [p]. It is also the forced scheme over the TCP fallback (§5.9) — no
// separate code. It holds no state, spawns no goroutines, and allocates only the
// single-element slice header each call.
type noneFEC struct{}

// NewNone returns the identity FEC.
func NewNone() FEC { return noneFEC{} }

// ID reports None (0).
func (noneFEC) ID() FECID { return None }

// Protect returns exactly [pkt] (doc 05 §5.5). It does not copy pkt (zero-cost
// passthrough) and does not retain it — ownership stays with the caller. seq is
// unused by the identity scheme.
func (noneFEC) Protect(seq uint64, pkt []byte) (out [][]byte) {
	return [][]byte{pkt}
}

// Recover returns exactly [p] — the source packet as-is (doc 05 §5.5). None
// touches no header fields; it passes the value through.
func (noneFEC) Recover(p wire.Packet) (recovered []wire.Packet) {
	return []wire.Packet{p}
}
