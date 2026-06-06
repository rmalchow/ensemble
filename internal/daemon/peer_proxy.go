package daemon

// peer_proxy.go is the cross-node control proxy behind transport.peer (08 §F/§G
// fan-out): an mTLS HTTP client presenting THIS node's cluster leaf, verified
// manually against the cluster CA + the live revoked set + the EXPECTED peer id
// (CN pinning — identity-addressed, so a peer that moved IP since its leaf was
// signed still authenticates; plain SAN verification would refuse it). The
// peer's auth chain admits the client via the [2] node-cert path, so the §F.1
// reads work without a human session.
//
// Wired pieces: ListMedia (the Media screen's master-scoped listing — without
// it a group whose master is a PEER showed an empty library) and MediaExists
// (the §F.2 master-side existence check). FanOutTransport is a deliberate no-op
// (the store-change gossip kick already converges the doc to the master within
// milliseconds — plane.go kickSync); MemberStatus reports gossip liveness with
// zeroed telemetry (the per-member telemetry read needs a node-authorized
// endpoint, a noted gap); CalibratePlay is unimplemented (the calibrate
// endpoint is admin-session-only by design, so a node cert cannot drive it).

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/pki"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/web"
)

// httpPeer implements peerProxy over the cluster mTLS control plane.
type httpPeer struct {
	n  *Node
	cp *clusterPlane
}

// endpoint resolves a live peer's control address (host:port) from gossip Meta.
// A peer that is not currently alive is unreachable by definition (its record
// addrs carry no control port).
func (p *httpPeer) endpoint(nodeID string) (string, error) {
	if p.cp != nil && p.cp.mem != nil {
		for _, m := range p.cp.mem.Members() {
			if m.Meta.NodeID == nodeID && m.Meta.WebPort > 0 {
				return m.WebAddr(), nil
			}
		}
	}
	return "", fmt.Errorf("%w: node %s is not live", web.ErrUnreachable, shortID(nodeID))
}

// client builds the per-call mTLS client pinned to the expected peer identity:
// chain to the cluster CA, not revoked, CN == nodeID.
func (p *httpPeer) client(nodeID string) (*http.Client, error) {
	ci := p.n.tls.cluster.Load()
	if ci == nil {
		return nil, fmt.Errorf("%w: no cluster identity", web.ErrUnreachable)
	}
	verifier := pki.NewPeerVerifier(p.n.revokedPredicate(), func(id string) bool { return id == nodeID })
	caPool := ci.caPool
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{ci.leaf},
		// Manual verification below: identity-addressed (CN == nodeID) instead of
		// IP-SAN-addressed, so a renumbered peer still authenticates.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("daemon: peer sent no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			for _, der := range rawCerts[1:] {
				if c, cerr := x509.ParseCertificate(der); cerr == nil {
					pool.AddCert(c)
				}
			}
			if _, err := leaf.Verify(x509.VerifyOptions{
				Roots:         caPool,
				Intermediates: pool,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			}); err != nil {
				return fmt.Errorf("daemon: peer chain: %w", err)
			}
			return verifier.Verify(rawCerts, nil)
		},
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

// get performs an mTLS GET against the peer and decodes the JSON body into out.
func (p *httpPeer) get(nodeID, pathAndQuery string, out any) error {
	addr, err := p.endpoint(nodeID)
	if err != nil {
		return err
	}
	client, err := p.client(nodeID)
	if err != nil {
		return err
	}
	resp, err := client.Get("https://" + addr + pathAndQuery)
	if err != nil {
		return fmt.Errorf("%w: %v", web.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: peer returned %d", web.ErrUnreachable, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ListMedia proxies the §F.1 listing (one data/-relative folder: files + dirs)
// to the peer over mTLS.
func (p *httpPeer) ListMedia(nodeID, browsePath string) ([]web.MediaFile, []string, error) {
	q := url.Values{"node": {nodeID}}
	if browsePath != "" {
		q.Set("path", browsePath)
	}
	var body struct {
		Files []web.MediaFile `json:"files"`
		Dirs  []string        `json:"dirs"`
	}
	if err := p.get(nodeID, "/api/v1/media?"+q.Encode(), &body); err != nil {
		return nil, nil, err
	}
	return body.Files, body.Dirs, nil
}

// MediaExists is the §F.2 master-side existence check: list the file's folder
// on the master and look for it.
func (p *httpPeer) MediaExists(nodeID, file string) (bool, error) {
	dir := path.Dir(file)
	if dir == "." {
		dir = ""
	}
	files, _, err := p.ListMedia(nodeID, dir)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if f.File == file {
			return true, nil
		}
	}
	return false, nil
}

// FanOutTransport is a deliberate no-op: the §F.3/§F.4 doc write already
// triggers an immediate gossip push to every live peer (plane.go kickSync), so
// the master observes the transport change without a second channel.
func (p *httpPeer) FanOutTransport(string, string) error { return nil }

// MemberStatus reports gossip liveness with zeroed telemetry (the cross-node
// telemetry read needs a node-authorized endpoint — noted gap). An error means
// "offline" to the caller; success is forced Online by the §G.2 aggregator.
func (p *httpPeer) MemberStatus(nodeID, _ string) (web.MemberStatus, error) {
	if _, err := p.endpoint(nodeID); err != nil {
		return web.MemberStatus{}, err
	}
	return web.MemberStatus{NodeID: nodeID, ClockQuality: "poor"}, nil
}

// CalibratePlay cannot be proxied with a node cert (the endpoint is
// admin-session-only by design); surface unreachable so the warning lands on
// the row instead of a silent skip.
func (p *httpPeer) CalibratePlay(string, int) error {
	return fmt.Errorf("%w: cross-node calibrate requires an admin session on the target", web.ErrUnreachable)
}
