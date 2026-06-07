package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// FollowClientImpl implements contracts.FollowClient. The group engine (H) calls
// it during takeover (§5.2) to drive POST /api/follow / /api/unfollow on peers.
// It dials peers directly (not through the proxy) using DialCandidates, setting
// X-Ensemble-Proxied:1 so the peer treats it as a terminal request. Built before
// the engine (D16/D31), bound only to the cluster — no dependency on the server.
type FollowClientImpl struct {
	cluster Cluster
	http    *http.Client
}

// NewFollowClient returns a cluster-backed follow client (D16). Used by K to
// wire H before the API server exists.
func NewFollowClient(c Cluster) FollowClientImpl {
	return FollowClientImpl{
		cluster: c,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

var _ contracts.FollowClient = FollowClientImpl{}

// Follow issues POST /api/follow {target} to peer (§5.1/§5.2).
func (f FollowClientImpl) Follow(ctx context.Context, peer, target id.ID) error {
	body, _ := json.Marshal(FollowReq{Target: target.String()})
	return f.post(ctx, peer, "/api/follow", body)
}

// Unfollow issues POST /api/unfollow to peer (§5.1/§5.2).
func (f FollowClientImpl) Unfollow(ctx context.Context, peer id.ID) error {
	return f.post(ctx, peer, "/api/unfollow", nil)
}

// post sends body to peer's HTTP port, trying dial candidates in order. The
// one-hop proxied header marks it terminal so the peer never re-proxies.
func (f FollowClientImpl) post(ctx context.Context, peer id.ID, path string, body []byte) error {
	port := f.httpPortOf(peer)
	addrs := f.cluster.DialCandidates(peer)
	if port == 0 || len(addrs) == 0 {
		return fmt.Errorf("follow_client: peer %s unreachable", peer)
	}

	var lastErr error
	for _, a := range addrs {
		url := "http://" + net.JoinHostPort(a.String(), strconv.Itoa(port)) + path
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(proxiedHeader, "1")
		req.Header.Set(fromHeader, f.cluster.Self().String())

		resp, err := f.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		return fmt.Errorf("follow_client: peer %s %s: status %d", peer, path, resp.StatusCode)
	}
	return fmt.Errorf("follow_client: peer %s %s: %w", peer, path, lastErr)
}

func (f FollowClientImpl) httpPortOf(peer id.ID) int {
	for _, n := range f.cluster.Snapshot().Nodes {
		if n.ID == peer {
			return n.HTTPPort
		}
	}
	return 0
}
