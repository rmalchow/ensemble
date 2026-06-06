package adopt

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// nonceBytes is the single-use nonce size: 128 bits (A.9 / A.12). It matches
// auth.nonceBytes so the two guard implementations are interchangeable on the
// wire.
const nonceBytes = 16

// GuardParams are the A.12 "Adoption guard" tunables. DefaultGuardParams returns
// the verbatim values; the fields are exported so tests can shrink the windows
// with an injected clock.
type GuardParams struct {
	SoftBackoffAfter int           // consecutive fails -> soft backoff (A.12: 3)
	HardFailCount    int           // fails within HardFailWindow -> hard lockout (A.12: 10)
	HardFailWindow   time.Duration // sliding window for the hard threshold (A.12: 5 min)
	HardLockout      time.Duration // hard lockout length (A.12: 15 min)
	NonceTTL         time.Duration // single-use nonce lifetime (A.12: 30 s)
}

// DefaultGuardParams returns the A.12 "Adoption guard" row verbatim: soft backoff
// after 3 consecutive fails, hard 15-min lockout after 10 fails per 5 min,
// single-use nonces with a 30 s TTL.
func DefaultGuardParams() GuardParams {
	return GuardParams{
		SoftBackoffAfter: 3,
		HardFailCount:    10,
		HardFailWindow:   5 * time.Minute,
		HardLockout:      15 * time.Minute,
		NonceTTL:         30 * time.Second,
	}
}

// Throttle is the rate-limit surface the bootstrap node half consumes. Both this
// package's Guard and internal/auth.AdoptionGuard (via a cmd-side adapter)
// satisfy it, so the hard layering rule — bootstrap consumes the auth guard, the
// engine stays import-free — holds without adopt importing auth.
type Throttle interface {
	// Allow reports whether a fresh PIN-bearing attempt from src may proceed; when
	// !ok, retryAfter is the remaining backoff/lockout and err is ErrRateLimited or
	// ErrLockedOut.
	Allow(src string) (ok bool, retryAfter time.Duration, err error)
	// RecordFail logs a failed PIN proof for src (and globally).
	RecordFail(src string)
	// RecordSuccess clears the per-source counters on a completed adoption.
	RecordSuccess(src string)
}

// NonceStore is the single-use nonce surface the node half consumes. IssueNonce
// mints+stores; ConsumeNonce validates-and-removes (ErrNonceExpired /
// ErrNonceUnknown). Both Guard and the auth-guard adapter satisfy it.
type NonceStore interface {
	IssueNonce() []byte
	ConsumeNonce(nonce []byte) error
}

// srcState tracks one source IP's recent failures (mirrors auth.sourceState).
type srcState struct {
	consecutive int
	failTimes   []time.Time
	lockedUntil time.Time
}

// Guard enforces the A.12 adoption guard on /bootstrap/adopt: per-source AND
// global counters (03 §3.4), soft backoff, hard lockout, and single-use TTL'd
// nonces. In-memory and safe for concurrent use. It is the standalone reference
// implementation used by the adopt unit tests; production bootstrap wiring uses
// internal/auth.AdoptionGuard (the canonical guard, P1.2) through the Throttle /
// NonceStore interfaces above.
type Guard struct {
	p   GuardParams
	now func() time.Time

	mu      sync.Mutex
	sources map[string]*srcState
	global  srcState
	nonces  map[string]time.Time // base64 nonce -> issuedAt
}

// NewGuard builds a Guard with the given params and clock. A nil now defaults to
// time.Now (injected in tests so the windows are deterministic).
func NewGuard(p GuardParams, now func() time.Time) *Guard {
	if now == nil {
		now = time.Now
	}
	return &Guard{
		p:       p,
		now:     now,
		sources: make(map[string]*srcState),
		nonces:  make(map[string]time.Time),
	}
}

