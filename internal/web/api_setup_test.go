package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
)

// doJSON issues an in-process request against the server mux and returns the
// recorder. body, if non-nil, is JSON-encoded.
func doJSON(t *testing.T, s *Server, method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, r)
	return rec
}

// sessionCookie extracts the ensemble_session cookie value from a recorder, if
// the handler set one.
func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c.Value
		}
	}
	return ""
}

func TestHandleStatusUninitialized(t *testing.T) {
	s := New(Deps{NodeID: "n-test", Initialized: func() bool { return false }}, "")
	rec := doJSON(t, s, http.MethodGet, "/api/v1/status", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var v StatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Initialized {
		t.Fatalf("Initialized: got true want false (raw node)")
	}
	if v.NodeID != "n-test" {
		t.Fatalf("nodeId: got %q want n-test", v.NodeID)
	}
}

func TestHandleSetup(t *testing.T) {
	const goodPw = "correct horse battery staple"
	tests := []struct {
		name        string
		setupNil    bool
		initialized bool
		body        setupRequest
		wantCode    int
		wantCookie  bool
		wantETag    string
	}{
		{
			name:       "happy path",
			body:       setupRequest{ClusterName: "home", AdminPassword: goodPw, NodeName: "Living Room"},
			wantCode:   http.StatusOK,
			wantCookie: true,
			wantETag:   "1",
		},
		{
			name:     "empty clusterName -> 422",
			body:     setupRequest{ClusterName: "", AdminPassword: goodPw},
			wantCode: http.StatusUnprocessableEntity,
		},
		{
			name:     "weak password -> 400",
			body:     setupRequest{ClusterName: "home", AdminPassword: "short"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "empty password -> 400",
			body:     setupRequest{ClusterName: "home", AdminPassword: ""},
			wantCode: http.StatusBadRequest,
		},
		{
			name:        "already initialized -> 409",
			initialized: true,
			body:        setupRequest{ClusterName: "home", AdminPassword: goodPw},
			wantCode:    http.StatusConflict,
		},
		{
			name:     "setup unwired -> 503",
			setupNil: true,
			body:     setupRequest{ClusterName: "home", AdminPassword: goodPw},
			wantCode: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{
				NodeID:      "n-test",
				Initialized: func() bool { return tc.initialized },
			}
			if !tc.setupNil {
				deps.Setup = func(cn, pw, nn string) (SetupResult, error) {
					return SetupResult{
						ClusterName:   cn,
						CAFingerprint: "sha256:abcd",
						Created:       "2026-06-05T10:00:00Z",
						NodeID:        "n-test",
						NodeName:      nn,
						Version:       1,
					}, nil
				}
			}
			s := New(deps, "")
			rec := doJSON(t, s, http.MethodPost, "/api/v1/setup", tc.body, nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("code: got %d want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantETag != "" && rec.Header().Get("ETag") != tc.wantETag {
				t.Fatalf("ETag: got %q want %q", rec.Header().Get("ETag"), tc.wantETag)
			}
			if tc.wantCookie {
				ck := rec.Result().Cookies()
				var found *http.Cookie
				for _, c := range ck {
					if c.Name == auth.SessionCookieName {
						found = c
					}
				}
				if found == nil {
					t.Fatalf("expected %s cookie", auth.SessionCookieName)
				}
				if !found.HttpOnly || !found.Secure || found.SameSite != http.SameSiteStrictMode {
					t.Fatalf("cookie attrs: HttpOnly=%v Secure=%v SameSite=%v want all set/Strict",
						found.HttpOnly, found.Secure, found.SameSite)
				}
				var body setupResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Cluster.Name != "home" || body.Node.ID != "n-test" || body.Version != 1 {
					t.Fatalf("body: %+v", body)
				}
			}
		})
	}
}

// TestUninitializedGate asserts the [1] gate reachability table (03 §7.5): on an
// uninitialized node only GET /status and POST /setup pass; everything else under
// /api/v1 is 503 not_ready.
func TestUninitializedGate(t *testing.T) {
	s := New(Deps{
		NodeID:      "n-test",
		Initialized: func() bool { return false },
		Setup:       func(_, _, _ string) (SetupResult, error) { return SetupResult{Version: 1}, nil },
	}, "")

	tests := []struct {
		method, path string
		body         any
		want         int
	}{
		{http.MethodGet, "/api/v1/status", nil, http.StatusOK},
		{http.MethodPost, "/api/v1/setup", setupRequest{ClusterName: "home", AdminPassword: "correct horse battery staple"}, http.StatusOK},
		{http.MethodGet, "/api/v1/auth/session", nil, http.StatusServiceUnavailable},
		{http.MethodGet, "/api/v1/auth/keys", nil, http.StatusServiceUnavailable},
		{http.MethodPost, "/api/v1/auth/login", loginRequest{Password: "x"}, http.StatusServiceUnavailable},
		{http.MethodGet, "/api/v1/anything", nil, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.path, tc.body, nil)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d (body=%s)", rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
			if tc.want == http.StatusServiceUnavailable {
				var env errorEnvelope
				if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
					t.Fatalf("decode envelope: %v", err)
				}
				if env.Error.Code != codeNotReady {
					t.Fatalf("code: got %q want %q", env.Error.Code, codeNotReady)
				}
			}
		})
	}
}
