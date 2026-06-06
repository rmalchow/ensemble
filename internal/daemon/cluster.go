package daemon

// cluster.go is the daemon-side cluster-identity + TLS plumbing that closes the
// cmd-level integration gaps (genesis persistence + mTLS, doc 03 §8 / doc 01
// §3.1, §5.2). It owns:
//
//   - clusterIdentity: the live mTLS material (leaf cert+key, CA pool, CA signer)
//     a configured node serves with and signs adoption CSRs against;
//   - the on-disk persistence of certs/ + cluster.yaml + the genesis ConfigDoc
//     (the persistent state.Store the daemon loads at boot);
//   - the switchable tls.Config: an UNINITIALIZED node serves a self-signed
//     bootstrap leaf (ClientAuth NoClientCert surface, browser cert warning); on
//     genesis/adoption it SWITCHES to cluster mTLS WITHOUT a restart by publishing
//     the new identity into an atomic.Pointer the GetConfigForClient callback reads
//     (so the bound listener is never re-wrapped, doc 03 §8 last paragraph).
//
// daemon may import pki/state/auth (doc 01 §2 rule 5/6); web reaches none of this
// directly — it gets only the TLSConfig() function value and the Deps closures.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/config"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// leafValidity is the signed leaf lifetime (doc 03 §1.3 / A.12: 30 days).
const leafValidity = 30 * 24 * time.Hour

// clusterIdentity is the live mTLS material for a CONFIGURED node. It is built
// either at genesis (setup), at boot (loaded from certs/ + doc.json), or on
// adoption (the installed leaf+CA). It is published atomically so the TLS
// callback and the adoption signer read a consistent snapshot without a restart.
type clusterIdentity struct {
	leaf   tls.Certificate // this node's signed leaf cert + private key
	caPool *x509.CertPool  // cluster CA pool (verify peer certs)
	ca     *pki.CA         // full CA (cert+key) when this node holds the key; nil otherwise
	tlsCfg *tls.Config     // prebuilt mTLS server config (VerifyClientCertIfGiven)
}

// tlsState holds the switchable control-plane TLS material. selfSigned is the
// bootstrap config served before genesis (self-signed leaf, no client-cert
// requirement). cluster is the live mTLS identity, nil until genesis/adoption.
// Both are read by the GetConfigForClient callback, so the switch is atomic and
// needs no listener rebind (doc 03 §8).
type tlsState struct {
	selfSigned atomic.Pointer[tls.Config]
	cluster    atomic.Pointer[clusterIdentity]
	// browserCert is the ECDSA P-256 self-signed leaf (the same one in the
	// selfSigned bootstrap config). It is kept after genesis/adoption so the
	// cluster mTLS config can ALSO present it: the cluster leaf is Ed25519, which
	// browsers reject (SSL_ERROR_NO_CYPHER_OVERLAP), so a configured node serves
	// the ECDSA cert to browsers and the Ed25519 leaf to peer nodes — Go's TLS
	// stack auto-selects the cert the client's sigalgs support. nil if key-gen
	// failed at boot.
	browserCert atomic.Pointer[tls.Certificate]
}

// serverConfig is the GetConfigForClient callback: it returns the live cluster
// mTLS config once the node is configured, else the self-signed bootstrap config.
// A genesis/adoption that publishes a clusterIdentity therefore takes effect on
// the very next handshake with no rebind.
func (t *tlsState) serverConfig() *tls.Config {
	if ci := t.cluster.Load(); ci != nil {
		return ci.tlsCfg
	}
	return t.selfSigned.Load()
}

