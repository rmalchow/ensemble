package adopt

import "errors"

// Sentinel errors for the A.9 handshake and the online brute-force guard. They
// are matchable with errors.Is so the web layer can map each to the right HTTP
// status (08 §A.2 / §0.4) without importing the engine internals:
//
//	ErrEpochMismatch  -> 422 unprocessable   (m7, 03 §2.7)
//	ErrBadPIN         -> 401 unauthenticated  (PIN-keyed tag mismatch)
//	ErrLockedOut      -> 429 rate_limited     (hard 15-min lockout, A.12)
//	ErrRateLimited    -> 429 rate_limited     (soft backoff after 3 fails, A.12)
//	ErrNonceExpired   -> 401 unauthenticated  (single-use nonce past its 30 s TTL)
//	ErrNonceUnknown   -> 401 unauthenticated  (unknown/replayed nonce)
//	ErrForeign        -> 403 forbidden         (target belongs to another cluster; needs takeover)
//	ErrBadPayload     -> 400 invalid_request   (malformed AEAD payload / framing)
var (
	// ErrEpochMismatch is returned by the node at phase=key when the controller's
	// protocolEpoch differs from this build's (m7, 03 §2.7) — refused BEFORE any
	// PIN work.
	ErrEpochMismatch = errors.New("adopt: protocol epoch mismatch")
	// ErrBadPIN is returned when a PIN-keyed HMAC tag fails the constant-time
	// compare (wrong PIN or a MITM splice). The handler reports the source to the
	// guard and returns 401.
	ErrBadPIN = errors.New("adopt: bad PIN proof")
	// ErrLockedOut is returned by the guard when src (or the cluster globally) is
	// inside an active hard lockout (A.12: 15 min after 10 fails / 5 min).
	ErrLockedOut = errors.New("adopt: locked out")
	// ErrRateLimited is returned by the guard for the soft backoff after 3
	// consecutive fails (A.12).
	ErrRateLimited = errors.New("adopt: rate limited")
	// ErrNonceExpired is returned when a consumed nonce is past its NonceTTL (30 s).
	ErrNonceExpired = errors.New("adopt: nonce expired")
	// ErrNonceUnknown is returned when a nonce is unknown or already burned (replay).
	ErrNonceUnknown = errors.New("adopt: nonce unknown or already used")
	// ErrForeign is returned by Controller.Run when the target reports state
	// "foreign" (already a member of a different cluster) and force=false; the
	// operator must use takeover (03 §4).
	ErrForeign = errors.New("adopt: target belongs to another cluster (needs takeover)")
	// ErrBadPayload is returned when the complete-phase AEAD payload framing is
	// malformed (length prefixes do not match the buffer).
	ErrBadPayload = errors.New("adopt: malformed payload framing")
	// ErrSessionUnknown is returned when no NodeSession matches the supplied nonce
	// (expired session or a phase out of order).
	ErrSessionUnknown = errors.New("adopt: handshake session unknown or expired")
)
