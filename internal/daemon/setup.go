package daemon

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// This file wires the P1.3 §5.3 genesis act (POST /api/v1/setup) and the auth/
// status read closures it feeds, into the daemon. web reaches all of this only
// through the web.Deps function values built in deps.go (01 §2 rule 1). Genesis
// is now fully persistent (GAP 2): it routes the ConfigDoc through the daemon's
// shared persistent store (doc.json, 0600), captures ClusterSecrets (CA key +
// gossip key) so adoption can sign, writes certs/ to disk, marks the node
// configured (cluster.yaml), and switches the control plane from self-signed TLS
// to cluster mTLS in-process (GAP 3) — so a restart restores everything without
// re-running setup.

// setup performs the genesis act (P1.3 §5.3), in order: argon2id-hash the admin
// password; mint the cluster CA (capturing its key + a gossip key into
// ClusterSecrets); self-issue this node's leaf; write the genesis ConfigDoc v1
// into the daemon's PERSISTENT store; write certs/ (ca.crt/node.key/node.crt);
// switch the served TLS config to cluster mTLS; record the live genesisState; and
// activate the session in-process (which also persists cluster.yaml). It is the
// cmd-built web.Deps.Setup closure (here, because the daemon is the layer that may
// import pki/state/auth, 01 §2 rule 6). Returns the founding identity + version.
func (n *Node) setup(clusterName, adminPassword, nodeName string) (web.SetupResult, error) {
	n.genesisMu.Lock()
	defer n.genesisMu.Unlock()

	// Genesis is once-only: a second setup on an initialised node is a 409 in the
	// handler, but guard here too against a racing caller.
	if n.genesis != nil {
		return web.SetupResult{}, errors.New("daemon: node already initialised")
	}

	now := time.Now().UTC()

	// 1) argon2id-hash the admin password (03 §7.1; A.12 cost params).
	argon := auth.DefaultArgon2id()
	adminHash, err := auth.HashPassword(adminPassword, argon)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: hash admin password: %w", err)
	}

	// 2) Mint the cluster CA (Ed25519, CN=ensemble-ca/<clusterName>, IsCA, 10y) and
	// capture its private key + a fresh gossip key into ClusterSecrets so adoption
	// can sign adoptee CSRs and rekey gossip (GAP 2; D18 §1.2 plaintext at rest).
	ca, err := pki.CreateCA(clusterName, now)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: create CA: %w", err)
	}
	caKeyPEM, err := ca.KeyPEM()
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: export CA key: %w", err)
	}
	gossipKey := pki.NewGossipKey()

	// 3) Self-issue this node's leaf: fresh key → CSR → CA.Sign (30d, 03 §1.3).
	leafKey, err := pki.NewIdentity()
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: new node key: %w", err)
	}
	csrPEM, err := pki.NewCSR(leafKey, n.options.NodeID)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: build CSR: %w", err)
	}
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: parse CSR: %w", err)
	}
	leafPEM, err := ca.Sign(csr, n.options.NodeID, localIPs(), leafValidity, now)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: sign leaf: %w", err)
	}

	caFinger := pki.Fingerprint(ca.Cert.Raw)
	createdRFC := now.Format(time.RFC3339)

	// 4) Write the genesis ConfigDoc (Version=1) into the daemon's PERSISTENT store
	// (doc.json, 0600). The store's Apply path bumps a Version=0 seed to 1; a fresh
	// node's store is at version 0, so this single Apply produces version 1.
	store := n.store
	genesisDoc := state.ConfigDoc{
		Version: store.Get().Version, // 0 on a fresh node; Apply -> +1 (07 §2.1)
		Cluster: state.ClusterInfo{
			Name:        clusterName,
			CACertPEM:   string(ca.CertPEM()),
			Created:     createdRFC,
			Fingerprint: caFinger,
		},
		Secrets: state.ClusterSecrets{
			CAKeyPEM:     string(caKeyPEM),        // private CA key (so this node can sign adoptions)
			SharedSecret: gossipKeyB64(gossipKey), // base64 gossip key (replicated to adoptees)
		},
		Auth: state.AuthConfig{
			AdminHash: adminHash,
			Argon:     argon,
		},
		Nodes: []state.NodeRecord{{
			ID:   n.options.NodeID,
			Name: firstNonEmpty(nodeName, n.options.Name, n.options.NodeID),
			// The founder's own non-loopback addrs: the durable half of the
			// allowlist derivation (07 §3.1) and the UI members row. Live gossip
			// addrs refresh on top, so a later IP change is tolerated.
			Addrs:   nonLoopbackIPStrings(),
			CertPEM: string(leafPEM),
			Channel: "stereo",
			// Render-capable by default so a freshly-founded node plays (the P6
			// sink probe refines this to the actually-usable backends, 06 §1.5).
			Caps: state.Capabilities{Render: true},
		}},
		Groups: []state.GroupRecord{{
			ID:            "default",
			Name:          "Default",
			MemberNodeIDs: []string{n.options.NodeID},
		}},
		UpdatedBy: n.options.NodeID,
		UpdatedAt: createdRFC,
	}
	applied, err := store.Apply(genesisDoc)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: write genesis ConfigDoc: %w", err)
	}

	// 5) Persist certs/ to disk (ca.crt 0644, node.key 0600, node.crt 0644) and keep
	// the leaf key + cert in memory for the TLS listener (GAP 2/3).
	if err := writeCerts(n.options.Paths, ca.CertPEM(), leafPEM, leafKey); err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: write certs: %w", err)
	}

	// 6) Switch the control plane from self-signed TLS to cluster mTLS WITHOUT a
	// restart (GAP 3): build the live identity (this node's leaf + CA pool + CA key
	// + the live revoked-set verifier) and publish it so the GetConfigForClient
	// callback serves mTLS on the next handshake.
	leaf, err := pki.LeafFromPEM(leafPEM, leafKey)
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: assemble leaf: %w", err)
	}
	ci, err := buildClusterIdentity(leaf, ca.CertPEM(), ca, n.revokedPredicate(), n.tls.browserCert.Load())
	if err != nil {
		return web.SetupResult{}, fmt.Errorf("daemon: build cluster TLS: %w", err)
	}
	n.leafKey = leafKey
	n.tls.cluster.Store(ci)

	// 7) Record the live genesis identity so Initialized()/StatusView()/login flip.
	n.genesis = &genesisState{
		store:       store,
		caFinger:    caFinger,
		clusterName: clusterName,
		createdRFC:  createdRFC,
	}
	logf(n.options.Log, "setup: genesis complete cluster=%q ca=%s version=%d node=%s (mTLS active)",
		clusterName, shortID(caFinger), applied.Version, shortID(n.options.NodeID))

	// 8) Activate the session in-process (no restart, 01 §4.4). Best-effort: a
	// failure is logged but does not roll back genesis (the node is initialised;
	// the realtime planes are still nil-stub in this skeleton).
	if n.activateHook != nil {
		if err := n.activateHook(); err != nil {
			logf(n.options.Log, "setup: activate after genesis failed (continuing): %v", err)
		}
	}

	// 9) Persist the cluster.yaml marker (0600) so configured() is true at the next
	// boot WITHOUT re-running setup (GAP 2 restart-survival). Written inline here
	// (not via persistHook) because we still hold genesisMu and persistCluster
	// re-acquires it — and we have the marker fields right here. Best-effort: the
	// in-memory node is already initialised; the marker is the boot hint.
	if n.options.Paths.Cluster != "" {
		if werr := writeClusterMarker(n.options.Paths, clusterMarker{
			ClusterName:   clusterName,
			Group:         "default",
			CAFingerprint: caFinger,
			Configured:    true,
			CreatedAt:     createdRFC,
		}); werr != nil {
			logf(n.options.Log, "setup: persist cluster.yaml failed (node still active): %v", werr)
		}
	}

	// Re-announce over mDNS with the new cluster identity (cf + init=1).
	n.registerMDNS()

	return web.SetupResult{
		ClusterName:   clusterName,
		CAFingerprint: caFinger,
		Created:       createdRFC,
		NodeID:        n.options.NodeID,
		NodeName:      firstNonEmpty(nodeName, n.options.Name, n.options.NodeID),
		Version:       applied.Version,
	}, nil
}