// loadOrCreateBrowserTLS returns the node's PERSISTENT self-signed ECDSA leaf
// for the browser/bootstrap control surface: loaded from certs/browser.{crt,key}
// when present and not near expiry, freshly minted (and persisted, best-effort)
// otherwise. Persistence keeps the cert — and therefore its fingerprint and the
// operator's browser exception — stable across restarts; without it every boot
// re-minted a new cert and the browser re-prompted. A node with no data dir
// (tests) just mints an ephemeral one.
func loadOrCreateBrowserTLS(paths config.Paths, nodeID string) (*tls.Config, tls.Certificate, []byte, error) {
	crtPath, keyPath := browserCertPaths(paths)
	if crtPath != "" {
		if cfg, cert, der, err := loadBrowserTLS(crtPath, keyPath); err == nil {
			return cfg, cert, der, nil
		}
	}
	cfg, cert, der, err := newSelfSignedTLS(nodeID)
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}
	if crtPath != "" {
		persistBrowserTLS(crtPath, keyPath, der, cert.PrivateKey)
	}
	return cfg, cert, der, nil
}

// browserCertPaths returns the on-disk locations of the persistent browser
// cert/key, or empty strings when the node has no certs dir (test Node).
func browserCertPaths(p config.Paths) (crt, key string) {
	if p.Certs == "" {
		return "", ""
	}
	return filepath.Join(p.Certs, "browser.crt"), filepath.Join(p.Certs, "browser.key")
}

// browserRenewWindow: a persisted browser cert within this window of expiry is
// re-minted at boot rather than served to (then rejected by) the browser.
const browserRenewWindow = 30 * 24 * time.Hour

// loadBrowserTLS reads + validates the persisted browser cert/key pair and
// rebuilds the bootstrap tls.Config over it. Any failure (missing, unparseable,
// near expiry) returns an error so the caller re-mints.
func loadBrowserTLS(crtPath, keyPath string) (*tls.Config, tls.Certificate, []byte, error) {
	certPEM, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}
	key, err := pki.ParseCAKey(keyPEM) // PKCS#8, any signer
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, tls.Certificate{}, nil, errors.New("daemon: bad browser cert PEM")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, tls.Certificate{}, nil, err
	}
	if time.Now().After(parsed.NotAfter.Add(-browserRenewWindow)) {
		return nil, tls.Certificate{}, nil, errors.New("daemon: browser cert near expiry")
	}
	cert := tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
	}
	return cfg, cert, block.Bytes, nil
}

// persistBrowserTLS writes the freshly-minted browser cert/key to certs/
// (cert 0644 — public; key 0600 — secret). Best-effort: a write failure only
// costs the operator a re-prompt at the next restart, never startup.
func persistBrowserTLS(crtPath, keyPath string, der []byte, key crypto.PrivateKey) {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return
	}
	keyPEM, err := pki.MarshalCAKey(signer)
	if err != nil {
		return
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = writeFileMode(keyPath, keyPEM, 0o600)
	_ = writeFileMode(crtPath, certPEM, 0o644)
}

// newSelfSignedTLS mints an ephemeral self-signed ECDSA P-256 leaf for the
// bootstrap control surface (doc 03 §2.1/§8: the only non-mTLS TLS endpoint).
// The key is ECDSA (not the cluster's Ed25519) on purpose: browsers (Firefox,
// older Chrome/Safari) do NOT accept Ed25519 leaf certificates in TLS and fail
// the handshake with SSL_ERROR_NO_CYPHER_OVERLAP, but this cert is exactly what
// a browser hits when loading the setup wizard. P-256 is universally supported.
// The cluster mTLS plane (node↔node) keeps its Ed25519 identity — that path
// never touches a browser. ClientAuth is NoClientCert — the adoptee has no
// client cert yet, and a browser hitting the wizard must not be asked for one.
// The cert SANs cover loopback + any local IPs so an operator/controller
// reaching the node by IP gets a usable (if untrusted) cert; trust is
// established out-of-band by fingerprint pin (doc 03 §2.2).
func newSelfSignedTLS(nodeID string) (*tls.Config, tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("daemon: self-signed key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("daemon: self-signed serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "ensemble-bootstrap/" + nodeID},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  localIPs(),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, tls.Certificate{}, nil, fmt.Errorf("daemon: self-sign bootstrap cert: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
	}
	return cfg, cert, der, nil
}

// localIPs returns loopback + the host's non-loopback unicast IPs for the
// self-signed bootstrap cert SANs (best-effort; loopback is always included).
func localIPs() []net.IP {
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips
}

