package streamgen

// Action is the receiver's decision for an inbound packet's StreamGen vs the
// accepted generation (doc 05 §5.8).
type Action int

const (
	// Pass: hdr.StreamGen == gate.gen ⇒ normal decode path (FEC.Recover →
	// reorder/dedupe → Decode → ring). The gate is not advanced.
	Pass Action = iota
	// Adopt: hdr.StreamGen > gate.gen ⇒ a newer generation. The caller MUST flush
	// (reorder window + FEC state + the stale ring tail beyond the new sampleIndex)
	// BEFORE decoding the packet, then resume at the new keyframe. The gate's gen is
	// advanced to the inbound value by Accept.
	Adopt
	// Drop: hdr.StreamGen < gate.gen ⇒ a stale straggler reordered late from a prior
	// generation. streamGen monotonicity disambiguates it.
	Drop
)

// String renders an Action for logs/tests.
func (a Action) String() string {
	switch a {
	case Pass:
		return "pass"
	case Adopt:
		return "adopt"
	case Drop:
		return "drop"
	default:
		return "unknown"
	}
}

// Gate is the receiver-side generation gate (one per Receiver). Not
// concurrency-safe; the recv loop is single-goroutine (doc 05 §5.6.2).
type Gate struct {
	gen uint64
}

// NewGate starts a gate at gen 0. The first inbound chunk of any live generation
// (> 0) Adopts and locks the gate onto it; a fresh group whose master is still at
// gen 0 Passes from the first chunk.
func NewGate() *Gate { return &Gate{} }

// Accept compares an inbound StreamGen to the accepted generation and returns the
// action. On Adopt the gate's gen is advanced to gen (even if it jumps by more
// than 1 — a receiver may have missed an entire intermediate generation while
// orphaned); the FLUSH side effects are the caller's (Receiver.FlushAndReprime),
// so Accept stays pure and testable. Pass and Drop do not change state.
//
// Note the gen==0 / first-packet case: a gate freshly at 0 receiving gen 0 Passes
// (it is already locked onto generation 0); receiving gen>0 Adopts and locks on.
func (g *Gate) Accept(gen uint64) Action {
	switch {
	case gen > g.gen:
		g.gen = gen
		return Adopt
	case gen < g.gen:
		return Drop
	default:
		return Pass
	}
}

// Current returns the accepted generation. The FollowerTimeline streamGen gate
// (doc 04 §4.4.3) keys off this: render holds (ok=false) until a chunk of the
// current generation arrives.
func (g *Gate) Current() uint64 { return g.gen }
