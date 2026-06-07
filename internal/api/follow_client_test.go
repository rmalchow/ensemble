package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// stubPeer is a minimal peer that records follow/unfollow calls.
type stubPeer struct {
	srv     *httptest.Server
	path    string
	body    string
	proxied string
	from    string
	status  int
}

func newStubPeer(t *testing.T, status int) *stubPeer {
	sp := &stubPeer{status: status}
	sp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sp.path = r.URL.Path
		sp.body = string(b)
		sp.proxied = r.Header.Get(proxiedHeader)
		sp.from = r.Header.Get(fromHeader)
		w.WriteHeader(sp.status)
	}))
	t.Cleanup(sp.srv.Close)
	return sp
}

func peerPort(t *testing.T, ts *httptest.Server) int {
	return portOf(t, ts)
}

func TestFollowClientFollow(t *testing.T) {
	self := id.New()
	peer := id.New()
	target := id.New()
	sp := newStubPeer(t, http.StatusNoContent)

	fc := newFakeCluster(self)
	fc.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: peer, HTTPPort: peerPort(t, sp.srv)},
	}})
	fc.dial[peer] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

	cl := NewFollowClient(fc)
	if err := cl.Follow(context.Background(), peer, target); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if sp.path != "/api/follow" {
		t.Errorf("path = %q", sp.path)
	}
	if sp.proxied != "1" {
		t.Errorf("proxied header not set")
	}
	if sp.from != self.String() {
		t.Errorf("from = %q, want %q", sp.from, self.String())
	}
	var got FollowReq
	json.Unmarshal([]byte(sp.body), &got)
	if got.Target != target.String() {
		t.Errorf("target in body = %q", got.Target)
	}
}

func TestFollowClientUnfollow(t *testing.T) {
	self := id.New()
	peer := id.New()
	sp := newStubPeer(t, http.StatusNoContent)

	fc := newFakeCluster(self)
	fc.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: peer, HTTPPort: peerPort(t, sp.srv)},
	}})
	fc.dial[peer] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

	cl := NewFollowClient(fc)
	if err := cl.Unfollow(context.Background(), peer); err != nil {
		t.Fatalf("Unfollow: %v", err)
	}
	if sp.path != "/api/unfollow" {
		t.Errorf("path = %q", sp.path)
	}
}

func TestFollowClientDialFailover(t *testing.T) {
	self := id.New()
	peer := id.New()
	sp := newStubPeer(t, http.StatusNoContent)

	fc := newFakeCluster(self)
	fc.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: peer, HTTPPort: peerPort(t, sp.srv)},
	}})
	fc.dial[peer] = []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), // dead
		netip.MustParseAddr("127.0.0.1"), // live
	}

	cl := NewFollowClient(fc)
	cl.http.Transport = &http.Transport{
		DialContext: dialTimeout2s,
	}
	if err := cl.Follow(context.Background(), peer, id.New()); err != nil {
		t.Fatalf("failover Follow: %v", err)
	}
	if sp.path != "/api/follow" {
		t.Errorf("did not reach live peer: %q", sp.path)
	}
}

func TestFollowClientErrorPropagates(t *testing.T) {
	self := id.New()
	peer := id.New()
	sp := newStubPeer(t, http.StatusConflict)

	fc := newFakeCluster(self)
	fc.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: peer, HTTPPort: peerPort(t, sp.srv)},
	}})
	fc.dial[peer] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

	cl := NewFollowClient(fc)
	err := cl.Follow(context.Background(), peer, id.New())
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("want 409 error, got %v", err)
	}
}

func TestFollowClientUnreachable(t *testing.T) {
	self := id.New()
	peer := id.New()
	fc := newFakeCluster(self)
	fc.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{{ID: peer, HTTPPort: 9}}})
	// No dial candidates.
	cl := NewFollowClient(fc)
	if err := cl.Unfollow(context.Background(), peer); err == nil {
		t.Fatal("want unreachable error")
	}
}