// nonLoopbackIPStrings returns the host's routable unicast IPs as strings — the
// founder's NodeRecord.Addrs seed (genesis has no adoption to observe its
// addrs). Loopback and link-local addresses are skipped: fe80::/10 is one per
// interface (a wall of noise in the UI) and useless as a peer-reachable
// allowlist entry. Best-effort; empty on a host with no usable interface.
func nonLoopbackIPStrings() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	return out
}

// buildClusterIdentity assembles the live mTLS identity from a node's leaf
// cert+key, the cluster CA cert PEM, and the live revoked-set predicate. The
// resulting tls.Config uses VerifyClientCertIfGiven (doc 03 §8 / README §4): a
// browser (no client cert) still reaches the session/login path, while a peer
// node's cert is verified against the cluster CA + the revoked set. ca may be nil
// (a node that holds only a leaf, e.g. an adoptee) — adoption signing then has no
// local CA and the daemon proxies it (not needed for the genesis founder).
func buildClusterIdentity(leaf tls.Certificate, caCertPEM []byte, ca *pki.CA, revoked func(fp string) bool, browserCert *tls.Certificate) (*clusterIdentity, error) {
	caPool, err := pki.CAPoolFromPEM(caCertPEM)
	if err != nil {
		return nil, err
	}
	verifier := pki.NewPeerVerifier(revoked, nil)
	// ServerTLS requires+verifies a client cert; relax to VerifyClientCertIfGiven
	// so the human/browser path (no client cert) still reaches session/login while
	// node certs are verified when presented (doc 03 §8, README §4).
	cfg := pki.ServerTLS(leaf, caPool, verifier)
	cfg.ClientAuth = tls.VerifyClientCertIfGiven
	// Present the ECDSA browser cert ALONGSIDE the Ed25519 cluster leaf. Go's TLS
	// stack picks the cert whose key type the client's signature_algorithms
	// support: a browser (no Ed25519) gets the ECDSA cert; a peer node gets the
	// Ed25519 leaf. Without this, a configured node would fail every browser
	// handshake with SSL_ERROR_NO_CYPHER_OVERLAP. The cluster leaf stays first so
	// peer mTLS continues to present the cluster identity.
	if browserCert != nil {
		cfg.Certificates = append(cfg.Certificates, *browserCert)
	}
	// With VerifyClientCertIfGiven the callback still fires when NO cert is
	// presented (rawCerts empty — the browser path). Let that through and only run
	// the revoked-set/CA verifier when a cert IS presented (a peer node).
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, chains [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return nil
		}
		return verifier.Verify(rawCerts, chains)
	}
	return &clusterIdentity{
		leaf:   leaf,
		caPool: caPool,
		ca:     ca,
		tlsCfg: cfg,
	}, nil
}

// --- on-disk persistence (certs/ + cluster.yaml) -----------------------------

// writeCerts writes the genesis/adoption cert set to certs/ with the doc 01 §5.2
// modes: ca.crt 0644 (public), node.crt 0644, node.key 0600 (secret). The Certs
// dir is created 0700 by config.OpenDataDir; this only writes the leaves.
func writeCerts(paths config.Paths, caCertPEM, leafCertPEM []byte, leafKey crypto.Signer) error {
	if paths.Certs == "" {
		return errors.New("daemon: no certs dir configured")
	}
	keyPEM, err := pki.MarshalCAKey(leafKey) // PKCS#8 PEM (works for any signer)
	if err != nil {
		return fmt.Errorf("daemon: marshal node key: %w", err)
	}
	if err := writeFileMode(paths.CACert, caCertPEM, 0o644); err != nil {
		return err
	}
	if err := writeFileMode(paths.NodeKey, keyPEM, 0o600); err != nil {
		return err
	}
	return writeFileMode(paths.NodeCert, leafCertPEM, 0o644)
}

// writeFileMode writes data to path atomically (temp+rename) at exactly mode
// (defeating umask), creating it 0600 first when the target is secret-bearing.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("daemon: write %s: %w", path, err)
	}
	_ = os.Chmod(tmp, mode) // defeat umask so the mode is exact
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("daemon: rename %s: %w", path, err)
	}
	return os.Chmod(path, mode)
}

