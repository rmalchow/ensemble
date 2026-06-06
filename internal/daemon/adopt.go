package daemon

// adopt.go is the daemon-side glue binding the transport-agnostic internal/adopt
// engine to this node's concrete pki (CA signing), state (ConfigDoc write under
// If-Match + grow-only RevokedSet) and TLS material. It is the cmd-level
// integration that was missing: a configured node can now adopt a second node
// (controller half) and an uninitialized node can be adopted (adoptee/bootstrap
// half), all over the daemon's own listener (doc 01 §2 rule 6: daemon may import
// adopt/pki/state; web reaches it only via the Deps closures).
//
// Controller half (this node holds the CA): adoptDep pins the target's self-signed
// cert by fingerprint, drives the three /bootstrap/adopt phases, signs the CSR with
// the cluster CA, and records the new NodeRecord into the persistent ConfigDoc.
// Adoptee half: bootstrapDeps exposes the adopt.Node + Install hook that persists
// the verified leaf+CA+secrets and switches this node into mTLS in-process.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/adopt"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// bootstrapPort is the default control/bootstrap port: the bootstrap surface
// shares the control listener (the self-signed-vs-mTLS split is by request via
// GetConfigForClient, not by port). A.12 control mTLS port = 8443.
const bootstrapPort = "8443"

// --- controller half ---------------------------------------------------------

// adoptDep is the Deps.Adopt closure (08 §C.3/§C.4). It requires this node to hold
// the cluster CA (i.e. be configured); an unconfigured node cannot adopt and
// surfaces ErrUnreachable-class not-ready. It uses the default per-call pinning
// client (no test injection) — see adoptUsing for the testable core. password is
// the C.4 takeover release credential (the TARGET cluster's admin password);
// empty for a plain C.3 adopt.
func (n *Node) adoptDep(addr, fingerprint, pin, nodeID, name, password string, force bool) error {
	return n.adoptUsing("", nil, addr, fingerprint, pin, nodeID, name, password, force)
}

// adoptUsing is adoptDep with an optional base-URL override + http client (tests
// pass an httptest URL + its self-signed-trusting client). An empty baseURL
// derives https://<addr>; a nil client builds the default fingerprint-pinning one.
func (n *Node) adoptUsing(baseURL string, client *http.Client, addr, fingerprint, pin, nodeID, name, password string, force bool) error {
	ci := n.tls.cluster.Load()
	if ci == nil || ci.ca == nil {
		return fmt.Errorf("%w: node has no cluster CA (not configured)", web.ErrUnreachable)
	}
	doc := n.store.Get()
	// Version-ordering invariant: the adoptee seeds its bootstrap doc at v1 on
	// install. The 07 §2.1 LWW merge is lineage-blind, so on a FRESH cluster
	// (genesis doc also v1) the equal-version id tiebreak could let that sparse
	// bootstrap doc replace the authoritative one at the first gossip exchange.
	// Reserve a version ahead of it before the handshake so the controller's doc
	// always outranks the adoptee's.
	if doc.Version < 2 {
		if bumped, err := n.store.Apply(doc); err == nil {
			doc = bumped
		} else {
			doc = n.store.Get()
		}
	}

	runner, observed, err := dialTarget(client, addr, fingerprint)
	if err != nil {
		return err
	}
	if baseURL != "" {
		runner.baseURL = baseURL
	}
	nodeState, err := runner.info()
	if err != nil {
		return fmt.Errorf("%w: %v", web.ErrUnreachable, err)
	}

	// C.4 takeover: a member target must first be RELEASED with its current
	// cluster's admin password (03 §4 — the target operator's credential
	// authorizes the move). On success its bootstrap reopens uninitialized and
	// the normal A.9 adopt below proceeds (fresh nodes answer the default PIN).
	if force && nodeState == "member" {
		if password == "" {
			return fmt.Errorf("%w: takeover requires the target cluster's admin password", web.ErrUnreachable)
		}
		if err := runner.takeover(password); err != nil {
			return err
		}
		nodeState, err = runner.info()
		if err != nil {
			return fmt.Errorf("%w: %v", web.ErrUnreachable, err)
		}
		if pin == "" {
			pin = auth.DefaultPIN // a just-released node answers the default PIN
		}
	}

	ctrl := &adopt.Controller{
		Sign:        n.signFunc(ci.ca),
		CABundle:    ci.ca.CertPEM(),
		Secrets:     adopt.ClusterSecrets{CAKeyPEM: []byte(doc.Secrets.CAKeyPEM), GossipKey: decodeGossipKey(doc.Secrets.SharedSecret)},
		ClusterName: doc.Cluster.Name,
	}
	seed, err := ctrl.Run(context.Background(), runner, pin, nodeID, name, nodeState, observed, force)
	if err != nil {
		return mapEngineErr(err)
	}
	return n.recordNode(seed)
}

