package main

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
	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// This file is the cmd-only glue that binds the transport-agnostic
// internal/adopt engine to the concrete pki (CA signing), state (ConfigDoc write
// under If-Match + grow-only RevokedSet) and cluster (gossip rekey) subsystems.
// It is the SOLE place adopt ↔ pki ↔ state ↔ cluster are wired (01 §6); web
// reaches all of it only through the Deps.{Adopt,Forget,Leave} closures and the
// Bootstrap seam, never importing these engines directly.
//
// The daemon (P0.3) does not yet construct live state/cluster handles, so the
// wiring here is exposed as constructors that take the subsystems as parameters
// (controllerDeps / bootstrapDeps). They are reusable + unit-tested in isolation
// (wire_adopt_test.go) and ready to be called once the daemon stands up the
// realtime planes in the P2 wiring step.

// leafValidity is the signed leaf lifetime (A.12: 30 days; renew at ⅓ life).
const leafValidity = 30 * 24 * time.Hour

// rekeyer is the gossip-rekey surface the forget/takeover path drives
// (cluster.Membership.Rekey). It is an interface so the wiring is testable without
// a live memberlist.
type rekeyer interface {
	Rekey(key []byte) error
}

// signer is the CA signing surface (pki.CA.Sign), narrowed to an interface so the
// wiring is testable with a fake CA.
type signer interface {
	Sign(csr *x509.CertificateRequest, nodeID string, addrs []net.IP, validity time.Duration, now time.Time) (certPEM []byte, err error)
	CertPEM() []byte
}

// adoptController bundles the live subsystems the controller-side closures need.
type adoptController struct {
	store       *state.Store
	ca          signer
	clusterName string
	secrets     adopt.ClusterSecrets // {caKeyPem, gossipKey} replicated projection
	httpClient  *http.Client         // injected in tests; nil => a default pinning client per-call
}

// newAdoptFunc returns the Deps.Adopt closure (08 §C.3/§C.4). It pins the target's
// self-signed cert by fingerprint, drives the three /bootstrap/adopt phases (CA
// signing on this controller, Model B), then writes the NodeRecord into the
// ConfigDoc under optimistic concurrency and gossips. force=true allows a foreign
// target (takeover); force=false surfaces web.ErrForeign.
func (c *adoptController) newAdoptFunc() func(addr, fingerprint, pin, nodeID, name string, force bool) error {
	return c.newAdoptFuncTo("")
}

// newAdoptFuncTo is newAdoptFunc with an explicit base URL override (tests pass an
// httptest server URL); an empty override derives https://<addr> from the addr.
func (c *adoptController) newAdoptFuncTo(baseURL string) func(addr, fingerprint, pin, nodeID, name string, force bool) error {
	return func(addr, fingerprint, pin, nodeID, name string, force bool) error {
		runner, observed, err := c.dialTarget(addr, fingerprint)
		if err != nil {
			return err
		}
		if baseURL != "" {
			runner.baseURL = baseURL
		}
		// Probe state for the foreign/member gate before the PIN exchange.
		nodeState, err := runner.info()
		if err != nil {
			return fmt.Errorf("%w: %v", web.ErrUnreachable, err)
		}

		ctrl := &adopt.Controller{
			Sign:        c.signFunc(),
			CABundle:    c.ca.CertPEM(),
			Secrets:     c.secrets,
			ClusterName: c.clusterName,
		}
		seed, err := ctrl.Run(context.Background(), runner, pin, nodeID, name, nodeState, observed, force)
		if err != nil {
			return mapEngineErr(err)
		}
		return c.recordNode(seed)
	}
}

