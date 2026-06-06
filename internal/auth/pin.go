package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/netip"
	"sync"
	"time"
)

// DefaultPIN is the placeholder adoption PIN before the operator sets one (D9,
// 07 §2.3): an empty ConfigDoc.Auth.PINHash means this value is in effect. It is
// treated as a real secret by the A.9 protocol.
const DefaultPIN = "0000"

// HashPIN argon2id-hashes a PIN (setup / change-PIN populates Auth.PINHash).
// Zero cost fields fall back to the pinned defaults (03 §7.1).
func HashPIN(pin string, p Argon2id) (phc string, err error) {
	return HashPassword(pin, p)
}

// VerifyPIN constant-time-checks a candidate PIN against ConfigDoc.Auth.PINHash
// (argon2id PHC). An empty pinHash means the placeholder default DefaultPIN
// ("0000"), compared in constant time as a plain string.
func VerifyPIN(pin, pinHash string) bool {
	if pinHash == "" {
		return constantTimeEqualString(pin, DefaultPIN)
	}
	return VerifyPassword(pin, pinHash)
}

// Adoption-guard tunables (A.12 "Adoption guard" row, verbatim):
// backoff after 3 consecutive fails / 15-min lockout after 10 fails per 5 min /
// nonce TTL 30 s.
const (
	softFailThreshold = 3                // consecutive fails -> soft backoff
	hardFailThreshold = 10               // fails within hardFailWindow -> hard lockout
	hardFailWindow    = 5 * time.Minute  // sliding window for the hard threshold
	lockoutDuration   = 15 * time.Minute // hard lockout length
	NonceTTL          = 30 * time.Second // single-use nonce lifetime
	nonceBytes        = 16               // 128-bit nonce
)

// Guard refusal reasons (08 §0.4 / 03 §3.4). Returned by Allow when !ok.
const (
	ReasonRateLimited = "rate_limited"
	ReasonLockedOut   = "locked_out"
)

// sourceState tracks one source IP's recent failures.
type sourceState struct {
	consecutive int         // resets on success; drives soft backoff
	failTimes   []time.Time // within hardFailWindow; drives hard lockout
	lockedUntil time.Time   // zero => not locked
}

// AdoptionGuard enforces the A.9/A.12 §3.4 online-guess hardening on the
// bootstrap adopt surface: soft backoff after 3 consecutive fails, hard 15-min
// lockout after 10 fails per 5 min, single-use nonces with 30 s TTL. Counters
// are kept per source IP AND globally, in memory. Safe for concurrent use.
type AdoptionGuard struct {
	mu      sync.Mutex
	sources map[netip.Addr]*sourceState
	global  sourceState
	nonces  map[string]time.Time // nonce -> issuedAt (single-use, TTL'd)
}

// NewAdoptionGuard returns an empty guard.
func NewAdoptionGuard() *AdoptionGuard {
	return &AdoptionGuard{
		sources: make(map[netip.Addr]*sourceState),
		nonces:  make(map[string]time.Time),
	}
}

// Allow reports whether a /bootstrap/adopt attempt from src may proceed at now.
// reason is "" when allowed, or ReasonLockedOut / ReasonRateLimited when not.
// Hard lockout is checked before soft backoff (the stronger refusal wins). The
// /bootstrap/info path is NOT gated by Allow — only adopt is.
func (g *AdoptionGuard) Allow(src netip.Addr, now time.Time) (ok bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.pruneLocked(now)

	// Global hard lockout (any source can trip the cluster-wide ceiling).
	if locked(&g.global, now) {
		return false, ReasonLockedOut
	}
	s := g.sources[src]
	if s != nil && locked(s, now) {
		return false, ReasonLockedOut
	}

	// Soft backoff: too many consecutive fails (per-source or global).
	if g.global.consecutive >= softFailThreshold {
		return false, ReasonRateLimited
	}
	if s != nil && s.consecutive >= softFailThreshold {
		return false, ReasonRateLimited
	}
	return true, ""
}

// RecordFail registers a failed PIN proof from src (drives backoff/lockout). The
// caller is expected to log src for audit (03 §3.4).
func (g *AdoptionGuard) RecordFail(src netip.Addr, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.sources[src]
	if s == nil {
		s = &sourceState{}
		g.sources[src] = s
	}
	recordFail(s, now)
	recordFail(&g.global, now)
}

// RecordSuccess resets the per-source (and global consecutive) counters after a
// good proof, clearing soft backoff. An existing hard lockout window stands.
func (g *AdoptionGuard) RecordSuccess(src netip.Addr) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if s := g.sources[src]; s != nil {
		s.consecutive = 0
	}
	g.global.consecutive = 0
}

// IssueNonce returns a single-use nonce valid for NonceTTL (30 s). Empty on RNG
// failure.
func (g *AdoptionGuard) IssueNonce(now time.Time) (nonce string) {
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	nonce = base64.RawURLEncoding.EncodeToString(buf)

	g.mu.Lock()
	g.pruneLocked(now)
	g.nonces[nonce] = now
	g.mu.Unlock()
	return nonce
}

// ConsumeNonce validates and burns a nonce; false if unknown, expired, or
// already used. A consumed nonce is removed so a replay cannot succeed.
func (g *AdoptionGuard) ConsumeNonce(nonce string, now time.Time) bool {
	if nonce == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	issued, ok := g.nonces[nonce]
	if !ok {
		return false
	}
	delete(g.nonces, nonce) // single-use: burn regardless of freshness
	return now.Sub(issued) <= NonceTTL
}

// recordFail appends a failure at now and bumps the consecutive counter, then
// (re)evaluates the hard lockout over the sliding window.
func recordFail(s *sourceState, now time.Time) {
	s.consecutive++
	s.failTimes = pruneOld(append(s.failTimes, now), now)
	if len(s.failTimes) >= hardFailThreshold {
		s.lockedUntil = now.Add(lockoutDuration)
	}
}

// locked reports whether s is within an active hard lockout at now.
func locked(s *sourceState, now time.Time) bool {
	return !s.lockedUntil.IsZero() && now.Before(s.lockedUntil)
}

// pruneOld drops failure timestamps older than hardFailWindow relative to now.
func pruneOld(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-hardFailWindow)
	keep := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	return keep
}

// pruneLocked garbage-collects expired nonces, stale failure windows, and
// elapsed lockouts so the guard's memory stays bounded. Caller holds g.mu.
func (g *AdoptionGuard) pruneLocked(now time.Time) {
	for n, issued := range g.nonces {
		if now.Sub(issued) > NonceTTL {
			delete(g.nonces, n)
		}
	}
	g.global.failTimes = pruneOld(g.global.failTimes, now)
	for addr, s := range g.sources {
		s.failTimes = pruneOld(s.failTimes, now)
		// Drop fully idle per-source entries: no recent fails and no active
		// lockout — keeps the map small under scanning sources.
		if len(s.failTimes) == 0 && !locked(s, now) {
			delete(g.sources, addr)
		}
	}
}