// clusterMarker is the cluster.yaml body (doc 01 §5.2: "membership marker — group
// + activation"). It is intentionally tiny: its mere PRESENCE flips configured(),
// and it records the activation group + cluster name + CA fingerprint for the
// operator/diagnostics. The authoritative config is doc.json; cluster.yaml is the
// boot-time "am I configured?" flag (0600 because it sits next to the secrets).
type clusterMarker struct {
	ClusterName   string `yaml:"clusterName"`
	Group         string `yaml:"group"`
	CAFingerprint string `yaml:"caFingerprint"`
	Configured    bool   `yaml:"configured"`
	CreatedAt     string `yaml:"createdAt"`
}

// writeClusterMarker writes cluster.yaml (0600) marking the node configured. It is
// hand-rolled minimal YAML (no yaml dependency: the daemon must not add deps) —
// the file is flat scalars, so plain key: value lines are valid YAML.
func writeClusterMarker(paths config.Paths, m clusterMarker) error {
	body := fmt.Sprintf(
		"clusterName: %q\ngroup: %q\ncaFingerprint: %q\nconfigured: %t\ncreatedAt: %q\n",
		m.ClusterName, m.Group, m.CAFingerprint, m.Configured, m.CreatedAt,
	)
	return writeFileMode(paths.Cluster, []byte(body), 0o600)
}

// --- boot-time load -----------------------------------------------------------

// loadClusterIdentity reconstructs the live mTLS identity from disk at boot when
// the node is already configured (certs/ + doc.json + cluster.yaml present). It
// pairs the on-disk leaf cert+key with the CA cert (and the CA key from the
// ConfigDoc secrets, when this node holds it) so a restart restores mTLS + the
// adoption signer WITHOUT re-running setup. revoked reads the live store.
func loadClusterIdentity(paths config.Paths, doc state.ConfigDoc, revoked func(fp string) bool, browserCert *tls.Certificate) (*clusterIdentity, error) {
	certPEM, err := os.ReadFile(paths.NodeCert)
	if err != nil {
		return nil, fmt.Errorf("daemon: read node cert: %w", err)
	}
	keyPEM, err := os.ReadFile(paths.NodeKey)
	if err != nil {
		return nil, fmt.Errorf("daemon: read node key: %w", err)
	}
	leafKey, err := pki.ParseCAKey(keyPEM) // PKCS#8 parse, any signer
	if err != nil {
		return nil, fmt.Errorf("daemon: parse node key: %w", err)
	}
	leaf, err := pki.LeafFromPEM(certPEM, leafKey)
	if err != nil {
		return nil, err
	}

	caCertPEM := []byte(doc.Cluster.CACertPEM)
	if len(caCertPEM) == 0 {
		// Fall back to the on-disk ca.crt if the doc has not loaded a CA (defensive).
		if b, rerr := os.ReadFile(paths.CACert); rerr == nil {
			caCertPEM = b
		}
	}

	// Reconstruct the full CA when this node holds the private key (genesis founder
	// / any full node), so it can sign adoption CSRs after a restart.
	var ca *pki.CA
	if doc.Secrets.CAKeyPEM != "" {
		if full, cerr := pki.ParseCA(caCertPEM, []byte(doc.Secrets.CAKeyPEM)); cerr == nil {
			ca = full
		}
	}
	return buildClusterIdentity(leaf, caCertPEM, ca, revoked, browserCert)
}

// gossipKeyB64 encodes a raw gossip key as base64 for ClusterSecrets.SharedSecret
// transport (the field doubles as the replicated gossip key carrier in this build;
// adoption decodes it back into the memberlist SecretKey). A nil key yields "".
func gossipKeyB64(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(key)
}

// decodeGossipKey reverses gossipKeyB64. "" => nil.
func decodeGossipKey(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// fingerprintOfPEM returns the SHA-256 fingerprint of the first cert in a PEM, or
// "" if none. Used for the cluster.yaml marker / status.
func fingerprintOfPEM(certPEM []byte) string {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return ""
	}
	return pki.Fingerprint(block.Bytes)
}
