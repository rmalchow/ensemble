package auth

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestSessions returns a store with a controllable clock.
func newTestSessions(now *time.Time) *Sessions {
	s := NewSessions()
	s.now = func() time.Time { return *now }
	return s
}

func TestSessionLifecycle(t *testing.T) {
	s := NewSessions()
	v := s.Issue()

	if v == "" {
		t.Fatal("Issue returned empty value")
	}
	if _, err := base64.RawURLEncoding.DecodeString(v); err != nil {
		t.Errorf("Issue value is not base64url: %v", err)
	}
	if raw, _ := base64.RawURLEncoding.DecodeString(v); len(raw) != sessionIDBytes {
		t.Errorf("session value decodes to %d bytes, want %d", len(raw), sessionIDBytes)
	}

	if !s.Validate(v) {
		t.Error("Validate of issued value = false, want true")
	}
	if s.Validate("garbage") {
		t.Error("Validate of garbage = true, want false")
	}
	if s.Validate("") {
		t.Error("Validate of empty = true, want false")
	}

	s.Revoke(v)
	if s.Validate(v) {
		t.Error("Validate after Revoke = true, want false")
	}
}

// TestSessionStoresOnlyHash confirms the plaintext value is never held in the
// store (only its SHA-256), by checking the map key is the hash, not the value.
func TestSessionStoresOnlyHash(t *testing.T) {
	s := NewSessions()
	v := s.Issue()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.entries[hashValue(v)]; !found {
		t.Error("entry not keyed by SHA-256(value)")
	}
	// The plaintext value, interpreted as a raw key, must not be present.
	var asKey [32]byte
	copy(asKey[:], v)
	if _, found := s.entries[asKey]; found {
		t.Error("plaintext value found as a map key; value stored in clear")
	}
}

func TestSessionIdleTTL(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	s := newTestSessions(&now)
	v := s.Issue()

	// +11h59m with a touch: still valid, slides lastSeen.
	now = now.Add(11*time.Hour + 59*time.Minute)
	if !s.Validate(v) {
		t.Fatal("valid at +11h59m should slide and pass")
	}

	// From the slid lastSeen, +11h59m again: still valid (sliding works).
	now = now.Add(11*time.Hour + 59*time.Minute)
	if !s.Validate(v) {
		t.Fatal("valid at +11h59m after a slide should pass")
	}

	// Now let it idle past 12h without touching.
	now = now.Add(12*time.Hour + time.Minute)
	if s.Validate(v) {
		t.Error("idle past 12h should be invalid")
	}
}

func TestSessionAbsoluteTTL(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	s := newTestSessions(&now)
	v := s.Issue()

	// Touch every 6h (well inside idle) up to just under 7d: stays valid.
	for elapsed := 6 * time.Hour; elapsed < absoluteTTL; elapsed += 6 * time.Hour {
		now = now.Add(6 * time.Hour)
		if !s.Validate(v) {
			t.Fatalf("continuous touching should keep session valid before 7d (elapsed %v)", elapsed)
		}
	}

	// Past the absolute 7d cap, even continuous touching expires it.
	now = now.Add(8 * time.Hour) // total > 7d
	if s.Validate(v) {
		t.Error("session past 7d absolute cap should be invalid despite touching")
	}
}

func TestSessionSweep(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	s := newTestSessions(&now)
	live := s.Issue()
	stale := s.Issue()

	// Keep live fresh by touching it inside the idle window, then advance past
	// the idle TTL so only the un-touched stale entry expires.
	now = now.Add(8 * time.Hour)
	if !s.Validate(live) { // slides live's lastSeen to now (+8h)
		t.Fatal("live should still be valid at +8h")
	}
	now = now.Add(8 * time.Hour) // +16h total: stale idle 16h (dead), live idle 8h (alive)

	if s.Len() != 2 {
		t.Fatalf("Len before sweep = %d, want 2", s.Len())
	}
	s.Sweep()
	if s.Len() != 1 {
		t.Errorf("Len after sweep = %d, want 1 (stale dropped)", s.Len())
	}
	if s.Validate(stale) {
		t.Error("swept stale session still validates")
	}
	if !s.Validate(live) {
		t.Error("live session dropped by sweep")
	}
}

func TestSetSessionCookieAttrs(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "abc")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != SessionCookieName || c.Value != "abc" {
		t.Errorf("cookie name/value = %q/%q", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("cookie not HttpOnly")
	}
	if !c.Secure {
		t.Error("cookie not Secure")
	}
	if c.SameSite != 3 /* http.SameSiteStrictMode */ {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want > 0 (idle TTL)", c.MaxAge)
	}
	// Header text check for SameSite=Strict (defensive against enum drift).
	if hdr := rec.Header().Get("Set-Cookie"); !strings.Contains(hdr, "SameSite=Strict") {
		t.Errorf("Set-Cookie %q missing SameSite=Strict", hdr)
	}
}

func TestClearSessionCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	ClearSessionCookie(rec)
	c := rec.Result().Cookies()[0]
	if c.MaxAge != -1 {
		t.Errorf("ClearSessionCookie MaxAge = %d, want -1", c.MaxAge)
	}
	if c.Name != SessionCookieName {
		t.Errorf("name = %q", c.Name)
	}
}
