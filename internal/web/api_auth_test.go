package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/auth"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

const testPw = "correct horse battery staple"

// authTestDeps returns a Deps wired for the auth-endpoint tests: initialized,
// admin password "testPw", a mutable in-memory key store, and config version 42.
func authTestDeps(keys *[]state.APIKey, version *uint64) Deps {
	return Deps{
		NodeID:              "n-test",
		Initialized:         func() bool { return true },
		VerifyAdminPassword: func(pw string) bool { return pw == testPw },
		ConfigVersion:       func() uint64 { return *version },
		ChangeAdminPassword: func(ifMatch uint64, cur, next string) (uint64, error) {
			if ifMatch != *version {
				return 0, ErrVersionConflict
			}
			if cur != testPw {
				return 0, ErrWrongPassword
			}
			if !validPassword(next) {
				return 0, ErrWeakPassword
			}
			*version++
			return *version, nil
		},
		ListAPIKeys: func() (uint64, []state.APIKey) { return *version, *keys },
		CreateAPIKey: func(ifMatch uint64, label string) (uint64, string, string, error) {
			if ifMatch != *version {
				return 0, "", "", ErrVersionConflict
			}
			id, secret := auth.NewAPIKey()
			salt := auth.NewAPIKeySalt()
			*keys = append(*keys, state.APIKey{ID: id, Name: label, Hash: auth.HashAPIKey(secret, salt), Created: "2026-06-05T10:00:00Z"})
			*version++
			return *version, id, secret, nil
		},
		DeleteAPIKey: func(ifMatch uint64, id string) (uint64, error) {
			if ifMatch != *version {
				return 0, ErrVersionConflict
			}
			out := (*keys)[:0]
			found := false
			for _, k := range *keys {
				if k.ID == id {
					found = true
					continue
				}
				out = append(out, k)
			}
			if !found {
				return 0, ErrKeyNotFound
			}
			*keys = out
			*version++
			return *version, nil
		},
	}
}

// loginAndGetCookie performs a real login and returns the issued session cookie
// value, so admin-only endpoints can be exercised end-to-end.
func loginAndGetCookie(t *testing.T, s *Server) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/v1/auth/login", loginRequest{Password: testPw}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	v := sessionCookie(rec)
	if v == "" {
		t.Fatal("login did not set a session cookie")
	}
	return v
}

func cookieHdr(value string) map[string]string {
	return map[string]string{"Cookie": auth.SessionCookieName + "=" + value}
}

func TestLogin(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	tests := []struct {
		name       string
		uninit     bool
		password   string
		wantCode   int
		wantCookie bool
	}{
		{name: "wrong password -> 401", password: "nope", wantCode: http.StatusUnauthorized},
		{name: "right password -> 200 + cookie", password: testPw, wantCode: http.StatusOK, wantCookie: true},
		{name: "uninitialized -> 503", uninit: true, password: testPw, wantCode: http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := authTestDeps(&keys, &version)
			if tc.uninit {
				deps.Initialized = func() bool { return false }
			}
			s := New(deps, "")
			rec := doJSON(t, s, http.MethodPost, "/api/v1/auth/login", loginRequest{Password: tc.password}, nil)
			if rec.Code != tc.wantCode {
				t.Fatalf("code: got %d want %d", rec.Code, tc.wantCode)
			}
			if got := sessionCookie(rec) != ""; got != tc.wantCookie {
				t.Fatalf("cookie set=%v want %v", got, tc.wantCookie)
			}
		})
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	s := New(authTestDeps(&keys, &version), "")
	cookie := loginAndGetCookie(t, s)

	// session is valid before logout.
	if !s.sessions.Validate(cookie) {
		t.Fatal("session should be valid pre-logout")
	}

	rec := doJSON(t, s, http.MethodPost, "/api/v1/auth/logout", nil, cookieHdr(cookie))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: got %d want 204", rec.Code)
	}
	// cookie cleared (MaxAge<0).
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the cookie")
	}
	if s.sessions.Validate(cookie) {
		t.Fatal("session should be revoked after logout")
	}
}