// signFunc adapts the cluster CA into adopt.SignFunc: parse the CSR PEM, sign with
// SANs from the AUTHENTICATED nodeID + observed addrs (never the CSR's own).
func (n *Node) signFunc(ca *pki.CA) adopt.SignFunc {
	return func(csrPEM []byte, nodeID string, addrs []net.IP) ([]byte, error) {
		block, rest := pem.Decode(csrPEM)
		if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
			return nil, errors.New("daemon: bad CSR PEM")
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("daemon: parse CSR: %w", err)
		}
		return ca.Sign(csr, nodeID, addrs, leafValidity, time.Now())
	}
}

// recordNode writes the adopted node's NodeRecord into the ConfigDoc under
// optimistic concurrency, retrying on a mid-handshake gossip version bump (the
// node is already signed at this point, so we re-read + re-apply rather than fail).
func (n *Node) recordNode(seed adopt.NodeRecordSeed) error {
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		doc := n.store.Get()
		rec := state.NodeRecord{
			ID: seed.ID,
			// An adoption without an operator-chosen name falls back to the short
			// id so the members table never shows a blank row.
			Name:    firstNonEmpty(seed.Name, shortID(seed.ID)),
			CertPEM: string(seed.CertPEM),
			Addrs:   ipsToStrings(seed.Addrs),
			Channel: "stereo",
			// Render-capable by default so an adoptee plays out of the box (the P6
			// sink probe refines this, 06 §1.5). A re-adoption keeps the old Caps
			// (copied below).
			Caps: state.Capabilities{Render: true},
		}
		replaced := false
		for j := range doc.Nodes {
			if doc.Nodes[j].ID == seed.ID {
				rec.HWDelayUs = doc.Nodes[j].HWDelayUs
				rec.Channel = doc.Nodes[j].Channel
				rec.GainDB = doc.Nodes[j].GainDB
				rec.Device = doc.Nodes[j].Device
				rec.Caps = doc.Nodes[j].Caps
				doc.Nodes[j] = rec
				replaced = true
				break
			}
		}
		if !replaced {
			doc.Nodes = append(doc.Nodes, rec)
			// Add the new node to the default group so a freshly-adopted node can join
			// playback without a separate group edit (matches the genesis default group).
			for g := range doc.Groups {
				if doc.Groups[g].ID == "default" {
					doc.Groups[g].MemberNodeIDs = append(doc.Groups[g].MemberNodeIDs, seed.ID)
				}
			}
		}
		if _, err := n.store.Apply(doc); err != nil {
			if errors.Is(err, state.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return web.ErrVersionConflict
}

// forgetNodeDep is the Deps.Forget closure (08 §C.5): add the leaf's fingerprint to
// the grow-only RevokedSet, drop the NodeRecord, pull it from every group. (Gossip
// rekey is the membership plane's job, wired with P2 cluster; the revoke+drop is
// the authoritative config change that closes the door regardless.)
func (n *Node) forgetNodeDep(nodeID string) error {
	doc := n.store.Get()
	var rec *state.NodeRecord
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == nodeID {
			rec = &doc.Nodes[i]
			break
		}
	}
	if rec == nil {
		return web.ErrNotFound
	}
	if len(doc.Nodes) <= 1 {
		return web.ErrLastNode
	}
	if fp := certFingerprint(rec.CertPEM); fp != "" {
		doc.Revoked.Entries = append(doc.Revoked.Entries, state.RevokedCert{
			Fingerprint: fp,
			NodeID:      nodeID,
			Reason:      "forget",
			At:          time.Now().UTC().Format(time.RFC3339),
		})
	}
	doc.Nodes = dropNode(doc.Nodes, nodeID)
	for i := range doc.Groups {
		doc.Groups[i].MemberNodeIDs = dropString(doc.Groups[i].MemberNodeIDs, nodeID)
	}
	if _, err := n.store.Apply(doc); err != nil {
		if errors.Is(err, state.ErrConflict) {
			return web.ErrVersionConflict
		}
		return err
	}
	return nil
}

// --- adoptee (bootstrap) half ------------------------------------------------

// bootstrapDeps builds the web.BootstrapDeps seam: the adopt.Node adoptee half over
// this node's leaf key + PIN + the shared A.12 guard, the CSR builder, the Install
// hook, and the Info projection. The leaf key is minted lazily (and persisted) the
// first time the bootstrap surface needs a CSR, so an uninitialized node has a
// stable key across the handshake's csr/complete phases.
func (n *Node) bootstrapDeps() *web.BootstrapDeps {
	pin := auth.DefaultPIN // D9: per-cluster PIN override lands with auth; default "0000"
	node := adopt.NewNode(n.options.NodeID, pin, n.bootstrapLeafKey(), n.bootGuard)
	return &web.BootstrapDeps{
		Node:    node,
		Guard:   n.bootGuard,
		CSR:     func() ([]byte, error) { return pki.NewCSR(n.bootstrapLeafKey(), n.options.NodeID) },
		Install: n.installAdoption,
		Info:    n.bootstrapInfo,
		// Takeover release (03 §4): this node's CURRENT admin password authorizes
		// the wipe-and-reopen; the same A.12 guard throttles attempts.
		VerifyPassword: n.verifyAdminPassword,
		Release:        n.forget,
	}
}

// bootstrapLeafKey returns this node's leaf key, minting one if none exists yet
// (an unconfigured node). The same key is reused across the handshake and becomes
// the node's mTLS key on a successful install. Best-effort: a key-gen failure is
// extremely unlikely (crypto/rand) and would surface as a CSR build error.
func (n *Node) bootstrapLeafKey() crypto.Signer {
	n.genesisMu.Lock()
	defer n.genesisMu.Unlock()
	if n.leafKey == nil {
		if k, err := pki.NewIdentity(); err == nil {
			n.leafKey = k
		}
	}
	return n.leafKey
}

// installAdoption is the BootstrapDeps.Install hook: it atomically persists the
// verified adoption result (leaf+CA+secrets) and switches this node into mTLS, so
// a freshly-adopted node behaves exactly like a genesis founder on the next
// handshake (and after a restart). It runs after the adopt engine authenticates +
// decrypts; an error leaves the node uninitialized (takeover atomicity, 03 §4).
func (n *Node) installAdoption(inst adopt.Installed) error {
	leafKey := n.bootstrapLeafKey()
	if leafKey == nil {
		return errors.New("daemon: no leaf key for adoption install")
	}

	caCertPEM := inst.CABundlePEM
	caFinger := fingerprintOfPEM(caCertPEM)
	now := time.Now().UTC().Format(time.RFC3339)

	// 1) Persist certs/ (ca.crt 0644, node.key 0600, node.crt 0644).
	if err := writeCerts(n.options.Paths, caCertPEM, inst.LeafPEM, leafKey); err != nil {
		return err
	}

	// 2) Seed the persistent ConfigDoc with the cluster identity + secrets received
	// out-of-band, so this node has a usable doc before gossip converges. The
	// gossip merge (P2 cluster) then reconciles the controller's higher version.
	doc := n.store.Get()
	doc.Cluster = state.ClusterInfo{
		Name:        inst.ClusterName,
		CACertPEM:   string(caCertPEM),
		Fingerprint: caFinger,
		Created:     now,
	}
	doc.Secrets = state.ClusterSecrets{
		CAKeyPEM:     string(inst.Secrets.CAKeyPEM),
		SharedSecret: gossipKeyB64(inst.Secrets.GossipKey),
	}
	// Record self if absent (the controller's write will also add us; LWW merges).
	if indexNode(doc.Nodes, inst.NodeID) < 0 {
		doc.Nodes = append(doc.Nodes, state.NodeRecord{
			ID:      inst.NodeID,
			Name:    firstNonEmpty(n.options.Name, inst.NodeID),
			CertPEM: string(inst.LeafPEM),
			Channel: "stereo",
			Caps:    state.Capabilities{Render: true}, // default-capable; P6 probe refines
		})
	}
	if _, err := n.store.Apply(doc); err != nil && !errors.Is(err, state.ErrConflict) {
		return fmt.Errorf("daemon: persist adopted config: %w", err)
	}

	// 3) Switch to cluster mTLS in-process (GAP 3). Build the live identity from the
	// installed leaf + CA. The adoptee usually does NOT hold the CA key for SIGNING,
	// but it may receive it (full-node replication, D18) — buildClusterIdentity uses
	// it only when present.
	leaf, err := pki.LeafFromPEM(inst.LeafPEM, leafKey)
	if err != nil {
		return err
	}
	var ca *pki.CA
	if len(inst.Secrets.CAKeyPEM) > 0 {
		if full, cerr := pki.ParseCA(caCertPEM, inst.Secrets.CAKeyPEM); cerr == nil {
			ca = full
		}
	}
	ci, err := buildClusterIdentity(leaf, caCertPEM, ca, n.revokedPredicate(), n.tls.browserCert.Load())
	if err != nil {
		return err
	}
	n.tls.cluster.Store(ci)

	// 4) Record genesis state so initialized()/StatusView()/login flip, and persist
	// cluster.yaml so configured() survives a restart.
	n.genesisMu.Lock()
	n.genesis = &genesisState{store: n.store, caFinger: caFinger, clusterName: inst.ClusterName, createdRFC: now}
	n.genesisMu.Unlock()
	if err := writeClusterMarker(n.options.Paths, clusterMarker{
		ClusterName: inst.ClusterName, Group: "default", CAFingerprint: caFinger, Configured: true, CreatedAt: now,
	}); err != nil {
		logf(n.options.Log, "adoption: persist cluster.yaml failed (node still adopted): %v", err)
	}

	// 5) Activate the in-process session (no restart, 01 §4.4 / doc 01 §6b).
	if n.activateHook != nil {
		if err := n.activateHook(); err != nil {
			logf(n.options.Log, "adoption: activate failed (continuing): %v", err)
		}
	}
	// Re-announce over mDNS with the cluster identity (cf + init=1) so
	// controllers stop classifying this node as adoptable (02 §2.4).
	n.registerMDNS()
	logf(n.options.Log, "adoption: installed into cluster %q (mTLS active)", inst.ClusterName)
	return nil
}

// --- pinned HTTP phase runner (controller -> target /bootstrap/adopt) --------

// pinnedRunner drives the three /bootstrap/adopt phases against a target over a
// self-signed TLS channel pinned to the operator-supplied fingerprint.
type pinnedRunner struct {
	client  *http.Client
	baseURL string
}

// dialTarget builds a pinnedRunner for addr, pinning its self-signed leaf to
// fingerprint ("sha256:<hex>"). It returns the runner + observed target IP(s) for
// the leaf SANs. A nil client builds the default per-call pinning client.
func dialTarget(client *http.Client, addr, fingerprint string) (*pinnedRunner, []net.IP, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		addr = net.JoinHostPort(host, bootstrapPort)
	}
	var observed []net.IP
	if ip := net.ParseIP(host); ip != nil {
		observed = []net.IP{ip}
	}
	want := normalizeFingerprint(fingerprint)
	if client == nil {
		client = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // self-signed bootstrap channel; we pin instead
					VerifyConnection: func(cs tls.ConnectionState) error {
						if len(cs.PeerCertificates) == 0 {
							return errors.New("daemon: no peer cert")
						}
						got := pki.Fingerprint(cs.PeerCertificates[0].Raw)
						if want != "" && !pki.ConstantTimeEqualFingerprint(got, want) {
							return web.ErrFingerprintMismatch
						}
						return nil
					},
				},
			},
		}
	}
	return &pinnedRunner{client: client, baseURL: "https://" + addr}, observed, nil
}

