package pki

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
)

// NewIdentity generates a node's Ed25519 keypair. The private key NEVER leaves
// the node (doc 03 §1.3).
func NewIdentity() (crypto.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generate node key: %w", err)
	}
	return key, nil
}

// NewCSR builds a self-signed CSR: Subject.CN = nodeID, PublicKey = key's public,
// signature proves possession. SANs are deliberately NOT set (the CA ignores CSR
// SANs and sets them from authenticated inputs, doc 03 §1.3).
func NewCSR(key crypto.Signer, nodeID string) (csrPEM []byte, err error) {
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nodeID},
		// SignatureAlgorithm left zero so stdlib selects PureEd25519.
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("pki: create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertReq, Bytes: der}), nil
}
