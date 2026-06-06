package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

func decodeBody(t *testing.T, b []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// clusterDeps builds a Deps with adopt/forget/leave spies, an initialized cluster
// (so login works), and a ConfigView the handlers read back version/groups from.
type clusterSpy struct {
	adoptCalls   []adoptCall
	forgetCalls  []string
	leaveCalls   int
	adoptErr     error
	forgetErr    error
	leaveErr     error
	view         ConfigView
}

type adoptCall struct {
	addr, fingerprint, pin, nodeID, name string
	force                                bool
}

func newClusterServer(spy *clusterSpy) *Server {
	return New(Deps{
		NodeID:              "n-self",
		Initialized:         func() bool { return true },
		VerifyAdminPassword: func(pw string) bool { return pw == testPw },
		ConfigVersion:       func() uint64 { return spy.view.Version },
		State:               func() ConfigView { return spy.view },
		Adopt: func(addr, fingerprint, pin, nodeID, name, _ string, force bool) error {
			spy.adoptCalls = append(spy.adoptCalls, adoptCall{addr, fingerprint, pin, nodeID, name, force})
			return spy.adoptErr
		},
		Forget: func(nodeID string) error {
			spy.forgetCalls = append(spy.forgetCalls, nodeID)
			return spy.forgetErr
		},
		Leave: func() error {
			spy.leaveCalls++
			return spy.leaveErr
		},
	}, "")
}

func TestClusterAdoptDrivesPhases(t *testing.T) {
	spy := &clusterSpy{view: ConfigView{Version: 45, Nodes: []NodeView{{ID: "n-9b1d", Name: "Bedroom"}}}}
	s := newClusterServer(spy)
	cookie := loginAndGetCookie(t, s)
	hdr := cookieHdr(cookie)
	hdr["If-Match"] = "44"

	body := adoptRequest{NodeID: "n-9b1d", Addr: "192.168.1.55", Fingerprint: "sha256:aa", PIN: "0000", Name: "Bedroom"}
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/adopt", body, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(spy.adoptCalls) != 1 {
		t.Fatalf("adopt calls = %d, want 1", len(spy.adoptCalls))
	}
	c := spy.adoptCalls[0]
	if c.addr != "192.168.1.55" || c.fingerprint != "sha256:aa" || c.pin != "0000" || c.nodeID != "n-9b1d" || c.force {
		t.Fatalf("adopt args = %+v", c)
	}
	if got := rec.Header().Get("ETag"); got != "45" {
		t.Errorf("ETag = %q, want 45", got)
	}
}

func TestClusterAdoptRequiresIfMatch(t *testing.T) {
	spy := &clusterSpy{}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s)) // no If-Match
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/adopt",
		adoptRequest{NodeID: "n-9b1d", Addr: "1.2.3.4"}, hdr)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status = %d, want 412", rec.Code)
	}
}

func TestClusterAdoptForeign403(t *testing.T) {
	spy := &clusterSpy{adoptErr: ErrForeign}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "1"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/adopt",
		adoptRequest{NodeID: "n-9b1d", Addr: "1.2.3.4"}, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign status = %d, want 403", rec.Code)
	}
}

func TestClusterAdoptUnreachable502(t *testing.T) {
	spy := &clusterSpy{adoptErr: ErrUnreachable}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "1"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/adopt",
		adoptRequest{NodeID: "n-9b1d", Addr: "1.2.3.4"}, hdr)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable status = %d, want 502", rec.Code)
	}
}

func TestClusterTakeoverForce(t *testing.T) {
	spy := &clusterSpy{view: ConfigView{Version: 50}}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "49"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/takeover",
		adoptRequest{NodeID: "n-9b1d", Addr: "1.2.3.4", PIN: "0000"}, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("takeover status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(spy.adoptCalls) != 1 || !spy.adoptCalls[0].force {
		t.Fatalf("takeover did not pass force=true: %+v", spy.adoptCalls)
	}
}

func TestNodeForget(t *testing.T) {
	spy := &clusterSpy{view: ConfigView{
		Version: 46,
		Nodes:   []NodeView{{ID: "n-9b1d"}, {ID: "n-self"}},
		Groups:  []GroupView{{ID: "g-kitchen", MemberNodeIDs: []string{"n-9b1d"}}},
	}}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "45"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/nodes/n-9b1d/forget", nil, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("forget status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(spy.forgetCalls) != 1 || spy.forgetCalls[0] != "n-9b1d" {
		t.Fatalf("forget calls = %v", spy.forgetCalls)
	}
	var resp forgetResponse
	decodeBody(t, rec.Body.Bytes(), &resp)
	if resp.RemovedNodeID != "n-9b1d" || resp.Version != 46 {
		t.Fatalf("forget resp = %+v", resp)
	}
	if len(resp.AffectedGroups) != 1 || resp.AffectedGroups[0] != "g-kitchen" {
		t.Fatalf("affectedGroups = %v, want [g-kitchen]", resp.AffectedGroups)
	}
}

func TestNodeForgetRequiresIfMatch(t *testing.T) {
	spy := &clusterSpy{}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	rec := doJSON(t, s, http.MethodPost, "/api/v1/nodes/n-9b1d/forget", nil, hdr)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match = %d, want 412", rec.Code)
	}
}

func TestNodeForgetLastNode409(t *testing.T) {
	spy := &clusterSpy{forgetErr: ErrLastNode}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "1"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/nodes/n-x/forget", nil, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("last-node status = %d, want 409", rec.Code)
	}
}

func TestClusterLeaveCoordinated(t *testing.T) {
	spy := &clusterSpy{view: ConfigView{Version: 47}}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "46"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/leave", nil, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave status = %d (%s)", rec.Code, rec.Body.String())
	}
	if spy.leaveCalls != 1 {
		t.Fatalf("leave calls = %d, want 1", spy.leaveCalls)
	}
	var resp leaveResponse
	decodeBody(t, rec.Body.Bytes(), &resp)
	if !resp.Coordinated || resp.LeftNodeID != "n-self" {
		t.Fatalf("leave resp = %+v", resp)
	}
}

func TestClusterLeaveUnreachableFallback(t *testing.T) {
	spy := &clusterSpy{leaveErr: ErrUnreachable}
	s := newClusterServer(spy)
	hdr := cookieHdr(loginAndGetCookie(t, s))
	hdr["If-Match"] = "1"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/leave", nil, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("unreachable leave status = %d, want 200 (fallback)", rec.Code)
	}
	var resp leaveResponse
	decodeBody(t, rec.Body.Bytes(), &resp)
	if resp.Coordinated {
		t.Fatal("expected coordinated:false on unreachable cluster")
	}
}

func TestClusterRoutesRequireAdmin(t *testing.T) {
	spy := &clusterSpy{}
	s := newClusterServer(spy)
	// No session cookie: RequireAdminSession rejects with 401 before any handler.
	rec := doJSON(t, s, http.MethodPost, "/api/v1/cluster/adopt",
		adoptRequest{NodeID: "n", Addr: "1.2.3.4"}, map[string]string{"If-Match": "1"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth adopt status = %d, want 401", rec.Code)
	}
}
