package state

// This file declares the canonical authentication credential types that live in
// the replicated ConfigDoc (07 §2.3 / P2.1). They are defined here so the leaf
// credential package internal/auth (P1, built before the full ConfigDoc engine
// of P2.1) can consume the canonical names — auth imports state for these types
// only, never the reverse (no cycle: state is pure schema). When the full
// ConfigDoc store lands (P2.1) it builds on these declarations unchanged.

// AuthConfig holds the single cluster admin credential and the revocable API
// keys (D11). Only verifier material (argon2id / salted-SHA-256 hashes) is
// stored — never a plaintext password or raw key. Replicated so any node
// authenticates the same admin/keys; see 03 §7 and 08 (auth endpoints).
type AuthConfig struct {
	AdminHash string   `json:"adminHash"`         // argon2id encoded hash of the admin password (PHC string)
	Argon     Argon2id `json:"argon"`             // argon2id cost params used for AdminHash
	APIKeys   []APIKey `json:"apiKeys"`           // revocable API keys (hashed)
	PINHash   string   `json:"pinHash,omitempty"` // argon2id hash of the adoption PIN (D9); "" => default "0000"
}

// Argon2id captures the cost parameters so every node verifies with identical
// settings and the admin can raise cost cluster-wide via a single versioned
// write. The per-credential salt is embedded in each PHC hash string, so these
// are the GENERATION params for the next credential write; existing hashes
// self-describe their own params for verification.
type Argon2id struct {
	MemKiB  uint32 `json:"memKiB"`  // memory cost in KiB (e.g. 65536 = 64 MiB)
	Time    uint32 `json:"time"`    // iterations (e.g. 3)
	Threads uint8  `json:"threads"` // parallelism (e.g. 4)
	KeyLen  uint32 `json:"keyLen"`  // output length in bytes (e.g. 32)
	SaltLen uint32 `json:"saltLen"` // per-credential salt length in bytes (e.g. 16)
}

// APIKey is one revocable key. The raw key is shown exactly once at creation
// (08); only Hash is persisted. Revocation = remove from this slice (keys are
// not certs, so RevokedSet does not apply). Hash is salted SHA-256 in the form
// "<saltHex>$<hashHex>" (03 §7.3): API keys are high-entropy so a fast salted
// hash + constant-time compare suffices; argon2id is reserved for the
// low-entropy human password.
type APIKey struct {
	ID       string `json:"id"`                 // opaque key id (also the lookup handle)
	Name     string `json:"name"`               // human label ("kitchen-tablet")
	Hash     string `json:"hash"`               // salted SHA-256 of the raw key: "saltHex$hashHex"
	Created  string `json:"created"`            // RFC3339
	LastUsed string `json:"lastUsed,omitempty"` // RFC3339; best-effort hint, never an authz input
}
