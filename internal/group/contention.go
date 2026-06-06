package group

// contention.go (P7.1) makes takeover-under-contention deterministic (03 §4 /
// §2.7). Takeover is adoption-with-supersede, PIN-gated; two failure modes are
// nailed down here:
//
//   - Two controllers race the SAME target node. TakeoverGuard.Do single-flights
//     per target nodeID: the first caller runs the A.9 handshake + CA.Sign +
//     If-Match ConfigDoc write; concurrent callers for the same id wait for it and
//     then observe its committed result instead of signing a SECOND competing
//     leaf. (Cross-node, with no shared guard, LWW on the ConfigDoc still resolves
//     it — A.6 — and the loser's fingerprint lands in the grow-only RevokedSet;
//     the guard makes the same-process race produce exactly one signature.)
//
//   - The target is revoked / epoch-mismatched mid-handshake. precheck runs
//     BEFORE the guard invokes fn, so signing is aborted before it starts if the
//     target's fingerprint is already in RevokedSet or its protocolEpoch differs
//     (03 §2.7 "abort before signing"; mixed-epoch clusters unsupported).
//
// This is the single-flight + precheck wrapper only; the actual handshake/sign/
// write closure (fn) and the revoked/epoch predicate (precheck) are supplied by
// the takeover handler in cmd/ensemble (which owns pki.CA, state.Store, and the
// A.9 transport). The guard adds NO pki/state import edge to group.

import "sync"

// TakeoverGuard serializes re-adoption of a single target node so two controllers
// racing to take over the same node produce ONE signed identity, not two
// competing ones (03 §4). It is safe for concurrent use.
type TakeoverGuard struct {
	mu       sync.Mutex
	inflight map[string]*takeoverCall
}

// takeoverCall is one in-flight (or just-completed) single-flight slot. Waiters
// block on done and then read err — the committed result of the leader's fn.
type takeoverCall struct {
	done chan struct{}
	err  error
}

// NewTakeoverGuard returns an empty guard.
func NewTakeoverGuard() *TakeoverGuard {
	return &TakeoverGuard{inflight: make(map[string]*takeoverCall)}
}

// Do single-flights fn per target nodeID (03 §4). Semantics:
//
//   - precheck runs FIRST, on every caller, before any guarding: if it returns a
//     non-nil error (target revoked or epoch-mismatched, 03 §2.7) Do returns that
//     error WITHOUT signing — fn never runs. The precheck is per-caller (not
//     shared) so a late caller that observes a fresh revocation still aborts.
//   - If precheck passes, the FIRST caller for nodeID becomes the leader and runs
//     fn (the A.9 handshake + CA.Sign + If-Match write). Concurrent callers for
//     the same nodeID block until the leader finishes, then observe the leader's
//     err — they do NOT run fn again (no second competing leaf).
//   - Different nodeIDs run concurrently: the guard serializes per target only.
//
// A nil precheck is treated as "allowed"; a nil fn is a no-op success.
func (g *TakeoverGuard) Do(nodeID string, precheck func() error, fn func() error) error {
	// Precheck is per-caller and BEFORE the guard: a revoked/epoch-mismatched
	// target aborts before signing even if another caller is mid-flight (03 §2.7).
	if precheck != nil {
		if err := precheck(); err != nil {
			return err
		}
	}

	g.mu.Lock()
	if c, ok := g.inflight[nodeID]; ok {
		// A leader is already signing this target: wait and observe its result.
		g.mu.Unlock()
		<-c.done
		return c.err
	}
	c := &takeoverCall{done: make(chan struct{})}
	g.inflight[nodeID] = c
	g.mu.Unlock()

	// We are the leader. Run fn exactly once for this target.
	if fn != nil {
		c.err = fn()
	}

	// Publish the result and free the slot so a LATER takeover of the same target
	// (a new generation) starts a fresh single-flight rather than re-observing this
	// stale result.
	g.mu.Lock()
	delete(g.inflight, nodeID)
	g.mu.Unlock()
	close(c.done)

	return c.err
}
