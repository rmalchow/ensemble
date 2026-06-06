package web

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
)

// mustLeaf signs a node leaf for nodeID off ca and returns it as a tls.Certificate
// (built from the pki PUBLIC API: NewIdentity -> NewCSR -> CA.Sign -> LeafFromPEM).
func mustLeaf(t *testing.T, ca *pki.CA, nodeID string, now time.Time) tls.Certificate {
	t.Helper()
	key, err := pki.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	csrPEM, err := pki.NewCSR(key, nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		t.Fatal("decode CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	certPEM, err := ca.Sign(csr, nodeID, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("CA.Sign: %v", err)
	}
	leaf, err := pki.LeafFromPEM(certPEM, key.(crypto.Signer))
	if err != nil {
		t.Fatalf("LeafFromPEM: %v", err)
	}
	return leaf
}

// TestIntegrationNodeCertAuth boots a real TLS Server with the pki mTLS config
// (P1.2 ServerTLS + PeerVerifier) and asserts the web [2] node path end-to-end: a
// client presenting a node leaf chained to the cluster CA reaches GET
// /auth/session authenticated as method:"node" with NodeID = the cert CN, while a
// foreign-CA client is rejected at the TLS handshake and never reaches a handler.
func TestIntegrationNodeCertAuth(t *testing.T) {
	now := time.Now()
	ca, err := pki.CreateCA("home", now)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	pool, err := pki.CAPoolFromPEM(ca.CertPEM())
	if err != nil {
		t.Fatalf("CAPoolFromPEM: %v", err)
	}
	verifier := pki.NewPeerVerifier(func(string) bool { return false }, nil)

	serverLeaf := mustLeaf(t, ca, "n-server", now)
	clientLeaf := mustLeaf(t, ca, "n-client", now)

	srv := New(Deps{
		NodeID:        "n-server",
		Initialized:   func() bool { return true },
		ConfigVersion: func() uint64 { return 5 },
	}, "")

	ts := httptest.NewUnstartedServer(srv.mux)
	ts.TLS = pki.ServerTLS(serverLeaf, pool, verifier)
	ts.StartTLS()
	defer ts.Close()

	// Node-cert client: chains to the cluster CA, presents a client leaf.
	clientCfg := pki.ClientTLS(clientLeaf, pool, verifier)
	clientCfg.ServerName = "127.0.0.1"
	nodeClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientCfg}}

	resp, err := nodeClient.Get(ts.URL + "/api/v1/auth/session")
	if err != nil {
		t.Fatalf("node-cert GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("node-cert session: got %d want 200 (%s)", resp.StatusCode, body)
	}
	var sr sessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.Method != string(auth.MethodNode) {
		t.Fatalf("method: got %q want node", sr.Method)
	}
	if sr.NodeID != "n-client" {
		t.Fatalf("nodeId: got %q want n-client (cert CN)", sr.NodeID)
	}

	// NB: P1.2's ServerTLS uses tls.RequireAndVerifyClientCert, so a browser with
	// no client cert cannot complete the handshake on this listener — the [3]
	// human/session path is exercised over plain httptest in api_auth_test.go
	// instead. (See report open-question: 01 §3.1 expects VerifyClientCertIfGiven.)

	// Foreign-CA client: rejected at the TLS handshake, never reaches a handler.
	foreignCA, _ := pki.CreateCA("evil", now)
	foreignLeaf := mustLeaf(t, foreignCA, "n-evil", now)
	foreignCfg := pki.ClientTLS(foreignLeaf, pool, verifier)
	foreignCfg.ServerName = "127.0.0.1"
	foreignClient := &http.Client{Transport: &http.Transport{TLSClientConfig: foreignCfg}}
	if _, err := foreignClient.Get(ts.URL + "/api/v1/auth/session"); err == nil {
		t.Fatal("foreign-CA client was accepted; want handshake rejection")
	}
}
