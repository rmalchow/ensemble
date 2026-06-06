package pki

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"testing"
	"time"
)

func TestFingerprintGolden(t *testing.T) {
	der := []byte("fixed der bytes for golden test")
	sum := sha256.Sum256(der)
	want := hex.EncodeToString(sum[:])
	if got := Fingerprint(der); got != want {
		t.Errorf("Fingerprint=%q, want %q", got, want)
	}
	// Sanity: 64 lowercase hex chars.
	if got := Fingerprint(der); len(got) != 64 {
		t.Errorf("fingerprint len=%d, want 64", len(got))
	}
}

func TestVerifyRevokedSet(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-aaaa", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	leafDER := leaf.Certificate[0]
	fp := Fingerprint(leafDER)
	rawCerts := [][]byte{leafDER}

	tests := []struct {
		name    string
		revoked func(string) bool
		knownID func(string) bool
		wantErr error
	}{
		{
			name:    "not revoked, no knownID",
			revoked: func(string) bool { return false },
			knownID: nil,
			wantErr: nil,
		},
		{
			name:    "revoked",
			revoked: func(f string) bool { return ConstantTimeEqualFingerprint(f, fp) },
			knownID: nil,
			wantErr: ErrRevoked,
		},
		{
			name:    "knownID accepts",
			revoked: func(string) bool { return false },
			knownID: func(id string) bool { return id == "n-aaaa" },
			wantErr: nil,
		},
		{
			name:    "knownID rejects",
			revoked: func(string) bool { return false },
			knownID: func(string) bool { return false },
			wantErr: errUnknownPeer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := NewPeerVerifier(tc.revoked, tc.knownID)
			err := v.Verify(rawCerts, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Verify err=%v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyReadsLiveSnapshot(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-bbbb", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	rawCerts := [][]byte{leaf.Certificate[0]}

	revoked := false
	v := NewPeerVerifier(func(string) bool { return revoked }, nil)

	if err := v.Verify(rawCerts, nil); err != nil {
		t.Errorf("first Verify err=%v, want nil", err)
	}
	revoked = true // gossiped revocation, no restart
	if err := v.Verify(rawCerts, nil); !errors.Is(err, ErrRevoked) {
		t.Errorf("second Verify err=%v, want ErrRevoked", err)
	}
}

func TestVerifyNoCerts(t *testing.T) {
	v := NewPeerVerifier(func(string) bool { return false }, nil)
	if err := v.Verify(nil, nil); err == nil {
		t.Error("Verify accepted empty rawCerts")
	}
}

func TestConstantTimeEqualFingerprint(t *testing.T) {
	if !ConstantTimeEqualFingerprint("abc", "abc") {
		t.Error("equal fingerprints reported unequal")
	}
	if ConstantTimeEqualFingerprint("abc", "abd") {
		t.Error("unequal fingerprints reported equal")
	}
}