// signFunc adapts the CA into adopt.SignFunc: parse the CSR PEM, sign with SANs
// from the authenticated nodeID + observed addrs (never the CSR's own).
func (c *adoptController) signFunc() adopt.SignFunc {
	return func(csrPEM []byte, nodeID string, addrs []net.IP) ([]byte, error) {
		block, rest := pem.Decode(csrPEM)
		if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
			return nil, errors.New("wire: bad CSR PEM")
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("wire: parse CSR: %w", err)
		}
		return c.ca.Sign(csr, nodeID, addrs, leafValidity, time.Now())
	}
}

// recordNode writes the adopted node's NodeRecord into the ConfigDoc under
// optimistic concurrency. It retries on a mid-handshake gossip version bump
// (re-GET version, re-apply the add) rather than failing the whole adoption — the
// node is already signed at this point (open question §9.5).
func (c *adoptController) recordNode(seed adopt.NodeRecordSeed) error {
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		doc := c.store.Get()
		// Idempotent upsert: replace an existing record (takeover) or append.
		rec := state.NodeRecord{
			ID:      seed.ID,
			Name:    seed.Name,
			CertPEM: string(seed.CertPEM),
			Addrs:   ipsToStrings(seed.Addrs),
		}
		replaced := false
		for j := range doc.Nodes {
			if doc.Nodes[j].ID == seed.ID {
				// Preserve operator-set fields (channel/gain/etc.) on takeover.
				rec.HWDelayUs = doc.Nodes[j].HWDelayUs
				rec.Channel = doc.Nodes[j].Channel
				rec.GainDB = doc.Nodes[j].GainDB
				rec.Caps = doc.Nodes[j].Caps
				doc.Nodes[j] = rec
				replaced = true
				break
			}
		}
		if !replaced {
			doc.Nodes = append(doc.Nodes, rec)
		}
		if _, err := c.store.Apply(doc); err != nil {
			if errors.Is(err, state.ErrConflict) {
				continue // gossip bumped the version mid-handshake; retry the write
			}
			return err
		}
		return nil
	}
	return web.ErrVersionConflict
}

// newForgetFunc returns the Deps.Forget closure (08 §C.5): a single ConfigDoc
// write that adds the leaf's SHA-256 fingerprint to the grow-only RevokedSet,
// drops the NodeRecord, and pulls the id from every GroupRecord.MemberNodeIDs;
// then it triggers a gossip rekey so the forgotten node cannot re-add its IP.
func (c *adoptController) newForgetFunc(rk rekeyer, newGossipKey func() []byte) func(nodeID string) error {
	return func(nodeID string) error {
		doc := c.store.Get()

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
		// Refuse to forget the last node (08 §C.5 409).
		if len(doc.Nodes) <= 1 {
			return web.ErrLastNode
		}

		// 1. RevokedSet add (grow-only): the leaf's SHA-256(DER) fingerprint.
		if fp := certFingerprint(rec.CertPEM); fp != "" {
			doc.Revoked.Entries = append(doc.Revoked.Entries, state.RevokedCert{
				Fingerprint: fp,
				NodeID:      nodeID,
				Reason:      "forget",
				At:          time.Now().UTC().Format(time.RFC3339),
			})
		}
		// 2. Drop the NodeRecord.
		doc.Nodes = dropNode(doc.Nodes, nodeID)
		// 3. Pull the id from every group's membership.
		for i := range doc.Groups {
			doc.Groups[i].MemberNodeIDs = dropString(doc.Groups[i].MemberNodeIDs, nodeID)
		}

		if _, err := c.store.Apply(doc); err != nil {
			if errors.Is(err, state.ErrConflict) {
				return web.ErrVersionConflict
			}
			return err
		}

		// 4. Gossip rekey so the forgotten node's IP cannot re-join (03 §5.3).
		// Best-effort: an open cluster (no keyring) returns an error we swallow.
		if rk != nil && newGossipKey != nil {
			_ = rk.Rekey(newGossipKey())
		}
		return nil
	}
}

// --- bootstrap node-side wiring ---------------------------------------------

