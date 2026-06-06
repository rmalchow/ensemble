package pki

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"errors"
)

// ErrRevoked is returned by Verify when the peer leaf's fingerprint is revoked.
var ErrRevoked = errors.New("pki: peer certificate revoked")

// errUnknownPeer is returned by Verify when the optional knownID cross-check
// rejects the peer's identity (doc 03 §5.4 step 3).
var errUnknownPeer = errors.New("pki: peer identity not a known node")

// PeerVerifier runs the extra post-chain check installed as
// tls.Config.VerifyPeerCertificate on BOTH sides. It reads a LIVE ConfigDoc
// snapshot via the revoked closure so a gossiped revocation takes effect without
// a restart (doc 03 §8, §5.3).
type PeerVerifier struct {
	revoked func(fingerprint string) bool
	knownID func(nodeID string) bool // optional CN/URI-SAN cross-check; may be nil
}

// NewPeerVerifier wraps a live revoked-set predicate. revoked(fp) reports whether
// the lowercase-hex SHA-256 fingerprint is in ConfigDoc.RevokedSet. knownID may be
// nil (the optional CN/URI-SAN cross-check, doc 03 §5.4 step 3).
func NewPeerVerifier(revoked func(fingerprint string) bool, knownID func(nodeID string) bool) *PeerVerifier {
	return &PeerVerifier{revoked: revoked, knownID: knownID}
}

// Verify is the VerifyPeerCertificate callback (rawCerts[0] is the peer leaf).
// Chain-to-CA is already enforced by ClientCAs/RootCAs+ClientAuth; this adds
// the revoked-set reject (and optional id cross-check). It reads the revoked-set
// via the live closure, so a gossiped revocation takes effect without a restart.
func (v *PeerVerifier) Verify(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("pki: no peer certificate")
	}
	leafDER := rawCerts[0]

	// 1. Chain-to-CA already enforced by ClientCAs/RootCAs + ClientAuth.
	// 2. Reject if SHA-256(leaf DER) ∈ ConfigDoc.RevokedSet (live snapshot).
	if v.revoked != nil && v.revoked(Fingerprint(leafDER)) {
		return ErrRevoked
	}

	// 3. (optional) confirm CN matches a current NodeRecord id.
	if v.knownID != nil {
		leaf, err := x509.ParseCertificate(leafDER)
		if err != nil {
			return err
		}
		if !v.knownID(leaf.Subject.CommonName) {
			return errUnknownPeer
		}
	}
	return nil
}

// Fingerprint is the canonical revocation key: lowercase hex of SHA-256 over the
// leaf DER (doc 03 §5.2 / §5.4). Used by forget (to add) and Verify (to test).
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEqualFingerprint reports whether two fingerprints are equal using a
// constant-time compare (idiom from media internal/auth controller.go:116). It is
// exposed so callers that compare secret-material fingerprints avoid early-exit
// timing leaks; the revoked-set predicate itself is owned by the caller (P2).
func ConstantTimeEqualFingerprint(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
