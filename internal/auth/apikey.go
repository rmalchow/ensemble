package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// errMalformedHash marks a stored API-key hash that cannot be parsed; such a
// record can never authenticate (treated as a non-match, never a panic).
var errMalformedHash = errors.New("auth: malformed API key hash")

// APIKeyPrefix is the fixed, human-recognizable prefix of every minted key
// (08 §B.6). It is part of the high-entropy plaintext, not a secret by itself.
const APIKeyPrefix = "ek_live_"

// apiKeyBytes / apiKeyIDBytes are the entropy of the secret and of the opaque id.
const (
	apiKeyBytes   = 32 // 256-bit secret => fast salted hash + ct-compare suffices (03 §7.3)
	apiKeyIDBytes = 12 // opaque handle, not secret
	apiKeySaltLen = 16 // per-key random salt
)

// NewAPIKey mints a key: id is an opaque handle, plaintext is shown exactly once
// to the operator (08 §B.6) and never persisted. plaintext = "ek_live_" +
// base64url(32B). Returns empty strings if the system RNG fails (the caller must
// treat that as an error and not persist a key).
func NewAPIKey() (id, plaintext string) {
	idBuf := make([]byte, apiKeyIDBytes)
	secret := make([]byte, apiKeyBytes)
	if _, err := rand.Read(idBuf); err != nil {
		return "", ""
	}
	if _, err := rand.Read(secret); err != nil {
		return "", ""
	}
	id = base64.RawURLEncoding.EncodeToString(idBuf)
	plaintext = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	return id, plaintext
}

// HashAPIKey returns the stored hash for plaintext under salt, in the canonical
// "<saltHex>$<hashHex>" form persisted in state.APIKey.Hash, where hashHex =
// hex(SHA-256(salt ‖ plaintext)). API keys are high-entropy (>=128-bit), so a
// fast salted hash + constant-time compare suffices (03 §7.3); argon2id is
// reserved for the low-entropy human password. salt is the raw salt bytes;
// callers typically pass NewAPIKeySalt().
func HashAPIKey(plaintext, salt string) string {
	saltBytes := []byte(salt)
	sum := saltedSHA256(saltBytes, plaintext)
	return hex.EncodeToString(saltBytes) + "$" + hex.EncodeToString(sum)
}

// NewAPIKeySalt returns a fresh random per-key salt as a raw byte string (pass
// straight to HashAPIKey). Empty on RNG failure.
func NewAPIKeySalt() string {
	salt := make([]byte, apiKeySaltLen)
	if _, err := rand.Read(salt); err != nil {
		return ""
	}
	return string(salt)
}

// saltedSHA256 computes SHA-256(salt ‖ plaintext).
func saltedSHA256(salt []byte, plaintext string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(plaintext))
	return h.Sum(nil)
}

// VerifyAPIKey constant-time-matches a presented bearer key against every stored
// key (state.APIKey, whose Hash carries the per-key salt as "saltHex$hashHex").
// It returns the matching key id on success. To avoid leaking which key (if any)
// matched via timing, it iterates ALL keys with a constant-time compare and does
// not early-exit on the first match.
func VerifyAPIKey(plaintext string, keys []state.APIKey) (id string, ok bool) {
	for _, k := range keys {
		salt, want, perr := parseAPIKeyHash(k.Hash)
		if perr != nil {
			continue // malformed stored hash can never authenticate
		}
		got := saltedSHA256(salt, plaintext)
		if subtle.ConstantTimeCompare(got, want) == 1 {
			// Record without breaking: assigning here and continuing keeps the
			// loop's work independent of where the match landed.
			id, ok = k.ID, true
		}
	}
	return id, ok
}

// parseAPIKeyHash splits a stored "<saltHex>$<hashHex>" into raw salt bytes and
// the stored SHA-256 digest.
func parseAPIKeyHash(stored string) (salt, hash []byte, err error) {
	saltHex, hashHex, found := strings.Cut(stored, "$")
	if !found {
		return nil, nil, errMalformedHash
	}
	if salt, err = hex.DecodeString(saltHex); err != nil {
		return nil, nil, errMalformedHash
	}
	if hash, err = hex.DecodeString(hashHex); err != nil || len(hash) != sha256.Size {
		return nil, nil, errMalformedHash
	}
	return salt, hash, nil
}
