package pki

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func newVerifier() *PeerVerifier {
	return NewPeerVerifier(func(string) bool { return false }, nil)
}

// mustLeaf signs a node leaf with ca and assembles it into a tls.Certificate.
func mustLeaf(t *testing.T, ca *CA, nodeID string, addrs []net.IP, now time.Time) tls.Certificate {
	t.Helper()
	key, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	csrPEM, err := NewCSR(key, nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr := parseCSR(t, csrPEM)
	certPEM, err := ca.Sign(csr, nodeID, addrs, leafValidity, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	leaf, err := LeafFromPEM(certPEM, key)
	if err != nil {
		t.Fatalf("LeafFromPEM: %v", err)
	}
	return leaf
}

func TestServerTLS(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-aaaa", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	pool, err := CAPoolFromPEM(ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}

	cfg := ServerTLS(leaf, pool, newVerifier())
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%x, want TLS1.3", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth=%v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate is nil")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len=%d, want 1", len(cfg.Certificates))
	}
}

func TestClientTLS(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-bbbb", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	pool, err := CAPoolFromPEM(ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}

	cfg := ClientTLS(leaf, pool, newVerifier())
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion=%x, want TLS1.3", cfg.MinVersion)
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs is nil")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate is nil")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len=%d, want 1", len(cfg.Certificates))
	}
	if cfg.ServerName != "" {
		t.Errorf("ServerName=%q, want empty (caller fills per dial)", cfg.ServerName)
	}
}

func TestCAPoolFromPEMRejectsGarbage(t *testing.T) {
	if _, err := CAPoolFromPEM([]byte("not a cert")); err == nil {
		t.Error("CAPoolFromPEM accepted garbage")
	}
}

func TestLeafFromPEMRoundTrip(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	leaf := mustLeaf(t, ca, "n-cccc", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	if leaf.Leaf == nil || leaf.Leaf.Subject.CommonName != "n-cccc" {
		t.Errorf("assembled leaf CN mismatch: %+v", leaf.Leaf)
	}
	if leaf.PrivateKey == nil {
		t.Error("assembled leaf has no private key")
	}
}

func TestLeafFromPEMRejectsGarbage(t *testing.T) {
	key, _ := NewIdentity()
	if _, err := LeafFromPEM([]byte("not pem"), key); err == nil {
		t.Error("LeafFromPEM accepted garbage")
	}
}
