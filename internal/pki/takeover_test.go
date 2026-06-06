package pki

import (
	"net"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// TestTakeoverSupersedesOldCert models the takeover supersede (03 §4 / A.6 / A.13
// P7 "takeover supersedes old cert"): a node is adopted (leaf v1), then re-adopted
// by a takeover that signs a NEW leaf (v2) for the same nodeID and adds v1's
// fingerprint to the grow-only RevokedSet. After the swap, the verify hook rejects
// the OLD leaf and accepts the NEW one.
func TestTakeoverSupersedesOldCert(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)

	const nodeID = "n-target"
	addrs := []net.IP{net.IPv4(127, 0, 0, 1)}

	// v1: initial adoption leaf.
	v1 := mustLeaf(t, ca, nodeID, addrs, now)
	fp1 := Fingerprint(v1.Certificate[0])

	// v2: takeover re-signs a fresh leaf for the SAME nodeID (new key, new serial).
	v2 := mustLeaf(t, ca, nodeID, addrs, now)
	fp2 := Fingerprint(v2.Certificate[0])

	if fp1 == fp2 {
		t.Fatal("v1 and v2 fingerprints identical; re-sign did not produce a distinct leaf")
	}

	// The takeover commits v1's fingerprint to the RevokedSet (forget/supersede).
	store := state.New("controller")
	store.Apply(state.ConfigDoc{
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{
			{Fingerprint: fp1, NodeID: nodeID, Reason: "takeover"},
		}},
	})

	v := NewPeerVerifier(revokedFromDoc(store), nil)

	// Old leaf rejected.
	if err := v.Verify([][]byte{v1.Certificate[0]}, nil); err != ErrRevoked {
		t.Errorf("old leaf Verify err=%v, want ErrRevoked", err)
	}
	// New leaf accepted (not revoked).
	if err := v.Verify([][]byte{v2.Certificate[0]}, nil); err != nil {
		t.Errorf("new leaf Verify err=%v, want nil", err)
	}
}

// TestTakeoverRevokedSetGrowOnlyAcrossMerge: the superseded fingerprint, once in
// the RevokedSet, survives a merge from a stale replica that never saw the
// takeover (A.6 grow-only union) — the loser's cert stays rejected cluster-wide.
func TestTakeoverRevokedSetGrowOnlyAcrossMerge(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	old := mustLeaf(t, ca, "n-z", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	fp := Fingerprint(old.Certificate[0])

	store := state.New("self")
	store.Apply(state.ConfigDoc{
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{{Fingerprint: fp, Reason: "takeover"}}},
	})

	// A stale replica (higher version, but empty revoked set) gossips in. LWW takes
	// its doc body, but the union must preserve the revocation.
	store.Merge(state.ConfigDoc{Version: 99}, "stale-peer")

	v := NewPeerVerifier(revokedFromDoc(store), nil)
	if err := v.Verify([][]byte{old.Certificate[0]}, nil); err != ErrRevoked {
		t.Errorf("revocation lost after stale merge: err=%v, want ErrRevoked", err)
	}
}
