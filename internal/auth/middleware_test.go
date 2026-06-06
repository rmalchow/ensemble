package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler records that it ran and echoes the auth method from context.
func okHandler(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		m, _ := MethodFromContext(r.Context())
		w.Header().Set("X-Method", string(m))
		w.WriteHeader(http.StatusOK)
	})
}

func decodeEnvelope(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var e errorEnvelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("response not an error envelope: %v (%s)", err, body)
	}
	return e
}

func TestAuthenticateUninitializedGate(t *testing.T) {
	d := Deps{Initialized: func() bool { return false }}

	// Non-public endpoint => 503 not_ready.
	t.Run("closed endpoint 503", func(t *testing.T) {
		var ran bool
		req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
		rec := httptest.NewRecorder()
		d.Authenticate(okHandler(&ran)).ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if ran {
			t.Error("handler ran on a gated request")
		}
		if e := decodeEnvelope(t, rec.Body.Bytes()); e.Error.Code != codeNotReady {
			t.Errorf("code = %q, want %q", e.Error.Code, codeNotReady)
		}
	})

	// Public setup endpoint => passes even when uninitialized.
	t.Run("setup passes", func(t *testing.T) {
		var ran bool
		req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", nil)
		rec := httptest.NewRecorder()
		d.Authenticate(okHandler(&ran)).ServeHTTP(rec, req)
		if !ran || rec.Code != http.StatusOK {
			t.Errorf("setup gated: ran=%v status=%d", ran, rec.Code)
		}
	})
}

func TestAuthenticateNodePath(t *testing.T) {
	// NodeAuth ok; Sessions/VerifyKey would panic if consulted (they must not be).
	d := Deps{
		Initialized: func() bool { return true },
		NodeAuth:    func(*http.Request) (string, bool) { return "node-7", true },
		VerifyKey:   func(string) (string, bool) { panic("human path consulted on a node request") },
	}
	var ran bool
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	rec := httptest.NewRecorder()

	captured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		if m, _ := MethodFromContext(r.Context()); m != MethodNode {
			t.Errorf("method = %q, want node", m)
		}
		if id, _ := NodeIDFromContext(r.Context()); id != "node-7" {
			t.Errorf("node id = %q, want node-7", id)
		}
	})
	d.Authenticate(captured).ServeHTTP(rec, req)
	if !ran {
		t.Error("node-authenticated handler did not run")
	}
}

func TestAuthenticateHumanPath(t *testing.T) {
	sessions := NewSessions()
	good := sessions.Issue()
	d := Deps{
		Initialized: func() bool { return true },
		Sessions:    sessions,
		VerifyKey: func(pt string) (string, bool) {
			if pt == "valid-key" {
				return "key-1", true
			}
			return "", false
		},
	}

	tests := []struct {
		name       string
		setup      func(*http.Request)
		wantStatus int
		wantMethod AuthMethod
	}{
		{
			name:       "valid session",
			setup:      func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: good}) },
			wantStatus: http.StatusOK,
			wantMethod: MethodSession,
		},
		{
			name:       "valid bearer key",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer valid-key") },
			wantStatus: http.StatusOK,
			wantMethod: MethodAPIKey,
		},
		{
			name:       "no credential",
			setup:      func(*http.Request) {},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bad session cookie",
			setup:      func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "nope"}) },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong bearer key",
			setup:      func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") },
			wantStatus: http.StatusUnauthorized,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
			tc.setup(req)
			rec := httptest.NewRecorder()
			d.Authenticate(okHandler(&ran)).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				if got := rec.Header().Get("X-Method"); got != string(tc.wantMethod) {
					t.Errorf("method = %q, want %q", got, tc.wantMethod)
				}
			} else {
				if e := decodeEnvelope(t, rec.Body.Bytes()); e.Error.Code != codeUnauthenticated {
					t.Errorf("code = %q, want %q", e.Error.Code, codeUnauthenticated)
				}
			}
		})
	}
}

func TestRequireAdminSession(t *testing.T) {
	inner := func(method AuthMethod, hasMethod bool) *httptest.ResponseRecorder {
		var ran bool
		req := httptest.NewRequest(http.MethodPost, "/api/v1/keys", nil)
		if hasMethod {
			req = req.WithContext(context.WithValue(req.Context(), ctxMethod, method))
		}
		rec := httptest.NewRecorder()
		RequireAdminSession(okHandler(&ran)).ServeHTTP(rec, req)
		return rec
	}

	if rec := inner(MethodSession, true); rec.Code != http.StatusOK {
		t.Errorf("session: status = %d, want 200", rec.Code)
	}
	if rec := inner(MethodNode, true); rec.Code != http.StatusForbidden {
		t.Errorf("node: status = %d, want 403", rec.Code)
	} else if e := decodeEnvelope(t, rec.Body.Bytes()); e.Error.Code != codeForbidden {
		t.Errorf("node: code = %q, want %q", e.Error.Code, codeForbidden)
	}
	if rec := inner(MethodAPIKey, true); rec.Code != http.StatusForbidden {
		t.Errorf("apiKey: status = %d, want 403", rec.Code)
	}
	if rec := inner("", false); rec.Code != http.StatusForbidden {
		t.Errorf("no method: status = %d, want 403", rec.Code)
	}
}

func TestChainComposition(t *testing.T) {
	var order []string
	mark := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	d := Deps{Initialized: func() bool { return true }, Sessions: NewSessions()}
	sess := d.Sessions.Issue()

	var ran bool
	h := Chain(d, mark("a"), mark("b"))(okHandler(&ran))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !ran || rec.Code != http.StatusOK {
		t.Fatalf("chain did not reach handler: ran=%v status=%d", ran, rec.Code)
	}
	// recover+auth run before the per-endpoint mw, then a, then b.
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Errorf("mw order = %v, want [a b]", order)
	}
}

func TestChainRecoversPanic(t *testing.T) {
	d := Deps{Initialized: func() bool { return true }, Sessions: NewSessions()}
	sess := d.Sessions.Issue()

	panicker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := Chain(d)(panicker)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess})
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req) // must not crash the test process
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if e := decodeEnvelope(t, rec.Body.Bytes()); e.Error.Code != codeInternal {
		t.Errorf("code = %q, want %q", e.Error.Code, codeInternal)
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusTeapot, "custom_code", "a message")
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	e := decodeEnvelope(t, rec.Body.Bytes())
	if e.Error.Code != "custom_code" || e.Error.Message != "a message" {
		t.Errorf("envelope = %+v", e.Error)
	}
}
