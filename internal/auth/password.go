package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// Argon2id is the argon2id cost-parameter struct carried in ConfigDoc.Auth.Argon
// (07 §2.3). auth does not own this type; it aliases the canonical state type so
// callers use one set of names cluster-wide.
type Argon2id = state.Argon2id

// Default argon2id cost (03 §7.1, A.11/A.12). m=64 MiB, t=3, p=4 (doc 03/07);
// keyLen=32, saltLen=16. These are the GENERATION params for the next write —
// every verify reads the params embedded in the PHC string instead.
const (
	defaultMemKiB  uint32 = 64 * 1024 // 65536 KiB = 64 MiB
	defaultTime    uint32 = 3
	defaultThreads uint8  = 4
	defaultKeyLen  uint32 = 32
	defaultSaltLen uint32 = 16
)

// DefaultArgon2id returns the pinned default cost parameters (03 §7.1, A.12).
// Used to seed ConfigDoc.Auth.Argon at setup.
func DefaultArgon2id() Argon2id {
	return Argon2id{
		MemKiB:  defaultMemKiB,
		Time:    defaultTime,
		Threads: defaultThreads,
		KeyLen:  defaultKeyLen,
		SaltLen: defaultSaltLen,
	}
}

// argon2Version is the only argon2 version we emit/accept ($v=19, RFC 9106).
const argon2Version = argon2.Version // 19

// b64 is the standard PHC encoding: base64 (std alphabet) without padding.
var b64 = base64.RawStdEncoding

// errMalformedPHC is returned internally when a stored PHC string cannot be
// parsed; verifiers translate it to a plain false (never panic).
var errMalformedPHC = errors.New("auth: malformed argon2id PHC string")

// withDefaults fills any zero cost field from the pinned defaults so a partially
// populated (or zero) Argon2id still produces a valid, safe hash.
func withDefaults(p Argon2id) Argon2id {
	if p.MemKiB == 0 {
		p.MemKiB = defaultMemKiB
	}
	if p.Time == 0 {
		p.Time = defaultTime
	}
	if p.Threads == 0 {
		p.Threads = defaultThreads
	}
	if p.KeyLen == 0 {
		p.KeyLen = defaultKeyLen
	}
	if p.SaltLen == 0 {
		p.SaltLen = defaultSaltLen
	}
	return p
}

// HashPassword hashes pw with argon2id using the given cost params and a fresh
// random salt, returning a PHC-encoded string
// ($argon2id$v=19$m=...,t=...,p=...$salt$hash). Cost fields left zero fall back
// to the pinned defaults (03 §7.1).
func HashPassword(pw string, p Argon2id) (phc string, err error) {
	p = withDefaults(p)
	salt := make([]byte, p.SaltLen)
	if _, err = rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	return phcEncode(pw, salt, p), nil
}

// phcEncode derives the argon2id key over (pw, salt, params) and renders the PHC
// string. Shared by HashPassword and HashPIN.
func phcEncode(secret string, salt []byte, p Argon2id) string {
	key := argon2.IDKey([]byte(secret), salt, p.Time, p.MemKiB, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, p.MemKiB, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// VerifyPassword constant-time-compares pw against a PHC string produced by
// HashPassword. Cost params are read FROM the PHC string (self-describing), so a
// hash made under old params still verifies after the cluster raises cost. A
// malformed PHC returns false (never panics).
func VerifyPassword(pw, phc string) bool {
	salt, want, p, err := phcDecode(phc)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.MemKiB, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// phcDecode parses a $argon2id$v=19$m=,t=,p=$salt$hash string into its salt, the
// stored derived key, and the cost params embedded in it.
func phcDecode(phc string) (salt, hash []byte, p Argon2id, err error) {
	// Fields: ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"].
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, p, errMalformedPHC
	}

	var ver int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &ver); err != nil || ver != argon2Version {
		return nil, nil, p, errMalformedPHC
	}

	var mem, time uint64
	var threads uint64
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return nil, nil, p, errMalformedPHC
	}
	if mem == 0 || time == 0 || threads == 0 || threads > 255 {
		return nil, nil, p, errMalformedPHC
	}

	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return nil, nil, p, errMalformedPHC
	}
	if hash, err = b64.DecodeString(parts[5]); err != nil || len(hash) == 0 {
		return nil, nil, p, errMalformedPHC
	}

	p = Argon2id{
		MemKiB:  uint32(mem),
		Time:    uint32(time),
		Threads: uint8(threads),
		KeyLen:  uint32(len(hash)),
		SaltLen: uint32(len(salt)),
	}
	return salt, hash, p, nil
}
