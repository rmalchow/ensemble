package pki

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"
)

// leafValidity mirrors the A.12 leaf lifetime callers pass to Sign.
const leafValidity = 30 * 24 * time.Hour

func mustCreateCA(t *testing.T, name string, now time.Time) *CA {
	t.Helper()
	ca, err := CreateCA(name, now)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	return ca
}

func TestCreateCA(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	ca := mustCreateCA(t, "living-room", now)

	c := ca.Cert
	if !c.IsCA {
		t.Error("cert.IsCA = false, want true")
	}
	if !c.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	if c.MaxPathLen != 0 || !c.MaxPathLenZero {
		t.Errorf("MaxPathLen=%d MaxPathLenZero=%v, want 0/true", c.MaxPathLen, c.MaxPathLenZero)
	}
	if want := x509.KeyUsageCertSign | x509.KeyUsageCRLSign; c.KeyUsage != want {
		t.Errorf("KeyUsage=%v, want %v", c.KeyUsage, want)
	}
	if want := "ensemble-ca/living-room"; c.Subject.CommonName != want {
		t.Errorf("CN=%q, want %q", c.Subject.CommonName, want)
	}
	if wantAfter := now.AddDate(10, 0, 0); !c.NotAfter.Equal(wantAfter) {
		t.Errorf("NotAfter=%v, want %v", c.NotAfter, wantAfter)
	}
	if !c.NotBefore.Equal(now) {
		t.Errorf("NotBefore=%v, want %v", c.NotBefore, now)
	}
	if _, ok := c.PublicKey.(ed25519.PublicKey); !ok {
		t.Errorf("public key type %T, want ed25519.PublicKey", c.PublicKey)
	}
	if c.SignatureAlgorithm != x509.PureEd25519 {
		t.Errorf("SignatureAlgorithm=%v, want PureEd25519", c.SignatureAlgorithm)
	}
	if c.SerialNumber.Sign() <= 0 {
		t.Errorf("serial=%v, want positive non-zero", c.SerialNumber)
	}
	if c.SerialNumber.BitLen() > 128 {
		t.Errorf("serial bitlen=%d, want <=128", c.SerialNumber.BitLen())
	}

	block, _ := pem.Decode(ca.CertPEM())
	if block == nil || block.Type != pemTypeCert {
		t.Fatalf("CertPEM is not a CERTIFICATE block")
	}
}

func signLeaf(t *testing.T, ca *CA, nodeID string, addrs []net.IP, now time.Time) (*x509.Certificate, crypto.Signer) {
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
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf, key
}

func parseCSR(t *testing.T, csrPEM []byte) *x509.CertificateRequest {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("CSR PEM decode failed")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	return csr
}

func ipEqual(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func TestSign(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	ca := mustCreateCA(t, "lr", now)

	tests := []struct {
		name   string
		nodeID string
		addrs  []net.IP
	}{
		{"single IPv4", "n-aaaa", []net.IP{net.IPv4(192, 168, 1, 10)}},
		{"IPv4+IPv6", "n-bbbb", []net.IP{net.IPv4(192, 168, 1, 11), net.ParseIP("fe80::1")}},
		{"no addrs", "n-cccc", nil},
		{"multi-addr", "n-dddd", []net.IP{net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), net.IPv4(10, 0, 0, 3)}},
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leaf, _ := signLeaf(t, ca, tc.nodeID, tc.addrs, now)

			if leaf.Subject.CommonName != tc.nodeID {
				t.Errorf("CN=%q, want %q", leaf.Subject.CommonName, tc.nodeID)
			}
			wantURI := "ensemble://node/" + tc.nodeID
			if len(leaf.URIs) != 1 || leaf.URIs[0].String() != wantURI {
				t.Errorf("URIs=%v, want [%q]", leaf.URIs, wantURI)
			}
			// Normalize expected IPs to canonical form for compare.
			if !ipEqual(leaf.IPAddresses, tc.addrs) && !(len(leaf.IPAddresses) == 0 && len(tc.addrs) == 0) {
				t.Errorf("IPAddresses=%v, want %v", leaf.IPAddresses, tc.addrs)
			}
			wantDNS := tc.nodeID + ".ensemble.local"
			if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != wantDNS {
				t.Errorf("DNSNames=%v, want [%q]", leaf.DNSNames, wantDNS)
			}
			if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
				t.Errorf("KeyUsage=%v, want DigitalSignature", leaf.KeyUsage)
			}
			hasServer, hasClient := false, false
			for _, eku := range leaf.ExtKeyUsage {
				switch eku {
				case x509.ExtKeyUsageServerAuth:
					hasServer = true
				case x509.ExtKeyUsageClientAuth:
					hasClient = true
				}
			}
			if !hasServer || !hasClient {
				t.Errorf("ExtKeyUsage=%v, want Server+Client", leaf.ExtKeyUsage)
			}
			if want := now.Add(-skewBackdate); !leaf.NotBefore.Equal(want) {
				t.Errorf("NotBefore=%v, want %v", leaf.NotBefore, want)
			}
			if want := now.Add(leafValidity); !leaf.NotAfter.Equal(want) {
				t.Errorf("NotAfter=%v, want %v", leaf.NotAfter, want)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{
				Roots:       pool,
				CurrentTime: now,
				KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			}); err != nil {
				t.Errorf("leaf does not chain to CA: %v", err)
			}
		})
	}
}

