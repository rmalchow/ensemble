package pki

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestNewIdentity(t *testing.T) {
	k1, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if _, ok := k1.(ed25519.PrivateKey); !ok {
		t.Errorf("key type %T, want ed25519.PrivateKey", k1)
	}

	k2, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if k1.(ed25519.PrivateKey).Equal(k2) {
		t.Error("two NewIdentity calls returned the same key")
	}
}

func TestNewCSR(t *testing.T) {
	key, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "n-7a3f"
	csrPEM, err := NewCSR(key, nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != pemTypeCertReq {
		t.Fatalf("PEM type=%q, want CERTIFICATE REQUEST", blockType(block))
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CSR self-signature invalid: %v", err)
	}
	if csr.Subject.CommonName != nodeID {
		t.Errorf("CN=%q, want %q", csr.Subject.CommonName, nodeID)
	}

	csrPub, ok := csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("CSR public key type %T", csr.PublicKey)
	}
	keyPub := key.Public().(ed25519.PublicKey)
	if !bytes.Equal(csrPub, keyPub) {
		t.Error("CSR public key does not match the signer")
	}

	if len(csr.IPAddresses) != 0 || len(csr.DNSNames) != 0 || len(csr.URIs) != 0 {
		t.Errorf("CSR carries SANs (IP=%v DNS=%v URI=%v), want none",
			csr.IPAddresses, csr.DNSNames, csr.URIs)
	}
}
