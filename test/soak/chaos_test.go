//go:build soak

package soak

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/allowlist"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// mustCA mints a cluster CA for the revoke/forget scenario.
func mustCA(t *testing.T, name string, now time.Time) *pki.CA {
	t.Helper()
	ca, err := pki.CreateCA(name, now)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	return ca
}

// mustNodeLeaf signs a node leaf and returns its DER in the rawCerts form
// PeerVerifier.Verify expects ([][]byte with the leaf at [0]).
func mustNodeLeaf(t *testing.T, ca *pki.CA, nodeID string, now time.Time) [][]byte {
	t.Helper()
	key, err := pki.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	csrPEM, err := pki.NewCSR(key, nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("decode CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	certPEM, err := ca.Sign(csr, nodeID, []net.IP{net.IPv4(10, 0, 0, 9)}, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		t.Fatal("decode cert PEM")
	}
	return [][]byte{cb.Bytes}
}

// TestChaosUniformLoss: under 2% uniform loss (≤ recoverable budget with FEC, but
// here FEC=None so it is pure concealment), the follower keeps cadence — every
// chunk lands at the right sampleIndex (no mis-index) and sync stays sub-ms after
// the loss settles (A.13 P5). With None FEC a dropped chunk is concealed at its
// index, never shifting subsequent audio (the cardinal rule).
func TestChaosUniformLoss(t *testing.T) {
	h := newHarness(t, 2, 1)
	defer h.stop()

	for _, f := range h.followers {
		f.relay.setUniformLoss(2)
	}
	h.waitLocked(3 * time.Second)
	worst := h.sampleSync(2 * time.Second)
	// Concealment keeps cadence; the projected timeline tracks the master clock,
	// so the timeline sync error stays bounded even though some audio is concealed.
	if worst > boundedSync {
		t.Errorf("under 2%% loss worst sync=%v, want <= %v", worst, boundedSync)
	}
	for _, f := range h.followers {
		t.Logf("relay forwarded=%d dropped=%d", f.relay.forwarded.Load(), f.relay.dropped.Load())
		if f.relay.dropped.Load() == 0 {
			t.Error("no packets dropped; loss injector inert")
		}
	}
}

// TestChaosBurstLoss: a burst drop of D=4 packets is concealed without a permanent
// desync — the follower re-locks and holds sub-ms afterward (A.13 P5, A.12 D=4).
func TestChaosBurstLoss(t *testing.T) {
	h := newHarness(t, 1, 1)
	defer h.stop()
	h.waitLocked(3 * time.Second)

	h.followers[0].relay.injectBurst(4) // ~40 ms burst (A.12)
	time.Sleep(200 * time.Millisecond)

	worst := h.sampleSync(2 * time.Second)
	if worst > boundedSync {
		t.Errorf("post-burst worst sync=%v, want <= %v (re-lock)", worst, boundedSync)
	}
}

// TestChaosPartitionHeal: a partition splits a 4-node group into two sides, each
// electing its own master; on heal the group converges to a single master per
// group with a generation bump on the losing side (02 §6, §5.5). This drives the
// REAL cluster election under the partition gate.
func TestChaosPartitionHeal(t *testing.T) {
	const gid = "g1"
	doc := state.ConfigDoc{Groups: []state.GroupRecord{{
		ID: gid, MemberNodeIDs: []string{"n-a", "n-b", "n-c", "n-d"},
	}}}

	part := newPartition()
	part.assign("n-a", 0)
	part.assign("n-b", 0)
	part.assign("n-c", 1)
	part.assign("n-d", 1)
	part.setSplit(true)

	side1 := cluster.NewGroupElections("n-a")
	side2 := cluster.NewGroupElections("n-c")

	aliveSide := func(side int) []cluster.Member {
		ids := []string{"n-a", "n-b", "n-c", "n-d"}
		var out []cluster.Member
		for _, id := range ids {
			// A node sees only same-side peers while split.
			if part.connected(refNode(side), id) {
				out = append(out, cluster.Member{Meta: cluster.Meta{NodeID: id, GroupID: gid}})
			}
		}
		return out
	}

	side1.Recompute(doc, aliveSide(0))
	side2.Recompute(doc, aliveSide(1))
	if side1.Master(gid) != "n-a" || side2.Master(gid) != "n-c" {
		t.Fatalf("split masters: side1=%q side2=%q, want n-a / n-c", side1.Master(gid), side2.Master(gid))
	}
	genLoserBefore := side2.Generation(gid)

	// Heal.
	part.setSplit(false)
	side1.Recompute(doc, aliveSide(0))
	changed2 := side2.Recompute(doc, aliveSide(1))

	if side1.Master(gid) != side2.Master(gid) {
		t.Errorf("post-heal masters disagree: %q vs %q", side1.Master(gid), side2.Master(gid))
	}
	if side2.Master(gid) != "n-a" {
		t.Errorf("post-heal master=%q, want n-a (global min)", side2.Master(gid))
	}
	if out, ok := changed2[gid]; !ok || out.Generation <= genLoserBefore {
		t.Errorf("losing side did not bump generation on demote: %+v", changed2[gid])
	}
}

// TestChaosForgetAndRevoke: a forgotten node's realtime packets are dropped by the
// allowlist AND its cert is rejected by the verify hook (RevokedSet); it cannot
// rejoin (03 §5). This composes the real allowlist.Set and pki.PeerVerifier.
func TestChaosForgetAndRevoke(t *testing.T) {
	now := time.Now()
	ca := mustCA(t, "soak", now)
	leaf := mustNodeLeaf(t, ca, "n-gone", now)
	fp := pki.Fingerprint(leaf[0])

	store := state.New("controller")
	// Before forget: n-gone is a node with a durable addr and alive.
	store.Apply(state.ConfigDoc{
		Nodes: []state.NodeRecord{{ID: "n-gone", Addrs: []string{"10.0.0.9"}}},
	})

	allow := allowlist.New()
	allow.Update(store.Get(), []allowlist.MemberAddr{{Addr: mustNetip("10.0.0.9")}})
	if !allow.Allowed(net.ParseIP("10.0.0.9")) {
		t.Fatal("pre-forget: n-gone should be allowed")
	}

	// Forget: drop from Nodes[], add to RevokedSet, and gossip-rekey so it cannot
	// re-announce (modeled by removing it from the live set too).
	store.Apply(state.ConfigDoc{
		Version: store.Get().Version,
		Revoked: state.RevokedSet{Entries: []state.RevokedCert{{Fingerprint: fp, NodeID: "n-gone", Reason: "forget"}}},
	})
	allow.Update(store.Get(), nil) // no live members

	if allow.Allowed(net.ParseIP("10.0.0.9")) {
		t.Error("post-forget: n-gone realtime packets still allowed; want dropped")
	}

	v := pki.NewPeerVerifier(func(f string) bool {
		for _, e := range store.Get().Revoked.Entries {
			if e.Fingerprint == f {
				return true
			}
		}
		return false
	}, nil)
	if err := v.Verify(leaf, nil); err != pki.ErrRevoked {
		t.Errorf("post-forget: cert Verify err=%v, want ErrRevoked", err)
	}
}

// TestChaosChurnLeak: thousands of failover/flap cycles (start+stop relays and
// gen bumps) leave no goroutine growth (doc P7.1 §5.7 / §7.7 leak check).
func TestChaosChurnLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("leak soak skipped in -short")
	}
	base := goroutineCount()

	for cycle := 0; cycle < 200; cycle++ {
		h := newHarness(t, 2, uint64(cycle+1))
		h.waitLocked(2 * time.Second)
		ms, _, _ := h.mtl.NowSample()
		h.bumpGen(ms)
		h.stop()
	}

	after := goroutineCount()
	// Allow a small slack for runtime/pool goroutines.
	if after > base+10 {
		t.Errorf("goroutine leak: base=%d after=%d (+%d) over 200 cycles", base, after, after-base)
	}
	t.Logf("goroutines base=%d after=%d", base, after)
}

// refNode maps a side index to a representative node id on that side (for the
// partition connectivity query).
func refNode(side int) string {
	if side == 0 {
		return "n-a"
	}
	return "n-c"
}
