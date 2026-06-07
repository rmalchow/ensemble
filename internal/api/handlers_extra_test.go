package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// TestGroupMasterForwardsToMaster pins §5.2/D17: a takeover posted at a NON-
// master member is forwarded (one hop) to the group's current master instead
// of failing with not_master — the UI's play-from-node flow depends on it.
func TestGroupMasterForwardsToMaster(t *testing.T) {
	self, master := id.New(), id.New()

	// Stand-in "master" node: accepts the forwarded request once and asserts
	// the one-hop guard header.
	got := make(chan MasterReq, 1)
	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/group/master" || r.Header.Get(proxiedHeader) == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req MasterReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		got <- req
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ms.Close()
	host, portStr, err := net.SplitHostPort(ms.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	mport, _ := strconv.Atoi(portStr)

	fc := newFakeCluster(self)
	fc.setSnapshot(contracts.Snapshot{
		Nodes: []contracts.NodeView{
			{ID: self, Name: "self", Alive: true},
			{ID: master, Name: "m", Alive: true, HTTPPort: mport},
		},
		Groups: []contracts.GroupView{{
			ID: master, Master: master, Members: []id.ID{master, self},
		}},
	})
	fc.dial[master] = []netip.Addr{netip.MustParseAddr(host)}

	cfg, _, _ := baseConfig(self)
	cfg.Cluster = fc
	cfg.Group = &fakeGroup{makeMasterErr: ErrNotMaster} // local engine: not master
	_, ts := testServer(t, cfg)

	resp := doJSON(t, ts, http.MethodPost, "/api/group/master", map[string]any{"node": self.String()})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (forwarded)", resp.StatusCode)
	}
	resp.Body.Close()
	select {
	case req := <-got:
		if req.Node != self.String() {
			t.Fatalf("forwarded node = %s, want %s", req.Node, self)
		}
	default:
		t.Fatal("master never received the forwarded takeover")
	}
}
