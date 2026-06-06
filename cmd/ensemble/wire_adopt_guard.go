package main

import (
	"crypto/rand"
	"net/netip"
	"sync"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// guardAdapter wraps the canonical auth.AdoptionGuard (P1.2) so the adopt engine
// consumes IT for throttle + nonces (hard rule: bootstrap MUST consume this guard,
// not mint its own). It satisfies adopt.Throttle + adopt.NonceStore, translating
// the engine's string-src / []byte-nonce vocabulary to the auth guard's
// netip.Addr / time.Time / string-nonce API.
//
// The auth guard stores nonces keyed by their base64-url string; it does not hand
// the raw bytes back. The adopt wire protocol carries raw nonce bytes, so the
// adapter keeps a side map from the raw bytes it minted to the auth guard's string
// key, and consumes by that key. Both are single-use and TTL'd by the auth guard
// itself, so the side map only needs to bridge the encoding, not enforce policy.
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
		return false, auth.NonceTTL, adopt.ErrLockedOut // retryAfter is advisory here
	default:
		return false, auth.NonceTTL, adopt.ErrRateLimited
	}
}

func (a *guardAdapter) RecordFail(src string)    { a.g.RecordFail(parseAddr(src), a.now()) }
func (a *guardAdapter) RecordSuccess(src string) { a.g.RecordSuccess(parseAddr(src)) }

// IssueNonce mints a raw 16-byte nonce, registers a parallel string nonce with the
// auth guard, and remembers the mapping so ConsumeNonce can validate-and-burn it
// there. nil on RNG failure.
func (a *guardAdapter) IssueNonce() []byte {
	now := a.now()
	key := a.g.IssueNonce(now) // auth guard owns TTL + single-use
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

// ConsumeNonce validates-and-burns the nonce via the auth guard's ConsumeNonce
// (single-use, TTL'd there). ErrNonceUnknown if we never minted it; ErrNonceExpired
// if the auth guard reports it stale/used.
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

// parseAddr extracts a netip.Addr from a source-IP string (the adopt engine's
// src). An unparseable value yields the zero Addr, which the guard buckets
// together (defensive: a malformed RemoteAddr should not bypass throttling).
func parseAddr(src string) netip.Addr {
	if a, err := netip.ParseAddr(src); err == nil {
		return a
	}
	if ap, err := netip.ParseAddrPort(src); err == nil {
		return ap.Addr()
	}
	return netip.Addr{}
}

// Compile-time guarantee the adapter satisfies the bootstrap guard seam.
var _ bootstrapGuard = (*guardAdapter)(nil)