// bootstrapDeps bundles the node-side handles for the BootstrapDeps seam.
type bootstrapDeps struct {
	nodeID  string
	leafKey crypto.Signer
	guard   bootstrapGuard
	info    func() web.BootstrapInfo
	install func(adopt.Installed) error
}

// bootstrapGuard is the auth.AdoptionGuard surface adapted to the adopt seams. It
// is satisfied by an adapter over internal/auth.AdoptionGuard (the canonical
// guard, P1.2) so bootstrap consumes that guard, not a freshly-minted one (hard
// rule). The adapter lives in wire_adopt_guard.go.
type bootstrapGuard interface {
	adopt.Throttle
	adopt.NonceStore
}

// newBootstrapDeps assembles the web.BootstrapDeps seam from the node-side
// handles: the adopt.Node (built over the leaf key + PIN + the guard), the CSR
// builder (pki.NewCSR), the Install hook, and the Info projection.
func (b *bootstrapDeps) build(pin string) *web.BootstrapDeps {
	node := adopt.NewNode(b.nodeID, pin, b.leafKey, b.guard)
	return &web.BootstrapDeps{
		Node:    node,
		Guard:   b.guard,
		CSR:     func() ([]byte, error) { return pki.NewCSR(b.leafKey, b.nodeID) },
		Install: b.install,
		Info:    b.info,
	}
}

// --- HTTP phase runner (controller -> target /bootstrap/adopt) ---------------

// pinnedRunner drives the three /bootstrap/adopt phases against a target over a
// self-signed TLS channel pinned to the operator-supplied fingerprint.
type pinnedRunner struct {
	client  *http.Client
	baseURL string // https://<addr>
}

// dialTarget builds a pinnedRunner for addr, pinning its self-signed leaf to
// fingerprint ("sha256:<hex>"). It returns the runner and the observed target
// IP(s) for the leaf SANs. The pin is enforced in a VerifyPeerCertificate hook
// (the bootstrap channel is self-signed, so normal chain verification is skipped).
func (c *adoptController) dialTarget(addr, fingerprint string) (*pinnedRunner, []net.IP, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // bare host[:port omitted] — default the bootstrap port below
		addr = net.JoinHostPort(host, bootstrapPort)
	}
	var observed []net.IP
	if ip := net.ParseIP(host); ip != nil {
		observed = []net.IP{ip}
	}

	want := normalizeFingerprint(fingerprint)
	client := c.httpClient
	if client == nil {
		client = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // self-signed bootstrap channel; we pin instead
					VerifyConnection: func(cs tls.ConnectionState) error {
						if len(cs.PeerCertificates) == 0 {
							return errors.New("wire: no peer cert")
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

// bootstrapPort is the default control/bootstrap port (open question §9.1: the
// bootstrap surface shares the control listener :8443 by serving the /bootstrap/*
// routes; the self-signed-vs-mTLS split is by request, not by port, in this
// build). A.12 control mTLS port = 8443.
const bootstrapPort = "8443"

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

// info fetches GET /bootstrap/info and returns the target's state string
// ("uninitialized"/"foreign"/"member"). A 403 means the target is already a
// member (foreign to us in the takeover sense handled by the engine).
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
		return "", fmt.Errorf("wire: /bootstrap/info status %d", resp.StatusCode)
	}
	var info web.BootstrapInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.State, nil
}

// post marshals body, POSTs it to /bootstrap/adopt?phase=<phase>, and decodes the
// reply into out. A non-200 maps the canonical error envelope's status to a
// web sentinel so the handler renders the right code.
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

func dropNode(nodes []state.NodeRecord, id string) []state.NodeRecord {
	out := nodes[:0]
	for _, n := range nodes {
		if n.ID != id {
			out = append(out, n)
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

// normalizeFingerprint strips an optional "sha256:" prefix and lowercases the hex
// so the operator-supplied pin matches pki.Fingerprint's output.
func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	fp = strings.TrimPrefix(fp, "sha256:")
	return strings.ToLower(fp)
}
