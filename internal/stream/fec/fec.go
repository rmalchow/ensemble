// Package fec is the wire forward-error-correction layer (README §6.3, doc 05
// §5.5). A FEC protects source packets on the master side and recovers them on
// the follower side. It is a leaf: its only internal import is stream/wire, for
// the wire.Packet type the §6.3 Recover signature references (doc 01 §2.1 Q1
// resolution (a): the single unavoidable fec→wire leaf edge). P4.3 ships the
// None (identity) scheme; P5.1 adds XOR-parity (k=8, interleave D=4) and packet
// duplication (Ddup=5) behind the same interface.
package fec

import (
	"errors"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/stream/wire"
)

// FECID is the integer FEC identifier carried at the §6.4 wire layer (header
// offset 7).
type FECID uint8

const (
	None      FECID = 0 // identity (doc 05 §5.5); also forced over TCP fallback (§5.9)
	XORParity FECID = 1 // XOR of k payloads (doc 05 §5.5.1) — P4.5
	Duplicate FECID = 2 // each packet sent twice (doc 05 §5.5.4) — P4.5
)

// FEC is the spine interface (README §6.3), implemented verbatim.
type FEC interface {
	ID() FECID
	// Protect groups source packets and emits source+repair packets to send.
	// None returns exactly [pkt].
	Protect(seq uint64, pkt []byte) (out [][]byte)
	// Recover ingests a received packet; returns any newly-recoverable source
	// packets. None returns exactly [p].
	Recover(p wire.Packet) (recovered []wire.Packet)
}

// ErrUnsupportedFEC is returned by New for an unknown id.
var ErrUnsupportedFEC = errors.New("fec: unsupported fec id")

// New returns the FEC for id, or ErrUnsupportedFEC for an unknown one. The
// non-trivial schemes use their A.12 default tunables; callers wanting custom
// K/Interleave/Offset use NewXOR/NewDup directly. Unknown id is the caller's
// negotiation bug to surface (origin/sink_net keep the prior instance on error,
// per resetFEC).
func New(id FECID) (FEC, error) {
	switch id {
	case None:
		return NewNone(), nil
	case XORParity:
		return NewXOR(DefaultXORConfig()), nil
	case Duplicate:
		return NewDup(DefaultDupConfig()), nil
	default:
		return nil, ErrUnsupportedFEC
	}
}

// --- name↔id registry (README §6.5: "none"|"xorParity"|"duplicate" in JSON) ---
//
// A fixed slice indexed by id, listing EVERY enum name so profile negotiation
// (P4.2) and state (P2.1) round-trip string↔id for any advertised capability,
// while New gates what is actually buildable. Immutable, allocation-free, no map.

// fecNames is the canonical id->string table (README §6.5 / doc 05 §6.3), indexed
// by FECID. The reverse direction is derived from it so the two never disagree.
// Note the camelCase "xorParity".
var fecNames = [...]string{
	None:      "none",
	XORParity: "xorParity",
	Duplicate: "duplicate",
}

// NameOf maps a FECID to its canonical JSON string ("none"|"xorParity"|
// "duplicate"). Returns ("", false) for an unknown id.
func NameOf(id FECID) (string, bool) {
	if int(id) >= len(fecNames) {
		return "", false
	}
	return fecNames[id], true
}

// FromName maps a canonical string to its FECID. Returns (0, false) for an
// unknown name.
func FromName(name string) (FECID, bool) {
	for id, n := range fecNames {
		if n == name {
			return FECID(id), true
		}
	}
	return 0, false
}