// Allow reports whether a fresh adopt attempt from src may proceed, and the
// remaining backoff/lockout when not. Hard lockout (per-source or global) wins
// over soft backoff. Called BEFORE running a PIN-bearing phase.
func (g *Guard) Allow(src string) (ok bool, retryAfter time.Duration, err error) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(now)

	// Hard lockout: the stronger refusal, checked first (global then per-source).
	if d := lockRemaining(&g.global, now); d > 0 {
		return false, d, ErrLockedOut
	}
	if s := g.sources[src]; s != nil {
		if d := lockRemaining(s, now); d > 0 {
			return false, d, ErrLockedOut
		}
	}
	// Soft backoff after too many consecutive fails (per-source or global).
	if g.global.consecutive >= g.p.SoftBackoffAfter {
		return false, g.p.HardFailWindow, ErrRateLimited
	}
	if s := g.sources[src]; s != nil && s.consecutive >= g.p.SoftBackoffAfter {
		return false, g.p.HardFailWindow, ErrRateLimited
	}
	return true, 0, nil
}

// RecordFail registers a failed PIN proof from src (drives soft backoff + the
// hard lockout window), per-source AND globally.
func (g *Guard) RecordFail(src string) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.sources[src]
	if s == nil {
		s = &srcState{}
		g.sources[src] = s
	}
	g.recordFailLocked(s, now)
	g.recordFailLocked(&g.global, now)
}

// RecordSuccess resets the consecutive counters (per-source + global) after a
// good proof, clearing soft backoff. An active hard lockout window stands.
func (g *Guard) RecordSuccess(src string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sources[src]; s != nil {
		s.consecutive = 0
	}
	g.global.consecutive = 0
}

// IssueNonce returns a single-use nonce valid for NonceTTL. nil on RNG failure.
func (g *Guard) IssueNonce() []byte {
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil
	}
	key := base64.RawURLEncoding.EncodeToString(buf)
	now := g.now()
	g.mu.Lock()
	g.pruneLocked(now)
	g.nonces[key] = now
	g.mu.Unlock()
	return buf
}

// ConsumeNonce validates and burns a nonce: ErrNonceUnknown if unknown or already
// used, ErrNonceExpired if past its TTL. A consumed nonce is removed regardless of
// freshness so a replay can never succeed.
func (g *Guard) ConsumeNonce(nonce []byte) error {
	if len(nonce) == 0 {
		return ErrNonceUnknown
	}
	key := base64.RawURLEncoding.EncodeToString(nonce)
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	issued, ok := g.nonces[key]
	if !ok {
		return ErrNonceUnknown
	}
	delete(g.nonces, key) // single-use: burn regardless of freshness
	if now.Sub(issued) > g.p.NonceTTL {
		return ErrNonceExpired
	}
	return nil
}

// recordFailLocked appends a failure at now, bumps the consecutive counter, and
// (re)evaluates the hard lockout over the sliding window. Caller holds g.mu.
func (g *Guard) recordFailLocked(s *srcState, now time.Time) {
	s.consecutive++
	s.failTimes = pruneOld(append(s.failTimes, now), now, g.p.HardFailWindow)
	if len(s.failTimes) >= g.p.HardFailCount {
		s.lockedUntil = now.Add(g.p.HardLockout)
	}
}

// lockRemaining returns the remaining hard lockout for s at now (0 if not locked).
func lockRemaining(s *srcState, now time.Time) time.Duration {
	if s.lockedUntil.IsZero() || !now.Before(s.lockedUntil) {
		return 0
	}
	return s.lockedUntil.Sub(now)
}

// pruneOld drops failure timestamps older than window relative to now.
func pruneOld(times []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	keep := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	return keep
}

// pruneLocked GCs expired nonces, stale failure windows, and idle per-source
// entries so memory stays bounded under a scanning attacker. Caller holds g.mu.
func (g *Guard) pruneLocked(now time.Time) {
	for n, issued := range g.nonces {
		if now.Sub(issued) > g.p.NonceTTL {
			delete(g.nonces, n)
		}
	}
	g.global.failTimes = pruneOld(g.global.failTimes, now, g.p.HardFailWindow)
	for addr, s := range g.sources {
		s.failTimes = pruneOld(s.failTimes, now, g.p.HardFailWindow)
		if len(s.failTimes) == 0 && lockRemaining(s, now) == 0 && s.consecutive == 0 {
			delete(g.sources, addr)
		}
	}
}

// Compile-time guarantee that Guard satisfies the seams the node half consumes.
var (
	_ Throttle   = (*Guard)(nil)
	_ NonceStore = (*Guard)(nil)
)
