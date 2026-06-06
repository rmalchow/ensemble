package pki

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// ServerTLS builds the mTLS server config for the control plane (doc 03 §8).
// TLS 1.3 only; the node presents its leaf and REQUIRES + verifies the client's
// cert against caPool, plus the PeerVerifier revoked-set check (§5.4).
func ServerTLS(leaf tls.Certificate, caPool *x509.CertPool, v *PeerVerifier) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{leaf},
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             caPool,
		VerifyPeerCertificate: v.Verify,
	}
}

// ClientTLS builds the mTLS client config for the control plane (doc 03 §8).
// TLS 1.3 only; the node presents its leaf (ExtKeyUsage Server+Client) and
// verifies the server against caPool, plus the PeerVerifier revoked-set check
// (§5.4).
//
// The caller MUST set the returned config's ServerName (per dial) to the target
// nodeId/IP so SAN matching holds (doc 03 §8 / §1.3). pki exposes the config but
// does not know the dial target, so it leaves ServerName empty.
func ClientTLS(leaf tls.Certificate, caPool *x509.CertPool, v *PeerVerifier) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{leaf},
		RootCAs:               caPool,
		VerifyPeerCertificate: v.Verify,
	}
}

// CAPoolFromPEM builds a *x509.CertPool from ConfigDoc.Cluster's CA cert PEM.
func CAPoolFromPEM(caCertPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, errors.New("pki: no valid CA certificate in PEM")
	}
	return pool, nil
}

// LeafFromPEM assembles a tls.Certificate from a node's signed cert PEM + its
// private key (held only in memory).
func LeafFromPEM(certPEM []byte, key crypto.Signer) (tls.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemTypeCert {
		return tls.Certificate{}, errors.New("pki: invalid leaf cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("pki: parse leaf cert: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{block.Bytes},
		PrivateKey:  key,
		Leaf:        cert,
	}, nil
}
