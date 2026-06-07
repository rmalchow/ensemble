package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// startNode spins up a real api.Server on a loopback listener with the given
// self id and name, returning its server, base URL, and bound port.
func startNode(t *testing.T, self id.ID, name string, fc *fakeCluster) (*Server, *httptest.Server) {
	t.Helper()
	cfg, _, _ := baseConfig(self)
	cfg.Cluster = fc
	cfg.Media = &fakeMedia{files: []MediaFile{{Path: name + ".flac", Name: name + ".flac"}}}
	s, ts := testServer(t, cfg)
	return s, ts
}

// portOf extracts the integer port from an httptest server URL.
func portOf(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	u := strings.TrimPrefix(ts.URL, "http://")
	_, ps, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(ps)
	return p
}

func TestProxyByNodeID(t *testing.T) {
	id1, id2 := id.New(), id.New()

	// node2 serves its own media.
	fc2 := newFakeCluster(id2)
	_, ts2 := startNode(t, id2, "node2", fc2)
	port2 := portOf(t, ts2)

	// node1 knows node2's address+port and dial candidate.
	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "node2", Alive: true, HTTPPort: port2},
	}})
	fc1.dial[id2] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	_, ts1 := startNode(t, id1, "node1", fc1)

	resp := doJSON(t, ts1, http.MethodGet, "/api/"+id2.String()+"/media", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got MediaResp
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Files) != 1 || got.Files[0].Path != "node2.flac" {
		t.Errorf("expected node2 media, got %+v", got)
	}
}

func TestProxyByUniqueName(t *testing.T) {
	id1, id2 := id.New(), id.New()
	fc2 := newFakeCluster(id2)
	_, ts2 := startNode(t, id2, "node2", fc2)
	port2 := portOf(t, ts2)

	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "kitchen", Alive: true, HTTPPort: port2},
	}})
	fc1.dial[id2] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	_, ts1 := startNode(t, id1, "node1", fc1)

	resp := doJSON(t, ts1, http.MethodGet, "/api/kitchen/media", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProxyAmbiguousName404(t *testing.T) {
	id1, id2, id3 := id.New(), id.New(), id.New()
	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "dup", Alive: true, HTTPPort: 1},
		{ID: id3, Name: "dup", Alive: true, HTTPPort: 2},
	}})
	_, ts1 := startNode(t, id1, "node1", fc1)

	resp := doJSON(t, ts1, http.MethodGet, "/api/dup/media", nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != 404 || e.Error != "ambiguous_node" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}

