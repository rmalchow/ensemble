package web

import (
	"net/http"
	"testing"
)

// api_media_test.go drives the 08 §F handlers over a stub Deps (httptest), per
// P4.9 §7.3: F.1 list shape, F.2/F.3/F.4 If-Match → 412/409/200 + ETag echo, and
// F.3 select-and-play one-shot.

// mediaSpy records the media/transport closure calls and scripts their results.
type mediaSpy struct {
	listFiles  []MediaFile
	listErr    error
	selectErr  error
	playErr    error
	stopErr    error
	view       ConfigView // returned by the mutating closures
	lastSelect [3]any     // groupID, file, loop
	lastPlay   [3]any
	stopCalls  int
}

func newMediaServer(spy *mediaSpy) *Server {
	return New(Deps{
		NodeID:              "n-self",
		Initialized:         func() bool { return true },
		VerifyAdminPassword: func(pw string) bool { return pw == testPw },
		ConfigVersion:       func() uint64 { return spy.view.Version },
		State:               func() ConfigView { return spy.view },
		ListMedia:           func(string, string) ([]MediaFile, []string, error) { return spy.listFiles, nil, spy.listErr },
		SelectMedia: func(g, f string, loop bool, _ string, _ uint64) (ConfigView, error) {
			spy.lastSelect = [3]any{g, f, loop}
			return spy.view, spy.selectErr
		},
		Play: func(g, f string, loop bool, _ string, _ uint64) (ConfigView, error) {
			spy.lastPlay = [3]any{g, f, loop}
			return spy.view, spy.playErr
		},
		Stop: func(g string, _ uint64) (ConfigView, error) {
			spy.stopCalls++
			return spy.view, spy.stopErr
		},
	}, "")
}

func TestListMediaHandler(t *testing.T) {
	spy := &mediaSpy{listFiles: []MediaFile{{File: "a.mp3", SizeBytes: 100, SampleRate: 44100}}}
	s := newMediaServer(spy)
	cookie := loginAndGetCookie(t, s)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/media", nil, cookieHdr(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp mediaListResponse
	decodeBody(t, rec.Body.Bytes(), &resp)
	if resp.NodeID != "n-self" || len(resp.Files) != 1 || resp.Files[0].File != "a.mp3" {
		t.Fatalf("list resp = %+v", resp)
	}
}

func TestSelectMediaHandler(t *testing.T) {
	tests := []struct {
		name     string
		ifMatch  string // "" => omit header
		closErr  error
		wantCode int
	}{
		{"no If-Match => 412", "", nil, http.StatusPreconditionRequired},
		{"non-mp3 => 422", "5", ErrNotMP3, http.StatusUnprocessableEntity},
		{"missing on master => 404", "5", ErrMissingOnMaster, http.StatusNotFound},
		{"version conflict => 409", "5", ErrVersionConflict, http.StatusConflict},
		{"ok => 200 + ETag", "5", nil, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := &mediaSpy{view: ConfigView{Version: 6, Groups: []GroupView{{ID: "g1", Media: Media{File: "x.mp3"}}}}, selectErr: tc.closErr}
			s := newMediaServer(spy)
			hdr := cookieHdr(loginAndGetCookie(t, s))
			if tc.ifMatch != "" {
				hdr["If-Match"] = tc.ifMatch
			}
			rec := doJSON(t, s, http.MethodPost, "/api/v1/groups/g1/media", mediaRequest{File: "x.mp3"}, hdr)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				if got := rec.Header().Get("ETag"); got != "6" {
					t.Errorf("ETag = %q, want 6", got)
				}
			}
		})
	}
}

func TestPlayHandler(t *testing.T) {
	t.Run("no media => 409 conflict", func(t *testing.T) {
		spy := &mediaSpy{playErr: ErrNoMedia, view: ConfigView{Version: 2}}
		s := newMediaServer(spy)
		hdr := cookieHdr(loginAndGetCookie(t, s))
		hdr["If-Match"] = "1"
		rec := doJSON(t, s, http.MethodPost, "/api/v1/groups/g1/play", playRequest{}, hdr)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("master unreachable => 502", func(t *testing.T) {
		spy := &mediaSpy{playErr: ErrUnreachable, view: ConfigView{Version: 2}}
		s := newMediaServer(spy)
		hdr := cookieHdr(loginAndGetCookie(t, s))
		hdr["If-Match"] = "1"
		rec := doJSON(t, s, http.MethodPost, "/api/v1/groups/g1/play", playRequest{}, hdr)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("select-and-play one-shot => 200, file threaded through", func(t *testing.T) {
		spy := &mediaSpy{view: ConfigView{Version: 3, Groups: []GroupView{{ID: "g1"}}}}
		s := newMediaServer(spy)
		hdr := cookieHdr(loginAndGetCookie(t, s))
		hdr["If-Match"] = "2"
		rec := doJSON(t, s, http.MethodPost, "/api/v1/groups/g1/play", playRequest{File: "y.mp3", Loop: true}, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if spy.lastPlay[1] != "y.mp3" || spy.lastPlay[2] != true {
			t.Fatalf("Play args = %v, want [g1 y.mp3 true]", spy.lastPlay)
		}
		if got := rec.Header().Get("ETag"); got != "3" {
			t.Errorf("ETag = %q, want 3", got)
		}
	})
}

func TestStopHandler(t *testing.T) {
	t.Run("no If-Match => 412", func(t *testing.T) {
		spy := &mediaSpy{view: ConfigView{Version: 2}}
		s := newMediaServer(spy)
		rec := doJSON(t, s, http.MethodPost, "/api/v1/groups/g1/stop", nil, cookieHdr(loginAndGetCookie(t, s)))
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("status = %d, want 412 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("ok => 200 + Stop invoked", func(t *testing.T) {
		spy := &mediaSpy{view: ConfigView{Version: 4, Groups: []GroupView{{ID: "g1"}}}}
		s := newMediaServer(spy)
		hdr := cookieHdr(loginAndGetCookie(t, s))
		hdr["If-Match"] = "3"
		rec := doJSON(t, s, http.MethodPost, "/api/v1/groups/g1/stop", nil, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		if spy.stopCalls != 1 {
			t.Fatalf("Stop calls = %d, want 1", spy.stopCalls)
		}
	})
}
