package pki

import (
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// revokedFromDoc builds the live-snapshot revoked predicate the production wiring
// uses: it reads the current ConfigDoc.Revoked set on every call (03 §5.4 / §8 —
// a gossiped revocation takes effect without restart). store is read live.
func revokedFromDoc(store *state.Store) func(string) bool {
	return func(fp string) bool {
		for _, e := range store.Get().Revoked.Entries {
			if ConstantTimeEqualFingerprint(e.Fingerprint, fp) {
				return true
			}
		}
		return false
	}
}

// TestRevokeRejectsClientSide: the SERVER-side verifier rejects a revoked client
// leaf (03 §5.4). (The complementary client-rejects-server direction is
// TestRevokeRejectsServerSide.)
func TestRevokeRejectsClientSide(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	pool, _ := CAPoolFromPEM(ca.CertPEM())

	serverLeaf := mustLeaf(t, ca, "n-srv", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	clientLeaf := mustLeaf(t, ca, "n-cli", []net.IP{net.IPv4(127, 0, 0, 1)}, now)

	store := state.New("n-srv")
	store.Apply(state.ConfigDoc{
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{
			{Fingerprint: Fingerprint(clientLeaf.Certificate[0]), Reason: "forget"},
		}},
	})

	serverV := NewPeerVerifier(revokedFromDoc(store), nil)
	clientV := newVerifier()

	serverCfg := ServerTLS(serverLeaf, pool, serverV)
	clientCfg := ClientTLS(clientLeaf, pool, clientV)
	clientCfg.ServerName = "127.0.0.1"

	res := runHandshake(t, serverCfg, clientCfg)
	if res.serverErr == nil {
		t.Fatal("server accepted a revoked client; want rejection")
	}
	if !strings.Contains(res.serverErr.Error(), ErrRevoked.Error()) {
		t.Errorf("server err=%v, want ErrRevoked", res.serverErr)
	}
}

// TestRevokeRejectsServerSide: the CLIENT-side verifier rejects a revoked SERVER
// leaf — Verify runs on both sides after CA-chain validation (03 §5.4 "on BOTH
// client and server side").
func TestRevokeRejectsServerSide(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	pool, _ := CAPoolFromPEM(ca.CertPEM())

	serverLeaf := mustLeaf(t, ca, "n-srv", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	clientLeaf := mustLeaf(t, ca, "n-cli", []net.IP{net.IPv4(127, 0, 0, 1)}, now)

	store := state.New("n-cli")
	store.Apply(state.ConfigDoc{
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{
			{Fingerprint: Fingerprint(serverLeaf.Certificate[0]), Reason: "forget"},
		}},
	})

	// The CLIENT verifier carries the revoked set; it must reject the server leaf.
	clientV := NewPeerVerifier(revokedFromDoc(store), nil)
	serverV := newVerifier()

	serverCfg := ServerTLS(serverLeaf, pool, serverV)
	clientCfg := ClientTLS(clientLeaf, pool, clientV)
	clientCfg.ServerName = "127.0.0.1"

	res := runHandshake(t, serverCfg, clientCfg)
	if res.clientErr == nil {
		t.Fatal("client accepted a revoked server; want rejection")
	}
	if !strings.Contains(res.clientErr.Error(), ErrRevoked.Error()) {
		t.Errorf("client err=%v, want ErrRevoked", res.clientErr)
	}
}

// TestRevokeLiveSnapshotNoRestart: a verifier reading the live ConfigDoc starts
// permissive, then rejects the same leaf after the revocation is gossiped (Merge)
// — no rebuild of the PeerVerifier, no restart (03 §8).
func TestRevokeLiveSnapshotNoRestart(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-x", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	raw := [][]byte{leaf.Certificate[0]}
	fp := Fingerprint(leaf.Certificate[0])

	store := state.New("self")
	v := NewPeerVerifier(revokedFromDoc(store), nil)

	if err := v.Verify(raw, nil); err != nil {
		t.Fatalf("pre-revoke Verify err=%v, want nil", err)
	}

	// A peer gossips a doc that revokes the leaf. Merge unions it in.
	store.Merge(state.ConfigDoc{
		Version: 1,
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{{Fingerprint: fp, Reason: "forget"}}},
	}, "peer")

	if err := v.Verify(raw, nil); err != ErrRevoked {
		t.Errorf("post-merge Verify err=%v, want ErrRevoked (live snapshot)", err)
	}
}

// TestRevokeUnionNeverResurrects: a replica whose local doc does NOT list the cert
// still rejects it after a union-merge brings the revocation in — the grow-only
// RevokedSet cannot be undone by a stale replica (A.6 / 03 §5.2).
func TestRevokeUnionNeverResurrects(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-y", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	fp := Fingerprint(leaf.Certificate[0])

	// Local store starts at a HIGHER version with an EMPTY revoked set; the remote
	// is older but carries the revocation. LWW would keep the local doc body, but
	// the union must still bring the revocation in (A.6).
	local := state.New("self")
	local.Apply(state.ConfigDoc{}) // version -> 1
	local.Apply(local.Get())       // version -> 2

	local.Merge(state.ConfigDoc{
		Version: 1, // older than local's 2: loses the doc-body LWW
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{{Fingerprint: fp, Reason: "forget"}}},
	}, "peer")

	v := NewPeerVerifier(revokedFromDoc(local), nil)
	if err := v.Verify([][]byte{leaf.Certificate[0]}, nil); err != ErrRevoked {
		t.Errorf("union-merged revocation not honored: err=%v, want ErrRevoked", err)
	}
}

// TestRevokeExpiryBackstop: a leaf past its NotAfter (30-day lifetime, A.12) fails
// CA-chain validation even when it is NOT in the RevokedSet — the expiry backstop
// (03 §5.2). We sign a leaf 40 days in the past so it is expired now, and verify
// the chain build (the production ClientCAs/RootCAs path) rejects it.
func TestRevokeExpiryBackstop(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	pool, _ := CAPoolFromPEM(ca.CertPEM())

	// Leaf issued 40 days ago with the 30-day lifetime → expired ~10 days ago.
	past := now.AddDate(0, 0, -40)
	expiredLeaf := mustLeaf(t, ca, "n-old", []net.IP{net.IPv4(127, 0, 0, 1)}, past)

	leafCert := expiredLeaf.Leaf
	if leafCert == nil {
		t.Fatal("leaf has no parsed cert")
	}
	if !leafCert.NotAfter.Before(now) {
		t.Fatalf("leaf NotAfter=%v not before now=%v; expiry backstop not exercised", leafCert.NotAfter, now)
	}

	// Build the chain exactly as the TLS stack does: verify against the CA pool at
	// the current time. An expired leaf must fail with a CertificateInvalidError.
	_, err := leafCert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now})
	if err == nil {
		t.Error("expired leaf passed chain validation; want expiry rejection")
	}
}
