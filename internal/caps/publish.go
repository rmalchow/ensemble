package caps

// publish.go owns the optimistic If-Match self-write of this node's effective
// Caps into its OWN NodeRecord.Caps in the replicated ConfigDoc (07 §2.4.1 /
// §3.2 / §4.5, A.6). It reuses the mpvsync optimistic-write IDIOM
// ("Get -> edit -> Apply -> on ErrConflict refetch+retry") but edits one
// NodeRecord, not a playlist. A node edits ONLY its own record (07 §3.2).

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// maxAttempts bounds the optimistic-retry loop (07 §4.5 m6 jittered retry). This
// is a local robustness constant, NOT an A.12 tunable: on exhaustion Publish
// returns errTooManyConflicts and cmd re-drives on the next Store.Changed().
const maxAttempts = 8

// baseBackoff is the unit of the jittered backoff between conflicting Apply
// attempts. Small by design (config writes are rare and human-driven); tests
// inject a no-op sleeper so they never actually wait.
const baseBackoff = 5 * time.Millisecond

// errTooManyConflicts is the terminal error after the retry budget is exhausted.
// Not exported: cmd/group only logs it and retries on the next Changed() tick.
var errTooManyConflicts = errors.New("caps: too many version conflicts on self-write")

// Publisher computes this node's effective Caps and self-writes them into the
// node's own NodeRecord.Caps in the replicated ConfigDoc (07 §2.4.1). One per
// node, owned by cmd/group. Safe for concurrent SetMask/Publish/Effective.
type Publisher struct {
	store    *state.Store
	selfID   string
	detected Detected

	mu   sync.Mutex
	mask Mask

	// sleep is the backoff hook; nil => time.Sleep. Tests inject a no-op so the
	// retry path runs without real delays (§7).
	sleep func(time.Duration)
}

// NewPublisher binds the store, this node's id, the (already-run) Probe result,
// and the Mask. detected is captured once; Compute is re-evaluated on each
// Publish so a config reload can change the mask and re-publish (07 §2.4.1 "on a
// config reload of the masking keys").
func NewPublisher(store *state.Store, selfID string, detected Detected, mask Mask) *Publisher {
	return &Publisher{
		store:    store,
		selfID:   selfID,
		detected: detected,
		mask:     mask,
	}
}

// SetMask swaps the masking intent (config reload) so the next Publish
// re-evaluates and re-publishes the effective caps (07 §2.4.1).
func (p *Publisher) SetMask(m Mask) {
	p.mu.Lock()
	p.mask = m
	p.mu.Unlock()
}

// Effective returns Compute(detected, mask) without writing (for cmd logging /
// the UI). Snapshot-safe under concurrent SetMask.
func (p *Publisher) Effective() state.Capabilities {
	p.mu.Lock()
	m := p.mask
	p.mu.Unlock()
	return Compute(p.detected, m)
}

// Publish computes the effective caps and writes them to this node's
// NodeRecord.Caps via the optimistic If-Match self-write (07 §3.2/§4.5):
// Get -> if this node's Caps already equal the target, NO-OP (no version bump,
// no gossip churn); else edit ONLY this node's record -> Apply -> on ErrConflict
// refetch+retry (bounded, jittered). Idempotent. Returns the effective caps it
// published (or that were already current) and any terminal error after retries
// are exhausted.
//
// If this node has no NodeRecord yet (adoption pending, §4.3), Publish is a
// no-op that returns (zero, nil): caps are published once adoption seeds the
// record and Publish is re-driven on the next Store.Changed().
func (p *Publisher) Publish(ctx context.Context) (state.Capabilities, error) {
	target := p.Effective()

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return state.Capabilities{}, err
		}

		doc := p.store.Get()
		idx := indexOfNode(doc.Nodes, p.selfID)
		if idx < 0 {
			// Record not seeded yet (adoption race guard, §4.3): no-op, no error.
			return state.Capabilities{}, nil
		}
		if capsEqual(doc.Nodes[idx].Caps, target) {
			// Idempotent short-circuit: no version bump, no Changed() fired.
			return target, nil
		}

		doc.Nodes[idx].Caps = target // edit ONLY this node's record (07 §3.2)
		if _, err := p.store.Apply(doc); err == nil {
			return target, nil
		} else if errors.Is(err, state.ErrConflict) {
			p.backoff(ctx, attempt)
			continue // refetch+retry
		} else {
			return state.Capabilities{}, err
		}
	}
	return state.Capabilities{}, errTooManyConflicts
}

// backoff sleeps a jittered, attempt-scaled interval between conflicting Apply
// attempts (07 §4.5 m6). It honors ctx cancellation through the injected sleeper
// only indirectly; the loop re-checks ctx.Err() at the top of each attempt.
func (p *Publisher) backoff(_ context.Context, attempt int) {
	// Full-jitter: a random fraction of an attempt-scaled window. attempt+1 keeps
	// the first retry from being zero-width.
	window := baseBackoff * time.Duration(attempt+1)
	d := time.Duration(rand.Int64N(int64(window) + 1))
	if p.sleep != nil {
		p.sleep(d)
		return
	}
	time.Sleep(d)
}

// indexOfNode returns the index of the NodeRecord with id, or -1 if absent.
func indexOfNode(nodes []state.NodeRecord, id string) int {
	for i := range nodes {
		if nodes[i].ID == id {
			return i
		}
	}
	return -1
}

// capsEqual reports whether two effective Capabilities are equal field-by-field.
// Both operands come from Compute, which sorts+dedups every slice, so a direct
// slices.Equal after that canonicalization holds (§5.3). This drives the
// idempotent short-circuit that prevents write-storms on every Changed() tick.
func capsEqual(a, b state.Capabilities) bool {
	return a.Render == b.Render &&
		a.MaxRate == b.MaxRate &&
		slices.Equal(a.Sinks, b.Sinks) &&
		slices.Equal(a.EncodeCodecs, b.EncodeCodecs) &&
		slices.Equal(a.DecodeCodecs, b.DecodeCodecs) &&
		slices.Equal(a.FEC, b.FEC)
}
