package web

import (
	"net/http"
	"testing"
)

// api_groupstatus_test.go drives G.2 over a stub Deps (P4.9 §7.3): the member
// projection (field names per 08 §G.2) and the 503 not_ready mapping.

func newStatusServer(st GroupStatus, err error) *Server {
	return New(Deps{
		NodeID:              "n-self",
		Initialized:         func() bool { return true },
		VerifyAdminPassword: func(pw string) bool { return pw == testPw },
		GroupStatus:         func(string) (GroupStatus, error) { return st, err },
	}, "")
}

func TestGroupStatusProjection(t *testing.T) {
	st := GroupStatus{
		GroupID:      "g1",
		MasterNodeID: "n-self",
		StreamGen:    7,
		Playing:      true,
		Profile:      Profile{Codec: "pcm", FEC: "none", Rate: 48000, FramesPerChunk: 480},
		Members: []MemberStatus{
			{NodeID: "n-self", SyncErrorUs: 120, OffsetUs: -340, DriftRatio: 1.00002, Underruns: 0, ClockQuality: "good", Online: true},
			{NodeID: "n-b", Online: false},
		},
	}
	s := newStatusServer(st, nil)
	cookie := loginAndGetCookie(t, s)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/groups/g1/status", nil, cookieHdr(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var got GroupStatus
	decodeBody(t, rec.Body.Bytes(), &got)
	if got.MasterNodeID != "n-self" || got.StreamGen != 7 || !got.Playing {
		t.Fatalf("group fields = %+v", got)
	}
	if len(got.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(got.Members))
	}
	// Per-member: a down member is Online=false, NOT a top-level error.
	var down MemberStatus
	for _, m := range got.Members {
		if m.NodeID == "n-b" {
			down = m
		}
	}
	if down.Online {
		t.Fatal("n-b should project Online=false")
	}
	if got.Members[0].ClockQuality != "good" || got.Members[0].SyncErrorUs != 120 {
		t.Fatalf("self member projection lost fields: %+v", got.Members[0])
	}
}

func TestGroupStatusNotReady(t *testing.T) {
	s := newStatusServer(GroupStatus{}, ErrGroupNotReady)
	cookie := loginAndGetCookie(t, s)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/groups/g1/status", nil, cookieHdr(cookie))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}

func TestGroupStatusNotFound(t *testing.T) {
	s := newStatusServer(GroupStatus{}, ErrNotMember)
	cookie := loginAndGetCookie(t, s)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/groups/nope/status", nil, cookieHdr(cookie))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}
