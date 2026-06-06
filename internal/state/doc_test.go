package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaRoundTrip loads the 07 §6 worked-example doc, unmarshals into
// ConfigDoc, re-marshals, and asserts the schema captures every canonical field.
func TestSchemaRoundTrip(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "config_v87.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var d ConfigDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	if d.Version != 87 {
		t.Errorf("Version = %d, want 87", d.Version)
	}
	if d.Cluster.Name != "Home" {
		t.Errorf("Cluster.Name = %q, want Home", d.Cluster.Name)
	}
	if d.Secrets.CAKeyPEM == "" {
		t.Error("Secrets.CAKeyPEM is empty, want non-empty (D18 plaintext CA key)")
	}
	if d.Secrets.SharedSecret == "" {
		t.Error("Secrets.SharedSecret is empty")
	}
	if got := len(d.Nodes); got != 4 {
		t.Fatalf("len(Nodes) = %d, want 4", got)
	}
	if got := len(d.Groups); got != 2 {
		t.Fatalf("len(Groups) = %d, want 2", got)
	}
	if got := len(d.Revoked.Entries); got != 1 {
		t.Fatalf("len(Revoked.Entries) = %d, want 1", got)
	}
	if d.UpdatedBy != "node-living-room-8f2a" {
		t.Errorf("UpdatedBy = %q", d.UpdatedBy)
	}

	groups := map[string]GroupRecord{}
	for _, g := range d.Groups {
		groups[g.Name] = g
	}
	down := groups["downstairs"]
	if down.Profile.Codec != "pcm" {
		t.Errorf("downstairs.Profile.Codec = %q, want pcm", down.Profile.Codec)
	}
	if down.Profile.FECK != 8 {
		t.Errorf("downstairs.Profile.FECK = %d, want 8 (A.12)", down.Profile.FECK)
	}
	if down.Profile.Interleave != 4 {
		t.Errorf("downstairs.Profile.Interleave = %d, want 4 (A.12)", down.Profile.Interleave)
	}
	if down.Profile.Rate != 48000 {
		t.Errorf("downstairs.Profile.Rate = %d, want 48000", down.Profile.Rate)
	}
	if down.Profile.FramesPerChunk != 480 {
		t.Errorf("downstairs.Profile.FramesPerChunk = %d, want 480", down.Profile.FramesPerChunk)
	}
	up := groups["upstairs"]
	if up.Profile.Codec != "opus" {
		t.Errorf("upstairs.Profile.Codec = %q, want opus", up.Profile.Codec)
	}
	if up.Profile.FEC != "none" {
		t.Errorf("upstairs.Profile.FEC = %q, want none", up.Profile.FEC)
	}

	// A round-trip must succeed (proves no field is unmappable).
	if _, err := json.MarshalIndent(d, "", "  "); err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
}

// TestCanonicalFieldOrder asserts the top-level marshal key order and the
// omitempty behaviour of an empty ConfigDoc{}.
func TestCanonicalFieldOrder(t *testing.T) {
	b, err := json.Marshal(ConfigDoc{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	got := string(b)

	wantOrder := []string{
		`"version"`, `"cluster"`, `"secrets"`, `"auth"`,
		`"nodes"`, `"groups"`, `"revoked"`, `"updatedBy"`,
	}
	prev := -1
	for _, key := range wantOrder {
		idx := strings.Index(got, key)
		if idx < 0 {
			t.Errorf("key %s missing from %s", key, got)
			continue
		}
		if idx <= prev {
			t.Errorf("key %s out of canonical order in %s", key, got)
		}
		prev = idx
	}

	// omitempty fields must be absent on a zero doc.
	for _, key := range []string{
		`"updatedAt"`, `"pinHash"`, `"masterHint"`,
		`"lastUsed"`, `"lastSeen"`, `"sharedSecret"`,
	} {
		if strings.Contains(got, key) {
			t.Errorf("zero-value field %s should be omitted, got %s", key, got)
		}
	}
}

// TestCloneIsolation proves cloneConfigDoc deep-copies every slice so a caller
// mutating the result cannot race the original.
func TestCloneIsolation(t *testing.T) {
	orig := ConfigDoc{
		Version: 5,
		Auth: AuthConfig{
			APIKeys: []APIKey{{ID: "ak1", Name: "n1"}},
		},
		Nodes: []NodeRecord{
			{
				ID:    "n1",
				Addrs: []string{"10.0.0.1"},
				Caps: Capabilities{
					Sinks:        []string{"alsa"},
					EncodeCodecs: []string{"pcm"},
					DecodeCodecs: []string{"pcm"},
					FEC:          []string{"none"},
				},
			},
		},
		Groups: []GroupRecord{
			{ID: "g1", MemberNodeIDs: []string{"n1"}},
		},
		Revoked: RevokedSet{Entries: []RevokedCert{{Fingerprint: "ff"}}},
	}
	clone := cloneConfigDoc(orig)

	clone.Nodes[0].Addrs[0] = "MUTATED"
	clone.Nodes[0].Caps.Sinks[0] = "MUTATED"
	clone.Nodes[0].Caps.EncodeCodecs[0] = "MUTATED"
	clone.Nodes[0].Caps.DecodeCodecs[0] = "MUTATED"
	clone.Nodes[0].Caps.FEC[0] = "MUTATED"
	clone.Groups[0].MemberNodeIDs[0] = "MUTATED"
	clone.Auth.APIKeys[0].ID = "MUTATED"
	clone.Revoked.Entries[0].Fingerprint = "MUTATED"

	if orig.Nodes[0].Addrs[0] != "10.0.0.1" {
		t.Error("Nodes[0].Addrs aliased")
	}
	if orig.Nodes[0].Caps.Sinks[0] != "alsa" {
		t.Error("Caps.Sinks aliased")
	}
	if orig.Nodes[0].Caps.EncodeCodecs[0] != "pcm" {
		t.Error("Caps.EncodeCodecs aliased")
	}
	if orig.Nodes[0].Caps.DecodeCodecs[0] != "pcm" {
		t.Error("Caps.DecodeCodecs aliased")
	}
	if orig.Nodes[0].Caps.FEC[0] != "none" {
		t.Error("Caps.FEC aliased")
	}
	if orig.Groups[0].MemberNodeIDs[0] != "n1" {
		t.Error("Groups[0].MemberNodeIDs aliased")
	}
	if orig.Auth.APIKeys[0].ID != "ak1" {
		t.Error("Auth.APIKeys aliased")
	}
	if orig.Revoked.Entries[0].Fingerprint != "ff" {
		t.Error("Revoked.Entries aliased")
	}
}

// TestCloneNilPreserved asserts nil slices stay nil (not converted to []) so
// JSON round-trips are byte-stable.
func TestCloneNilPreserved(t *testing.T) {
	clone := cloneConfigDoc(ConfigDoc{Version: 1})
	if clone.Nodes != nil {
		t.Error("nil Nodes became non-nil")
	}
	if clone.Groups != nil {
		t.Error("nil Groups became non-nil")
	}
	if clone.Auth.APIKeys != nil {
		t.Error("nil APIKeys became non-nil")
	}
	if clone.Revoked.Entries != nil {
		t.Error("nil Revoked.Entries became non-nil")
	}
}
