package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

// sessionIDBytes is the entropy of a session cookie value (32 bytes = 256 bits).
const sessionIDBytes = 32

// sessionEntry is the server-side record for one live session. The plaintext
// cookie value is never stored — only its SHA-256 hash keys the map.
type sessionEntry struct {
	issuedAt time.Time
	lastSeen time.Time
}

// Sessions is the per-node, in-memory session store. NOT replicated (03 §7.2):
// a cookie issued by N1 is invalid at N2. Safe for concurrent use.
type Sessions struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]sessionEntry
	now     func() time.Time // injectable clock for tests
}

// NewSessions returns an empty session store with the real wall clock.
func NewSessions() *Sessions {
	return &Sessions{
		entries: make(map[[sha256.Size]byte]sessionEntry),
		now:     time.Now,
	}
}

// hashValue returns the SHA-256 of a cookie value as a fixed-size array (usable
// as a map key and a stable lookup handle; the plaintext never enters the map).
func hashValue(v string) [sha256.Size]byte {
	return sha256.Sum256([]byte(v))
}

// Issue returns the plaintext cookie value (32 random bytes, base64url); the
// store keeps only its SHA-256 hash.
func (s *Sessions) Issue() (cookieValue string) {
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is fatal for security; surface as empty so the
		// caller's Validate can never match an empty value.
		return ""
	}
	value := base64.RawURLEncoding.EncodeToString(buf)

	now := s.now()
	s.mu.Lock()
	s.entries[hashValue(value)] = sessionEntry{issuedAt: now, lastSeen: now}
	s.mu.Unlock()
	return value
}

// expired reports whether e is dead at now (idle timeout OR absolute cap).
func expired(e sessionEntry, now time.Time) bool {
	return now.Sub(e.issuedAt) > absoluteTTL || now.Sub(e.lastSeen) > idleTTL
}

// Validate looks up the hash of cookieValue; on a live hit it slides the 12 h
// idle TTL (within the 7 d absolute cap) and returns true. Expired or absent
// values return false (and an expired entry is dropped).
func (s *Sessions) Validate(cookieValue string) bool {
	if cookieValue == "" {
		return false
	}
	key := hashValue(cookieValue)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return false
	}
	if expired(e, now) {
		delete(s.entries, key)
		return false
	}
	e.lastSeen = now // slide idle TTL; absolute cap (issuedAt) is untouched
	s.entries[key] = e
	return true
}

// Revoke drops the server-side entry (logout, 03 §7.2 / 08 §B.3). A no-op for an
// unknown value.
func (s *Sessions) Revoke(cookieValue string) {
	if cookieValue == "" {
		return
	}
	key := hashValue(cookieValue)
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// Sweep evicts expired entries to bound memory on a constrained node; call from
// a background ticker (e.g. every 1 min). O(n) over live sessions.
func (s *Sessions) Sweep() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if expired(e, now) {
			delete(s.entries, k)
		}
	}
}

// Len reports the number of stored entries (live or not-yet-swept). Intended for
// tests and metrics.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