func (r *pinnedRunner) Key(ctx context.Context, req adopt.KeyReq) (adopt.KeyResp, error) {
	var resp adopt.KeyResp
	err := r.post(ctx, "key", req, &resp)
	return resp, err
}
func (r *pinnedRunner) CSR(ctx context.Context, req adopt.CSRReq) (adopt.CSRResp, error) {
	var resp adopt.CSRResp
	err := r.post(ctx, "csr", req, &resp)
	return resp, err
}
func (r *pinnedRunner) Complete(ctx context.Context, req adopt.CompleteReq) (adopt.CompleteResp, error) {
	var resp adopt.CompleteResp
	err := r.post(ctx, "complete", req, &resp)
	return resp, err
}

// takeover drives POST /bootstrap/takeover: present the target's CURRENT
// cluster admin password so it self-releases and reopens bootstrap (03 §4).
// 401 => wrong password; 429 => the target's guard throttled us.
func (r *pinnedRunner) takeover(password string) error {
	buf, err := json.Marshal(struct {
		Password string `json:"password"`
	}{Password: password})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, r.baseURL+"/bootstrap/takeover", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", web.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: wrong takeover password", web.ErrWrongPassword)
	case http.StatusTooManyRequests:
		return adopt.ErrRateLimited
	default:
		return fmt.Errorf("%w: takeover returned %d", web.ErrUnreachable, resp.StatusCode)
	}
}