// initialized reports whether genesis has run (in-memory) OR the node was already
// configured on disk. It backs the web [1] gate + the setup 409 guard (P1.3).
func (n *Node) initialized() bool {
	n.genesisMu.Lock()
	g := n.genesis
	n.genesisMu.Unlock()
	if g != nil {
		return true
	}
	return n.configured()
}

// verifyAdminPassword constant-time-checks pw against the genesis admin hash
// (B.2 login). False when genesis has not run (no credential to check against).
func (n *Node) verifyAdminPassword(pw string) bool {
	n.genesisMu.Lock()
	g := n.genesis
	n.genesisMu.Unlock()
	if g == nil {
		return false
	}
	doc := g.store.Get()
	if doc.Auth.AdminHash == "" {
		return false
	}
	return auth.VerifyPassword(pw, doc.Auth.AdminHash)
}

// configVersion returns the current genesis ConfigDoc version (for ETag/session
// responses, B.4). 0 before genesis.
func (n *Node) configVersion() uint64 {
	n.genesisMu.Lock()
	g := n.genesis
	n.genesisMu.Unlock()
	if g == nil {
		return 0
	}
	return g.store.Get().Version
}

// clusterInfo is the GET /api/v1/cluster/info projection: the cluster's public
// identity (name, CA fingerprint, created, node count) + the ConfigDoc version.
// It reads the persistent store directly so it works both after an in-session
// genesis and after a restart of a configured node. ClusterSecrets are never
// touched (only doc.Cluster is read), so the CA key cannot leak. Zero value on an
// uninitialised node (empty cluster name) — the handler still answers so the SPA
// renders.
func (n *Node) clusterInfo() web.ClusterInfoView {
	doc := n.store.Get()
	finger := doc.Cluster.Fingerprint
	if finger != "" {
		finger = "sha256:" + finger
	}
	return web.ClusterInfoView{
		ClusterName:   doc.Cluster.Name,
		CAFingerprint: finger,
		Created:       doc.Cluster.Created,
		NodeCount:     len(doc.Nodes),
		Version:       doc.Version,
	}
}

// statusView is the GET /api/v1/status (08 §G.1) live projection. Initialized
// steers the SPA wizard-vs-app; the rest is best-effort runtime telemetry.
func (n *Node) statusView() web.StatusView {
	var v web.StatusView
	v.NodeID = n.options.NodeID
	v.Online = true
	v.UptimeSec = int64(time.Since(n.startedAt).Seconds())
	v.Initialized = n.initialized()
	v.ConfigVersion = n.configVersion()
	return v
}

// parseCSR decodes a PEM CSR into the x509 request CA.Sign verifies + signs.
func parseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, errors.New("daemon: CSR PEM decode failed")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
