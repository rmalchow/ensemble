package fec

// Tunables for the two non-trivial FEC schemes. The defaults are pinned VERBATIM
// by Appendix A.12 (the single source of truth for these numbers); they are
// carried in the negotiated group profile (04/07) and may be re-tuned there in
// the future, but the constructors default to the A.12 values.

// XORConfig parameterizes the XOR-parity scheme (doc 05 §5.5.3).
type XORConfig struct {
	K          int // source packets per parity group (A.12: 8)
	Interleave int // D: number of concurrent interleaved parity groups (A.12: 4)
}

// DupConfig parameterizes the packet-duplication scheme (doc 05 §5.5.4).
type DupConfig struct {
	Offset int // Ddup: packets between a source packet and its duplicate (A.12: 5)
}

// A.12 pinned tunables. Named so the magic numbers appear exactly once.
const (
	defaultK          = 8 // FEC XOR k (A.12: "8 / 4 (~40 ms burst)")
	defaultInterleave = 4 // FEC XOR interleave depth D (A.12)
	defaultDupOffset  = 5 // FEC dup offset Ddup (A.12: "5 packets (~50 ms)")
)

// DefaultXORConfig returns the A.12 XOR tunables (K=8, Interleave=4).
func DefaultXORConfig() XORConfig {
	return XORConfig{K: defaultK, Interleave: defaultInterleave}
}

// DefaultDupConfig returns the A.12 duplication tunable (Offset=5).
func DefaultDupConfig() DupConfig {
	return DupConfig{Offset: defaultDupOffset}
}

// normalize clamps a config to sane positive values, falling back to the A.12
// default for any non-positive field so a misconfigured profile can never panic
// (e.g. divide-by-zero on the interleave modulus) and degrades to the default.
func (c XORConfig) normalize() XORConfig {
	if c.K <= 0 {
		c.K = defaultK
	}
	if c.Interleave <= 0 {
		c.Interleave = defaultInterleave
	}
	return c
}

func (c DupConfig) normalize() DupConfig {
	if c.Offset <= 0 {
		c.Offset = defaultDupOffset
	}
	return c
}
