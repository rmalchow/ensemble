// Package auth is the human-path authentication layer of the control plane
// (D11). It hashes and verifies the single cluster admin password with
// argon2id, mints and validates per-node in-memory sessions (HttpOnly/Secure/
// SameSite=Strict cookies, sliding 12h + absolute 7d TTL), mints/hashes/verifies
// revocable API keys with salted SHA-256, verifies the adoption PIN and runs the
// online-guess guard (A.9/A.12 §3.4 backoff/lockout/nonce TTL), and exposes the
// composable HTTP auth middleware chain that wraps every /api/v1 handler in the
// ordering of 03 §7.4.
//
// It is pure verifier/credential logic: it owns no replicated schema (that is
// internal/state) and never reaches the engine. The node (mTLS) path is
// authenticated by internal/pki's PeerVerifier, injected as Deps.NodeAuth — auth
// itself does not import pki. It imports internal/state only for the canonical
// credential types it consumes (state.Argon2id, state.APIKey) and otherwise only
// the Go stdlib plus golang.org/x/crypto/argon2. Consumed by internal/web and
// cmd (01 §2 layering).
package auth