func TestSignIgnoresCSRSANs(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	ca := mustCreateCA(t, "lr", now)

	key, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// Build a CSR that smuggles a foreign IP SAN and a bogus CN.
	tmpl := &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "evil"},
		IPAddresses: []net.IP{net.IPv4(10, 0, 0, 99)},
		DNSNames:    []string{"evil.example"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}

	authedAddrs := []net.IP{net.IPv4(192, 168, 1, 50)}
	certPEM, err := ca.Sign(csr, "n-good", authedAddrs, leafValidity, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	leaf, _ := x509.ParseCertificate(block.Bytes)

	if leaf.Subject.CommonName != "n-good" {
		t.Errorf("CN=%q, want n-good (CSR CN must be ignored)", leaf.Subject.CommonName)
	}
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.IPv4(10, 0, 0, 99)) {
			t.Error("issued leaf carries the CSR's smuggled IP SAN")
		}
	}
	for _, dns := range leaf.DNSNames {
		if dns == "evil.example" {
			t.Error("issued leaf carries the CSR's smuggled DNS SAN")
		}
	}
}

func TestSignRejectsBrokenCSRSignature(t *testing.T) {
	now := time.Now()
	ca := mustCreateCA(t, "lr", now)
	key, _ := NewIdentity()
	csrPEM, err := NewCSR(key, "n-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	// Mutate a byte deep in the DER (in the signature/body region).
	mutated := append([]byte(nil), block.Bytes...)
	mutated[len(mutated)-1] ^= 0xFF
	csr, err := x509.ParseCertificateRequest(mutated)
	if err != nil {
		// Mutation may also break parsing; that is an acceptable rejection.
		return
	}
	if _, err := ca.Sign(csr, "n-aaaa", nil, leafValidity, now); err == nil {
		t.Error("Sign accepted a CSR with a broken self-signature")
	}
}

func TestMarshalParseCAKeyRoundTrip(t *testing.T) {
	ca := mustCreateCA(t, "lr", time.Now())
	pemBytes, err := MarshalCAKey(ca.key)
	if err != nil {
		t.Fatalf("MarshalCAKey: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("PEM type=%q, want PRIVATE KEY", blockType(block))
	}
	if strings.Contains(string(pemBytes), "ENCRYPTED") {
		t.Error("CA key PEM contains ENCRYPTED; D18 requires plaintext")
	}

	parsed, err := ParseCAKey(pemBytes)
	if err != nil {
		t.Fatalf("ParseCAKey: %v", err)
	}
	msg := []byte("trust origin")
	sig := ed25519.Sign(parsed.(ed25519.PrivateKey), msg)
	pub := parsed.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("parsed key signature does not verify")
	}
}

func TestParseCA(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	orig := mustCreateCA(t, "lr", now)
	keyPEM, err := MarshalCAKey(orig.key)
	if err != nil {
		t.Fatal(err)
	}

	ca, err := ParseCA(orig.CertPEM(), keyPEM)
	if err != nil {
		t.Fatalf("ParseCA: %v", err)
	}
	if !bytes.Equal(ca.Cert.Raw, orig.Cert.Raw) {
		t.Error("reconstructed CA cert differs from original")
	}

	// The reconstructed CA can sign a leaf that chains to the original CA cert.
	leaf, _ := signLeaf(t, ca, "n-eeee", []net.IP{net.IPv4(127, 0, 0, 1)}, now)
	pool := x509.NewCertPool()
	pool.AddCert(orig.Cert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("leaf from ParseCA does not chain to original CA: %v", err)
	}
}

func TestParseCARejectsGarbage(t *testing.T) {
	if _, err := ParseCA([]byte("not pem"), []byte("nope")); err == nil {
		t.Error("ParseCA accepted garbage cert PEM")
	}
	if _, err := ParseCAKey([]byte("not pem")); err == nil {
		t.Error("ParseCAKey accepted garbage")
	}
}

// TestCASignerAndKeyPEMRoundTrip asserts the GAP 1 accessors: Signer() hands back
// the live signer (its public key matches the CA cert's), and KeyPEM() marshals a
// key that ParseCAKey re-parses into an equivalent signer (so genesis can persist
// ClusterSecrets.caKeyPEM and reload it).
func TestCASignerAndKeyPEMRoundTrip(t *testing.T) {
	ca, err := CreateCA("home", time.Now())
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}

	signer := ca.Signer()
	if signer == nil {
		t.Fatal("Signer() returned nil")
	}
	caPub, ok := ca.Cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("CA cert public key type = %T, want ed25519.PublicKey", ca.Cert.PublicKey)
	}
	signerPub, ok := signer.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("Signer().Public() type = %T, want ed25519.PublicKey", signer.Public())
	}
	if !signerPub.Equal(caPub) {
		t.Error("Signer() public key does not match the CA certificate public key")
	}

	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}
	want, err := MarshalCAKey(signer)
	if err != nil {
		t.Fatalf("MarshalCAKey: %v", err)
	}
	if !bytes.Equal(keyPEM, want) {
		t.Error("KeyPEM() != MarshalCAKey(Signer())")
	}
	reparsed, err := ParseCAKey(keyPEM)
	if err != nil {
		t.Fatalf("ParseCAKey(KeyPEM()): %v", err)
	}
	rePub, ok := reparsed.Public().(ed25519.PublicKey)
	if !ok || !rePub.Equal(caPub) {
		t.Error("KeyPEM round-trip lost the key identity")
	}
}

func blockType(b *pem.Block) string {
	if b == nil {
		return "<nil>"
	}
	return b.Type
}