// info fetches GET /bootstrap/info and returns the target's state string. A 403
// means the target is already a member.
func (r *pinnedRunner) info() (string, error) {
	req, err := http.NewRequest(http.MethodGet, r.baseURL+"/bootstrap/info", nil)
	if err != nil {
		return "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return "member", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon: /bootstrap/info status %d", resp.StatusCode)
	}
	var info web.BootstrapInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.State, nil
}

func (r *pinnedRunner) post(ctx context.Context, phase string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.baseURL+"/bootstrap/adopt?phase="+phase, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", web.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusToErr(resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusToErr maps a target's non-200 bootstrap status to a controller-side
// sentinel (08 §A.2 status map).
func statusToErr(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return adopt.ErrBadPIN
	case http.StatusForbidden:
		return web.ErrForeign
	case http.StatusUnprocessableEntity:
		return web.ErrEpochMismatch
	case http.StatusTooManyRequests:
		return adopt.ErrRateLimited
	default:
		return fmt.Errorf("%w: target returned %d", web.ErrUnreachable, status)
	}
}

// mapEngineErr maps an adopt-engine error to the web sentinel the handler expects.
func mapEngineErr(err error) error {
	switch {
	case errors.Is(err, adopt.ErrForeign):
		return web.ErrForeign
	case errors.Is(err, adopt.ErrEpochMismatch):
		return web.ErrEpochMismatch
	case errors.Is(err, adopt.ErrBadPIN):
		return fmt.Errorf("%w: bad PIN", web.ErrUnreachable)
	default:
		return err
	}
}

// --- small helpers -----------------------------------------------------------

func ipsToStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func indexNode(nodes []state.NodeRecord, id string) int {
	for i := range nodes {
		if nodes[i].ID == id {
			return i
		}
	}
	return -1
}

func dropNode(nodes []state.NodeRecord, id string) []state.NodeRecord {
	out := nodes[:0]
	for _, nr := range nodes {
		if nr.ID != id {
			out = append(out, nr)
		}
	}
	return out
}

func dropString(ss []string, drop string) []string {
	out := ss[:0]
	for _, s := range ss {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// certFingerprint returns the lowercase-hex SHA-256 over the leaf DER decoded from
// a cert PEM (the RevokedSet key, 03 §5.2). "" on a bad PEM.
func certFingerprint(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// normalizeFingerprint strips an optional "sha256:" prefix and lowercases the hex.
func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	fp = strings.TrimPrefix(fp, "sha256:")
	return strings.ToLower(fp)
}
