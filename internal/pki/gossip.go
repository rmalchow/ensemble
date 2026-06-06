package pki

import "crypto/rand"

// gossipKeyLen is the memberlist SecretKey size. memberlist accepts 16/24/32-byte
// keys (AES-128/192/256-GCM); we always use 32 bytes (AES-256-GCM, doc 03 §1.5).
// This mirrors media's keyLen=32 rationale (key.go:28) but the value is RANDOM,
// not HKDF-derived.
const gossipKeyLen = 32

// NewGossipKey returns 32 cryptographically-random bytes for memberlist's
// SecretKey (AES-256-GCM). D18: random, NOT derived, NOT sealed (doc 03 §1.6:
// "no derivation/sealing"). crypto/rand.Read never returns a short read without
// an error on the platforms in scope, so a failure is unrecoverable and panics
// rather than silently yielding a weak key.
func NewGossipKey() []byte {
	key := make([]byte, gossipKeyLen)
	if _, err := rand.Read(key); err != nil {
		panic("pki: gossip key generation failed: " + err.Error())
	}
	return key
}
