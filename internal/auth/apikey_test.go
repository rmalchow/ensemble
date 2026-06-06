package auth

import (
	"strings"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

func TestNewAPIKey(t *testing.T) {
	id1, pt1 := NewAPIKey()
	id2, pt2 := NewAPIKey()

	for _, pt := range []string{pt1, pt2} {
		if !strings.HasPrefix(pt, APIKeyPrefix) {
			t.Errorf("plaintext %q missing %q prefix", pt, APIKeyPrefix)
		}
	}
	if id1 == "" || id2 == "" {
		t.Fatal("empty key id")
	}
	if id1 == id2 {
		t.Error("two minted keys share an id")
	}
	if pt1 == pt2 {
		t.Error("two minted keys share a plaintext")
	}
}

func TestHashAPIKeyDeterminismAndSalt(t *testing.T) {
	const pt = "ek_live_secret"
	saltA := "salt-aaaa-16byte"
	saltB := "salt-bbbb-16byte"

	if HashAPIKey(pt, saltA) != HashAPIKey(pt, saltA) {
		t.Error("HashAPIKey not deterministic for same salt+plaintext")
	}
	if HashAPIKey(pt, saltA) == HashAPIKey(pt, saltB) {
		t.Error("HashAPIKey identical across different salts")
	}
	// Stored form is "<saltHex>$<hashHex>".
	h := HashAPIKey(pt, saltA)
	if !strings.Contains(h, "$") {
		t.Errorf("stored hash %q missing salt separator", h)
	}
}

func TestVerifyAPIKey(t *testing.T) {
	id1, pt1 := NewAPIKey()
	id2, pt2 := NewAPIKey()

	mk := func(id, pt string) state.APIKey {
		return state.APIKey{ID: id, Hash: HashAPIKey(pt, NewAPIKeySalt())}
	}
	keys := []state.APIKey{mk(id1, pt1), mk(id2, pt2)}

	tests := []struct {
		name      string
		plaintext string
		keys      []state.APIKey
		wantID    string
		wantOK    bool
	}{
		{"first key", pt1, keys, id1, true},
		{"second key", pt2, keys, id2, true},
		{"unknown key", "ek_live_nope", keys, "", false},
		{"empty key", "", keys, "", false},
		{"nil key list", pt1, nil, "", false},
		{"empty key list", pt1, []state.APIKey{}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := VerifyAPIKey(tc.plaintext, tc.keys)
			if ok != tc.wantOK || id != tc.wantID {
				t.Errorf("VerifyAPIKey = (%q,%v), want (%q,%v)", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestVerifyAPIKeyMalformedStored confirms a corrupt stored hash never matches
// and never panics.
func TestVerifyAPIKeyMalformedStored(t *testing.T) {
	id, pt := NewAPIKey()
	bad := []state.APIKey{
		{ID: "nosep", Hash: "deadbeef"},                 // no '$'
		{ID: "badsalt", Hash: "zz$deadbeef"},            // bad salt hex
		{ID: "badhash", Hash: "aa$zz"},                  // bad hash hex
		{ID: "shorthash", Hash: "aa$dead"},              // wrong digest length
		{ID: id, Hash: HashAPIKey(pt, NewAPIKeySalt())}, // one good key at the end
	}
	gotID, ok := VerifyAPIKey(pt, bad)
	if !ok || gotID != id {
		t.Errorf("VerifyAPIKey across malformed+valid = (%q,%v), want (%q,true)", gotID, ok, id)
	}
	if _, ok := VerifyAPIKey("ek_live_other", bad[:4]); ok {
		t.Error("VerifyAPIKey matched a malformed-only set")
	}
}

func TestHashAPIKeyRoundTripWithGeneratedSalt(t *testing.T) {
	id, pt := NewAPIKey()
	salt := NewAPIKeySalt()
	if salt == "" {
		t.Fatal("empty salt from NewAPIKeySalt")
	}
	rec := state.APIKey{ID: id, Hash: HashAPIKey(pt, salt)}
	if gotID, ok := VerifyAPIKey(pt, []state.APIKey{rec}); !ok || gotID != id {
		t.Errorf("round-trip verify = (%q,%v), want (%q,true)", gotID, ok, id)
	}
	if _, ok := VerifyAPIKey(pt+"x", []state.APIKey{rec}); ok {
		t.Error("tampered plaintext verified")
	}
}
