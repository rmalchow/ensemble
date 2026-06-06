package daemon

// adopt_guard.go adapts the canonical auth.AdoptionGuard (P1.2) to the adopt
// engine's Throttle + NonceStore seams, so the bootstrap adoptee surface consumes
// the SAME guard (one per process, shared throttle + nonce store) rather than
// minting its own. It translates the engine's string-src / []byte-nonce vocabulary
// to the auth guard's netip.Addr / time.Time / string-nonce API.
//
// The auth guard stores nonces keyed by their base64-url string and does not hand
// the raw bytes back; the adopt wire protocol carries raw nonce bytes. The adapter
// keeps a side map from the raw bytes it minted to the guard's string key and
// consumes by that key — both single-use + TTL'd by the auth guard, so the side
// map only bridges the encoding, never enforces policy.

import (
	"crypto/rand"
	"net/netip"
	"sync"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

type guardAdapter struct {
	g   *auth.AdoptionGuard
	now func() time.Time

	mu     sync.Mutex
	issued map[string]string // string(rawNonce) -> auth guard's base64 key
}

// newGuardAdapter wraps g (now defaults to time.Now).
func newGuardAdapter(g *auth.AdoptionGuard, now func() time.Time) *guardAdapter {
	if now == nil {
		now = time.Now
	}
	return &guardAdapter{g: g, now: now, issued: make(map[string]string)}
}

// Allow maps to auth.AdoptionGuard.Allow, classifying the refusal reason.
func (a *guardAdapter) Allow(src string) (ok bool, retryAfter time.Duration, err error) {
	addr := parseAddr(src)
	ok, reason := a.g.Allow(addr, a.now())
	if ok {
		return true, 0, nil
	}
	switch reason {
	case auth.ReasonLockedOut:
		return false, auth.NonceTTL, adopt.ErrLockedOut
	default:
		return false, auth.NonceTTL, adopt.ErrRateLimited
	}
}

func (a *guardAdapter) RecordFail(src string)    { a.g.RecordFail(parseAddr(src), a.now()) }
func (a *guardAdapter) RecordSuccess(src string) { a.g.RecordSuccess(parseAddr(src)) }

// IssueNonce mints a raw 16-byte nonce, registers a parallel string nonce with the
// auth guard, and remembers the mapping so ConsumeNonce can validate-and-burn it.
// nil on RNG failure.
func (a *guardAdapter) IssueNonce() []byte {
	now := a.now()
	key := a.g.IssueNonce(now)
	if key == "" {
		return nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil
	}
	a.mu.Lock()
	a.issued[string(raw)] = key
	a.mu.Unlock()
	return raw
}

// ConsumeNonce validates-and-burns the nonce via the auth guard's ConsumeNonce.
func (a *guardAdapter) ConsumeNonce(nonce []byte) error {
	if len(nonce) == 0 {
		return adopt.ErrNonceUnknown
	}
	a.mu.Lock()
	key, ok := a.issued[string(nonce)]
	if ok {
		delete(a.issued, string(nonce))
	}
	a.mu.Unlock()
	if !ok {
		return adopt.ErrNonceUnknown
	}
	if !a.g.ConsumeNonce(key, a.now()) {
		return adopt.ErrNonceExpired
	}
	return nil
}

// parseAddr extracts a netip.Addr from a source-IP string; an unparseable value
// yields the zero Addr (defensively bucketed together by the guard).
func parseAddr(src string) netip.Addr {
	if a, err := netip.ParseAddr(src); err == nil {
		return a
	}
	if ap, err := netip.ParseAddrPort(src); err == nil {
		return ap.Addr()
	}
	return netip.Addr{}
}

// bootstrapGuard is the combined Throttle + NonceStore seam the bootstrap surface
// consumes; the adapter satisfies it.
type bootstrapGuard interface {
	adopt.Throttle
	adopt.NonceStore
}

var _ bootstrapGuard = (*guardAdapter)(nil)
