// Package pki is the trust origin of the cluster's control plane (doc 03 §0.2).
//
// It mints and operates the Ed25519 cluster CA (create, self-sign, and leaf
// signing), generates per-node keypairs and CSRs, marshals/parses the
// plaintext-replicated CA private key (ClusterSecrets.caKeyPEM, D18), mints the
// random cluster gossip key, builds the TLS 1.3 mTLS *tls.Config for both the
// server and client control-plane roles, and supplies the PeerVerifier that
// enforces the grow-only RevokedSet (SHA-256-fingerprint) revocation check on
// every handshake (doc 03 §5.4).
//
// Trust model recap (doc 03 §0/§1):
//
//   - The control plane is the trust origin: a node holds a CA-signed leaf or it
//     is not a peer. Adoption is the act of getting a leaf signed; forget is the
//     act of making a leaf untrusted (the RevokedSet).
//   - The CA private key is replicated to full nodes in PLAINTEXT (Model B / D18):
//     no sealing, no AEAD at rest. Any full node can sign, so there is no adoption
//     single point of failure. The residual risk (disk read on any full node
//     yields the CA key) is accepted for a single trusted LAN.
//   - Keys are Ed25519 (small, fast verify, no parameter foot-guns); TLS is 1.3
//     only; the gossip key is 32 random bytes (NOT derived, NOT sealed).
//
// pki is a pure leaf library: it does no I/O, owns no persistence, holds no
// network sockets, and imports no other internal/* package. Callers hand it
// bytes and live ConfigDoc snapshots (via the revoked closure) and consume its
// artifacts. See doc 03 §1/§5/§8 and Appendix A.11/A.12.
package pki
