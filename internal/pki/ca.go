package pki

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"
)

// caValidityYears is the cluster CA certificate lifetime in years (doc 03 §1.1
// "validity 10 years"; NotAfter = now.AddDate(10, 0, 0)). Not in the A.12 table,
// owned by 03 §1.1.
const caValidityYears = 10

// skewBackdate is how far a leaf's NotBefore is moved into the past so a freshly
// issued cert is usable before clock sync converges (doc 03 §1.3 "5-min
// backdate absorbs clock skew").
const skewBackdate = 5 * time.Minute

// PEM block types used by this package.
const (
	pemTypeCert    = "CERTIFICATE"
	pemTypeCertReq = "CERTIFICATE REQUEST"
	pemTypePrivKey = "PRIVATE KEY" // PKCS#8, plaintext (D18: no "ENCRYPTED")
)

// CA is the cluster certificate authority. Cert is public (mirrored into
// ConfigDoc.Cluster); key is the PRIVATE Ed25519 signer (plaintext-replicated to
// full nodes as ClusterSecrets.caKeyPEM, D18 §1.2).
type CA struct {
	Cert    *x509.Certificate // public; mirrored into ConfigDoc.Cluster
	certPEM []byte            // public CA cert PEM (publish into ConfigDoc.Cluster)
	key     crypto.Signer     // PRIVATE Ed25519 signer
}

// CertPEM returns the public CA certificate PEM to publish into ConfigDoc.Cluster.
func (ca *CA) CertPEM() []byte {
	return ca.certPEM
}

// Signer returns the CA's PRIVATE Ed25519 signer. It is exposed so genesis can
// persist the freshly-minted key into ClusterSecrets.caKeyPEM (D18 §1.2) without
// re-deriving it; the key still never leaves the node except as the plaintext-
// replicated cluster secret. Treat the result as secret material.
func (ca *CA) Signer() crypto.Signer {
	return ca.key
}

// KeyPEM marshals the CA private key to plaintext PKCS#8 PEM (the round-trip
// source for ClusterSecrets.caKeyPEM, D18 §1.2). It is the convenience pairing of
// Signer()+MarshalCAKey genesis uses to capture the key for the ConfigDoc.
func (ca *CA) KeyPEM() ([]byte, error) {
	return MarshalCAKey(ca.key)
}

// randomSerial returns a random 128-bit positive serial number (doc 03 §1.1/§1.3).
func randomSerial() (*big.Int, error) {
	// Upper bound 2^128; rand.Int yields [0, max), all non-negative.
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("pki: serial number: %w", err)
	}
	return serial, nil
}

// CreateCA mints a fresh cluster CA on first-init (doc 03 §1.1). Ed25519 keypair,
// self-signed CA cert: CN="ensemble-ca/<clusterName>", IsCA=true,
// BasicConstraintsValid=true, MaxPathLen=0, KeyUsage=CertSign|CRLSign,
// validity 10 years from now.
func CreateCA(clusterName string, now time.Time) (*CA, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ensemble-ca/" + clusterName},
		NotBefore:             now,
		NotAfter:              now.AddDate(caValidityYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true, // signs leaves only, never intermediates
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// SignatureAlgorithm intentionally left zero so stdlib selects
		// PureEd25519 for the Ed25519 signer (doc P1.1 §9 risk 1).
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("pki: self-sign CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA cert: %w", err)
	}

	return &CA{
		Cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}),
		key:     key,
	}, nil
}

// Sign issues a leaf certificate for a CSR (doc 03 §1.1, §1.3). SANs are set from
// AUTHENTICATED inputs (nodeID, addrs) — the CSR's own SANs are ignored. The CSR
// signature (proof of possession) IS verified. validity is the leaf lifetime
// (30 days, A.12); NotBefore is now-5min (skew backdate, doc 03 §1.3).
func (ca *CA) Sign(csr *x509.CertificateRequest, nodeID string, addrs []net.IP, validity time.Duration, now time.Time) (certPEM []byte, err error) {
	// Proof of possession: the CSR must carry a valid self-signature.
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("pki: CSR signature: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	// SAN URI is the canonical address-independent identity (doc 03 §1.3/§5.2).
	uriSAN := &url.URL{Scheme: "ensemble", Host: "node", Path: "/" + nodeID}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    now.Add(-skewBackdate),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		// SANs set from authenticated inputs, NOT copied from the CSR.
		IPAddresses: addrs,
		DNSNames:    []string{nodeID + ".ensemble.local"},
		URIs:        []*url.URL{uriSAN},
	}

	// The leaf's public key is taken from the (authenticated) CSR; everything
	// else is set by the CA.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("pki: sign leaf: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: der}), nil
}

// MarshalCAKey moves the CA private key out as plaintext PKCS#8 PEM (D18 §1.2:
// no sealing, no AEAD at rest). Round-trip target: ClusterSecrets.caKeyPEM.
func MarshalCAKey(caKey crypto.Signer) (pemBytes []byte, err error) {
	der, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return nil, fmt.Errorf("pki: marshal CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypePrivKey, Bytes: der}), nil
}

// ParseCAKey parses a plaintext PKCS#8 PEM CA private key back into a
// crypto.Signer (D18 §1.2). Round-trip source: ClusterSecrets.caKeyPEM.
func ParseCAKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != pemTypePrivKey {
		return nil, errors.New("pki: invalid CA key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("pki: CA key is not a crypto.Signer")
	}
	return signer, nil
}

// ParseCA reconstructs a usable CA from its public cert PEM + private key PEM
// (used when a full node loads ClusterSecrets at boot and must sign).
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCert {
		return nil, errors.New("pki: invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CA cert: %w", err)
	}
	key, err := ParseCAKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return &CA{
		Cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: cert.Raw}),
		key:     key,
	}, nil
}
