package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// selfSignedBootstrapCert mints the node's self-signed bootstrap TLS leaf and
// returns the cert + its sha256 fingerprint (the value an operator pins, 03 §2.2).
func selfSignedBootstrapCert(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ensemble-bootstrap"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("self-sign bootstrap cert: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return cert, "sha256:" + pki.Fingerprint(der)
}

// fakeRekeyer records rekey calls.
type fakeRekeyer struct{ keys [][]byte }

func (f *fakeRekeyer) Rekey(key []byte) error { f.keys = append(f.keys, key); return nil }

// TestWireAdoptForgetIntegration is the A.13 P2 acceptance at the cmd layer: a
// controller (real pki.CA + state.Store) adopts an uninitialized node B over a
// real self-signed-pinned httptest TLS server; B installs a CA-signed leaf; the
// ConfigDoc converges with B's NodeRecord. Then the controller forgets B: its
// fingerprint enters the grow-only RevokedSet, its NodeRecord is dropped, it is
// pulled from its group, and a gossip rekey fires.
func TestWireAdoptForgetIntegration(t *testing.T) {
	// --- node B: uninitialized, exposes /bootstrap/* over self-signed TLS. ---
	nodeLeafKey, err := pki.NewIdentity()
	if err != nil {
		t.Fatalf("node identity: %v", err)
	}
	nodeGuard := newGuardAdapter(auth.NewAdoptionGuard(), nil)
	var installed adopt.Installed
	bd := (&bootstrapDeps{
		nodeID:  "n-B",
		leafKey: nodeLeafKey,
		guard:   nodeGuard,
		info: func() web.BootstrapInfo {
			return web.BootstrapInfo{NodeID: "n-B", State: "uninitialized", ProtocolEpoch: adopt.ProtocolEpoch}
		},
		install: func(inst adopt.Installed) error { installed = inst; return nil },
	}).build("0000")
	nodeSrv := web.New(web.Deps{NodeID: "n-B", Bootstrap: bd}, "")

	bootCert, fingerprint := selfSignedBootstrapCert(t)
	ts := httptest.NewUnstartedServer(nodeSrv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{bootCert}}
	ts.StartTLS()
	defer ts.Close()

	// --- controller A: real CA + state.Store. ---
	ca, err := pki.CreateCA("home", time.Now())
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	store := state.New("n-A")
	// Seed a genesis doc (version 1) so the adopt write has a base version and a
	// group B will join (to assert forget pulls it).
	doc := store.Get()
	doc.Cluster = state.ClusterInfo{Name: "home", CACertPEM: string(ca.CertPEM())}
	doc.Nodes = []state.NodeRecord{{ID: "n-A", Name: "Living Room"}}
	if _, err := store.Apply(doc); err != nil {
		t.Fatalf("seed doc: %v", err)
	}

	gossipKey := pki.NewGossipKey()
	// The replicated secret bundle carries the CA private key PEM; the CA's own key
	// is unexported, so the test stands in an independent key PEM and only asserts
	// the bundle is delivered + decrypted intact by the node.
	_, secretKey, _ := ed25519.GenerateKey(rand.Reader)
	caKeyPEM, _ := pki.MarshalCAKey(secretKey)
	ctrl := &adoptController{
		store:       store,
		ca:          ca,
		clusterName: "home",
		secrets:     adopt.ClusterSecrets{CAKeyPEM: caKeyPEM, GossipKey: gossipKey},
		httpClient:  ts.Client(), // trusts the httptest self-signed cert
	}
	// Re-point dialTarget at the httptest URL: the client already trusts the cert,
	// so the fingerprint is asserted via the explicit pin too.
	adoptFn := ctrl.newAdoptFuncTo(ts.URL)

	// --- adopt B. ---
	if err := adoptFn("127.0.0.1", fingerprint, "0000", "n-B", "Bedroom", false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// B installed a CA-signed leaf chaining to A's CA.
	if len(installed.LeafPEM) == 0 {
		t.Fatal("node B did not install a leaf")
	}
	block, _ := pem.Decode(installed.LeafPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse installed leaf: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("append CA")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("installed leaf does not chain to A's CA: %v", err)
	}
	if string(installed.Secrets.CAKeyPEM) != string(caKeyPEM) {
		t.Error("node B did not receive the CA key secret")
	}

	// ConfigDoc converged with B's NodeRecord.
	doc = store.Get()
	var bRec *state.NodeRecord
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == "n-B" {
			bRec = &doc.Nodes[i]
		}
	}
	if bRec == nil {
		t.Fatal("ConfigDoc did not converge with n-B")
	}
	if bRec.Name != "Bedroom" || len(bRec.Addrs) != 1 || bRec.Addrs[0] != "127.0.0.1" {
		t.Fatalf("n-B record = %+v", *bRec)
	}

	// Put B into a group so forget can pull it.
	doc = store.Get()
	doc.Groups = []state.GroupRecord{{ID: "g-kitchen", MemberNodeIDs: []string{"n-A", "n-B"}}}
	if _, err := store.Apply(doc); err != nil {
		t.Fatalf("add group: %v", err)
	}

	// --- forget B. ---
	rk := &fakeRekeyer{}
	forgetFn := ctrl.newForgetFunc(rk, pki.NewGossipKey)
	if err := forgetFn("n-B"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	doc = store.Get()
	// NodeRecord dropped.
	for _, n := range doc.Nodes {
		if n.ID == "n-B" {
			t.Fatal("n-B still present after forget")
		}
	}
	// Pulled from the group.
	for _, g := range doc.Groups {
		for _, m := range g.MemberNodeIDs {
			if m == "n-B" {
				t.Fatal("n-B still in group after forget")
			}
		}
	}
	// Fingerprint in the grow-only RevokedSet (matches the installed leaf DER, the
	// value pki.PeerVerifier checks).
	wantFP := pki.Fingerprint(leaf.Raw)
	found := false
	for _, e := range doc.Revoked.Entries {
		if e.Fingerprint == wantFP && e.Reason == "forget" {
			found = true
		}
	}
	if !found {
		t.Fatalf("forget did not add n-B fingerprint %s to RevokedSet: %+v", wantFP, doc.Revoked.Entries)
	}
	// Gossip rekey fired.
	if len(rk.keys) != 1 {
		t.Fatalf("rekey calls = %d, want 1", len(rk.keys))
	}

	// The forgotten leaf is now rejected by the PeerVerifier (mTLS verify hook).
	v := pki.NewPeerVerifier(func(fp string) bool {
		for _, e := range doc.Revoked.Entries {
			if pki.ConstantTimeEqualFingerprint(e.Fingerprint, fp) {
				return true
			}
		}
		return false
	}, nil)
	if err := v.Verify([][]byte{leaf.Raw}, nil); !errors.Is(err, pki.ErrRevoked) {
		t.Fatalf("PeerVerifier err = %v, want ErrRevoked", err)
	}
}

func TestWireAdoptForeignNeedsForce(t *testing.T) {
	nodeLeafKey, _ := pki.NewIdentity()
	guard := newGuardAdapter(auth.NewAdoptionGuard(), nil)
	bd := (&bootstrapDeps{
		nodeID: "n-B", leafKey: nodeLeafKey, guard: guard,
		info:    func() web.BootstrapInfo { return web.BootstrapInfo{NodeID: "n-B", State: "foreign", ProtocolEpoch: 1} },
		install: func(adopt.Installed) error { return nil },
	}).build("0000")
	nodeSrv := web.New(web.Deps{NodeID: "n-B", Bootstrap: bd}, "")
	bootCert, fp := selfSignedBootstrapCert(t)
	ts := httptest.NewUnstartedServer(nodeSrv.Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{bootCert}}
	ts.StartTLS()
	defer ts.Close()

	ca, _ := pki.CreateCA("home", time.Now())
	store := state.New("n-A")
	d := store.Get()
	d.Nodes = []state.NodeRecord{{ID: "n-A"}}
	store.Apply(d)
	ctrl := &adoptController{store: store, ca: ca, clusterName: "home", httpClient: ts.Client()}

	if err := ctrl.newAdoptFuncTo(ts.URL)("127.0.0.1", fp, "0000", "n-B", "B", false); !errors.Is(err, web.ErrForeign) {
		t.Fatalf("foreign force=false err = %v, want ErrForeign", err)
	}
}

func TestGuardAdapterNonceRoundTrip(t *testing.T) {
	a := newGuardAdapter(auth.NewAdoptionGuard(), nil)
	n := a.IssueNonce()
	if len(n) == 0 {
		t.Fatal("nil nonce")
	}
	if err := a.ConsumeNonce(n); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := a.ConsumeNonce(n); !errors.Is(err, adopt.ErrNonceUnknown) {
		t.Fatalf("second consume = %v, want ErrNonceUnknown", err)
	}
}