// TestSessionAnonymousProbe pins the SPA boot-probe contract (08 §B.4): on an
// INITIALIZED node, an anonymous GET /auth/session is 200 {authenticated:false}
// (the login-form signal) — never 401 — and leaks neither the config version
// nor the node id.
func TestSessionAnonymousProbe(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	s := New(authTestDeps(&keys, &version), "")

	rec := doJSON(t, s, http.MethodGet, "/api/v1/auth/session", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous session: got %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	var resp sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Authenticated || resp.Method != "" || resp.NodeID != "" || resp.ConfigVersion != 0 {
		t.Fatalf("anonymous session must be the zero probe, got %+v", resp)
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatalf("anonymous session must not leak the config version via ETag")
	}
}

func TestSession(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	s := New(authTestDeps(&keys, &version), "")
	cookie := loginAndGetCookie(t, s)

	rec := doJSON(t, s, http.MethodGet, "/api/v1/auth/session", nil, cookieHdr(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("session: got %d want 200", rec.Code)
	}
	if rec.Header().Get("ETag") != "42" {
		t.Fatalf("ETag: got %q want 42", rec.Header().Get("ETag"))
	}
	var resp sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Authenticated || resp.Method != string(auth.MethodSession) || resp.ConfigVersion != 42 {
		t.Fatalf("session resp: %+v", resp)
	}
}

func TestPasswordChange(t *testing.T) {
	tests := []struct {
		name     string
		ifMatch  string // "" => omit header
		current  string
		next     string
		wantCode int
	}{
		{name: "no If-Match -> 412", ifMatch: "", current: testPw, next: "a brand new strong passphrase", wantCode: http.StatusPreconditionRequired},
		{name: "version mismatch -> 409", ifMatch: "7", current: testPw, next: "a brand new strong passphrase", wantCode: http.StatusConflict},
		{name: "wrong current -> 401", ifMatch: "42", current: "wrong", next: "a brand new strong passphrase", wantCode: http.StatusUnauthorized},
		{name: "weak new -> 400", ifMatch: "42", current: testPw, next: "short", wantCode: http.StatusBadRequest},
		{name: "ok -> 200 + ETag", ifMatch: "42", current: testPw, next: "a brand new strong passphrase", wantCode: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var keys []state.APIKey
			version := uint64(42)
			s := New(authTestDeps(&keys, &version), "")
			cookie := loginAndGetCookie(t, s)
			hdr := cookieHdr(cookie)
			if tc.ifMatch != "" {
				hdr["If-Match"] = tc.ifMatch
			}
			rec := doJSON(t, s, http.MethodPost, "/api/v1/auth/password",
				passwordRequest{CurrentPassword: tc.current, NewPassword: tc.next}, hdr)
			if rec.Code != tc.wantCode {
				t.Fatalf("code: got %d want %d (%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK && rec.Header().Get("ETag") != "43" {
				t.Fatalf("ETag: got %q want 43", rec.Header().Get("ETag"))
			}
		})
	}
}

func TestAPIKeysLifecycle(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	s := New(authTestDeps(&keys, &version), "")
	cookie := loginAndGetCookie(t, s)

	// create (201) returns the secret exactly once.
	hdr := cookieHdr(cookie)
	hdr["If-Match"] = "42"
	rec := doJSON(t, s, http.MethodPost, "/api/v1/auth/keys", createKeyRequest{Label: "home-assistant"}, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201 (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Version uint64 `json:"version"`
		Key     struct {
			ID, Label, Secret, CreatedAt string
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !strings.HasPrefix(created.Key.Secret, auth.APIKeyPrefix) {
		t.Fatalf("secret prefix: %q", created.Key.Secret)
	}
	if rec.Header().Get("ETag") != "43" {
		t.Fatalf("create ETag: got %q want 43", rec.Header().Get("ETag"))
	}
	keyID := created.Key.ID

	// list (200) never returns the secret.
	rec = doJSON(t, s, http.MethodGet, "/api/v1/auth/keys", nil, cookieHdr(cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.Key.Secret) {
		t.Fatal("list leaked the secret")
	}
	if strings.Contains(rec.Body.String(), "hash") {
		t.Fatal("list leaked the hash field")
	}

	// empty label -> 400.
	hdr = cookieHdr(cookie)
	hdr["If-Match"] = "43"
	rec = doJSON(t, s, http.MethodPost, "/api/v1/auth/keys", createKeyRequest{Label: ""}, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty label: got %d want 400", rec.Code)
	}

	// delete unknown -> 404.
	hdr = cookieHdr(cookie)
	hdr["If-Match"] = "43"
	rec = doJSON(t, s, http.MethodDelete, "/api/v1/auth/keys/does-not-exist", nil, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: got %d want 404 (%s)", rec.Code, rec.Body.String())
	}

	// delete known -> 200, version bumps.
	hdr = cookieHdr(cookie)
	hdr["If-Match"] = "43"
	rec = doJSON(t, s, http.MethodDelete, "/api/v1/auth/keys/"+keyID, nil, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") != "44" {
		t.Fatalf("delete ETag: got %q want 44", rec.Header().Get("ETag"))
	}
}

// TestAdminOnlyRejectsNonSession asserts [4] RequireAdminSession: a request with
// NO session (unauthenticated) is 401, and a request authenticated as an API key
// is 403 (a key authenticates but does not authorize key management, 03 §7.4).
func TestAdminOnlyAuthz(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	// seed one usable api key so we can present it as a Bearer credential.
	id, secret := auth.NewAPIKey()
	salt := auth.NewAPIKeySalt()
	keys = append(keys, state.APIKey{ID: id, Name: "k", Hash: auth.HashAPIKey(secret, salt), Created: "2026-06-05T10:00:00Z"})
	s := New(authTestDeps(&keys, &version), "")

	// no credential -> 401 (auth precedes authz).
	rec := doJSON(t, s, http.MethodGet, "/api/v1/auth/keys", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth keys list: got %d want 401", rec.Code)
	}

	// api-key credential -> authenticated, but 403 on an admin-only endpoint.
	rec = doJSON(t, s, http.MethodGet, "/api/v1/auth/keys", nil,
		map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("api-key keys list: got %d want 403 (%s)", rec.Code, rec.Body.String())
	}

	// api-key CAN read /auth/session (session OR api-key, B.4).
	rec = doJSON(t, s, http.MethodGet, "/api/v1/auth/session", nil,
		map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code != http.StatusOK {
		t.Fatalf("api-key session: got %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	var resp sessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Method != string(auth.MethodAPIKey) {
		t.Fatalf("session method: got %q want apiKey", resp.Method)
	}
}

// note: NodeID for an api-key caller is this node's id (B.4), not a cert CN.

// TestAuthBeforeIfMatch asserts the ordering from the test plan: a config-
// mutating call with no auth on an initialized node returns 401 (auth) before it
// can reach the 412 (If-Match) check.
func TestAuthBeforeIfMatch(t *testing.T) {
	var keys []state.APIKey
	version := uint64(42)
	s := New(authTestDeps(&keys, &version), "")
	// No cookie, no If-Match: must be 401 (auth), not 412.
	rec := doJSON(t, s, http.MethodPost, "/api/v1/auth/keys", createKeyRequest{Label: "x"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d want 401 (auth before If-Match)", rec.Code)
	}
}