func TestProxyReservedRouteLocal(t *testing.T) {
	self := id.New()
	fc := newFakeCluster(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	_, ts := startNode(t, self, "n", fc)

	resp := doJSON(t, ts, http.MethodGet, "/api/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got StatusResp
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.ID != self.String() {
		t.Errorf("status should be local, got id %q", got.ID)
	}
}

func TestProxySelfHandledLocally(t *testing.T) {
	self := id.New()
	fc := newFakeCluster(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	_, ts := startNode(t, self, "n", fc)

	resp := doJSON(t, ts, http.MethodGet, "/api/"+self.String()+"/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got StatusResp
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.ID != self.String() {
		t.Errorf("self-proxy should be local, got %q", got.ID)
	}
}

func TestProxyOneHopGuard(t *testing.T) {
	self := id.New()
	foreign := id.New()
	fc := newFakeCluster(self)
	fc.setSnapshot(snapWith(self, self, []id.ID{self}, "n"))
	// Give a (broken) dial candidate so if it WERE re-proxied it would 502;
	// with the guard it must serve locally instead.
	fc.dial[foreign] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	fc.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: self, Name: "n", Alive: true, HTTPPort: 1},
		{ID: foreign, Name: "f", Alive: true, HTTPPort: 1},
	}})
	_, ts := startNode(t, self, "n", fc)

	// Request a FOREIGN id but with the proxied header → must be terminal.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/"+foreign.String()+"/cluster", nil)
	req.Header.Set(proxiedHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Served locally → 200 (cluster), not 502.
	if resp.StatusCode != 200 {
		t.Fatalf("one-hop guard failed: status %d", resp.StatusCode)
	}
}

// captureNode records inbound request headers + path for proxy assertions.
type captureNode struct {
	srv    *httptest.Server
	header http.Header
	path   string
	method string
	body   string
}

func newCaptureNode(t *testing.T, status int) *captureNode {
	cn := &captureNode{}
	cn.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cn.header = r.Header.Clone()
		cn.path = r.URL.Path
		cn.method = r.Method
		cn.body = string(b)
		w.WriteHeader(status)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(cn.srv.Close)
	return cn
}

func TestProxySetsProxiedHeaderAndStripsPath(t *testing.T) {
	id1, id2 := id.New(), id.New()
	cn := newCaptureNode(t, 200)
	port2 := portOf(t, cn.srv)

	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "n2", Alive: true, HTTPPort: port2},
	}})
	fc1.dial[id2] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	_, ts1 := startNode(t, id1, "n1", fc1)

	resp := doJSON(t, ts1, http.MethodGet, "/api/"+id2.String()+"/media", nil)
	resp.Body.Close()

	if cn.header.Get(proxiedHeader) != "1" {
		t.Errorf("proxied header not set")
	}
	if cn.header.Get(fromHeader) != id1.String() {
		t.Errorf("from header = %q, want %q", cn.header.Get(fromHeader), id1.String())
	}
	if cn.path != "/api/media" {
		t.Errorf("path stripped to %q, want /api/media", cn.path)
	}
}

func TestProxyStreamsBodyAndMethod(t *testing.T) {
	id1, id2 := id.New(), id.New()
	cn := newCaptureNode(t, 204)
	port2 := portOf(t, cn.srv)

	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "n2", Alive: true, HTTPPort: port2},
	}})
	fc1.dial[id2] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	_, ts1 := startNode(t, id1, "n1", fc1)

	resp := doJSON(t, ts1, http.MethodPost, "/api/"+id2.String()+"/follow",
		map[string]any{"target": "abc"})
	resp.Body.Close()

	if cn.method != http.MethodPost {
		t.Errorf("method = %q", cn.method)
	}
	if !strings.Contains(cn.body, "abc") {
		t.Errorf("body not forwarded: %q", cn.body)
	}
}

func TestProxyDialFailover(t *testing.T) {
	id1, id2 := id.New(), id.New()
	cn := newCaptureNode(t, 200)
	port2 := portOf(t, cn.srv)

	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "n2", Alive: true, HTTPPort: port2},
	}})
	// First candidate is a dead IP/host; second is the live loopback.
	fc1.dial[id2] = []netip.Addr{
		netip.MustParseAddr("192.0.2.1"), // TEST-NET-1, unroutable
		netip.MustParseAddr("127.0.0.1"),
	}
	_, ts1 := startNode(t, id1, "n1", fc1)

	resp := doJSON(t, ts1, http.MethodGet, "/api/"+id2.String()+"/media", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("failover did not reach second candidate: %d", resp.StatusCode)
	}
}

func TestProxyUnreachable502(t *testing.T) {
	id1, id2 := id.New(), id.New()
	fc1 := newFakeCluster(id1)
	fc1.setSnapshot(contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: id2, Name: "n2", Alive: true, HTTPPort: 9},
	}})
	// No dial candidates → unreachable.
	_, ts1 := startNode(t, id1, "n1", fc1)

	resp := doJSON(t, ts1, http.MethodGet, "/api/"+id2.String()+"/media", nil)
	e := decodeErr(t, resp)
	if resp.StatusCode != 502 || e.Error != "unreachable" {
		t.Fatalf("status=%d err=%q", resp.StatusCode, e.Error)
	}
}
