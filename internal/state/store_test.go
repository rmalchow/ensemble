package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fired reports whether the Changed channel has a pending signal (non-blocking).
func fired(s *Store) bool {
	select {
	case <-s.Changed():
		return true
	default:
		return false
	}
}

func TestApplyHappyPath(t *testing.T) {
	s := New("self")
	out, err := s.Apply(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "Home"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Version != 1 {
		t.Errorf("Version = %d, want 1", out.Version)
	}
	if out.UpdatedBy != "self" {
		t.Errorf("UpdatedBy = %q, want self", out.UpdatedBy)
	}
	if out.UpdatedAt == "" {
		t.Error("UpdatedAt empty, want RFC3339 timestamp")
	}
	if !fired(s) {
		t.Error("Changed did not fire after Apply")
	}
}

func TestApplyConflict(t *testing.T) {
	s := New("self")
	if _, err := s.Apply(ConfigDoc{Version: 0}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	<-s.Changed() // drain

	// Now current version is 1; submit a stale 0.
	cur, err := s.Apply(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "stale"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if cur.Version != 1 {
		t.Errorf("returned doc Version = %d, want current 1", cur.Version)
	}
	if cur.Cluster.Name == "stale" {
		t.Error("stale update leaked into store")
	}
	if fired(s) {
		t.Error("Changed fired on conflict, want no signal")
	}
}

func TestApplyThenGetIsolation(t *testing.T) {
	s := New("self")
	if _, err := s.Apply(ConfigDoc{Nodes: []NodeRecord{{ID: "n1", Addrs: []string{"a"}}}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	g := s.Get()
	g.Nodes[0].Addrs[0] = "MUTATED"
	g.Nodes[0].ID = "MUTATED"

	fresh := s.Get()
	if fresh.Nodes[0].ID != "n1" || fresh.Nodes[0].Addrs[0] != "a" {
		t.Error("mutating Get() result affected the store")
	}
}

func TestMergeHigherVersionWins(t *testing.T) {
	s := New("self")
	s.Merge(ConfigDoc{Version: 10, Cluster: ClusterInfo{Name: "remote"}}, "other")
	if got := s.Get(); got.Version != 10 || got.Cluster.Name != "remote" {
		t.Errorf("got Version=%d Name=%q, want 10/remote", got.Version, got.Cluster.Name)
	}
	if !fired(s) {
		t.Error("Changed did not fire on adopting higher version")
	}
}

func TestMergeLowerLoses(t *testing.T) {
	s := New("self")
	if _, err := s.Apply(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "local"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	<-s.Changed()
	// local is now Version 1. remote at Version 0 must lose.
	s.Merge(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "remote"}}, "other")
	if got := s.Get(); got.Version != 1 || got.Cluster.Name != "local" {
		t.Errorf("doc changed on losing merge: %+v", got)
	}
	if fired(s) {
		t.Error("Changed fired though nothing advanced")
	}
}

func TestMergeTiebreakByID(t *testing.T) {
	tests := []struct {
		name     string
		self     string
		remoteID string
		wantTake bool
	}{
		{"remote id greater wins", "aaa", "zzz", true},
		{"remote id smaller loses", "zzz", "aaa", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.self)
			s.Merge(ConfigDoc{Version: 5, Cluster: ClusterInfo{Name: "L"}}, tc.self)
			<-s.Changed() // drain (5 > 0 always took)
			s.Merge(ConfigDoc{Version: 5, Cluster: ClusterInfo{Name: "R"}}, tc.remoteID)
			got := s.Get()
			if tc.wantTake && got.Cluster.Name != "R" {
				t.Errorf("expected remote taken, got %q", got.Cluster.Name)
			}
			if !tc.wantTake && got.Cluster.Name != "L" {
				t.Errorf("expected local kept, got %q", got.Cluster.Name)
			}
		})
	}
}

func TestMergeVersionZeroNeverTiebreaks(t *testing.T) {
	s := New("aaa")
	s.Merge(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "R"}}, "zzz")
	if got := s.Get(); got.Cluster.Name == "R" {
		t.Error("Version 0 was taken via tiebreak, want no take")
	}
	if fired(s) {
		t.Error("Changed fired on Version 0 no-take")
	}
}

func TestMergeRevokedUnionOnLosingDoc(t *testing.T) {
	s := New("self")
	if _, err := s.Apply(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "local"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	<-s.Changed()
	// Remote loses the LWW doc (Version 0 < local 1) but carries a new revoked cert.
	s.Merge(ConfigDoc{
		Version: 0,
		Revoked: RevokedSet{Entries: []RevokedCert{{Fingerprint: "deadbeef", Reason: "forget"}}},
	}, "other")

	got := s.Get()
	if got.Cluster.Name != "local" {
		t.Errorf("losing doc body was adopted: %q", got.Cluster.Name)
	}
	if len(got.Revoked.Entries) != 1 || got.Revoked.Entries[0].Fingerprint != "deadbeef" {
		t.Errorf("revoked cert not unioned from losing remote: %+v", got.Revoked)
	}
	if !fired(s) {
		t.Error("Changed did not fire though Revoked grew")
	}
}

func TestMergeRevokedDedup(t *testing.T) {
	s := New("self")
	if _, err := s.Apply(ConfigDoc{
		Version: 0,
		Revoked: RevokedSet{Entries: []RevokedCert{
			{Fingerprint: "aa", Reason: "forget"},
			{Fingerprint: "bb", Reason: "takeover"},
		}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	<-s.Changed()
	// Remote overlaps on "bb", adds "cc". Version 0 loses the doc body.
	s.Merge(ConfigDoc{
		Version: 0,
		Revoked: RevokedSet{Entries: []RevokedCert{
			{Fingerprint: "bb", Reason: "rotate"}, // duplicate fingerprint, resident wins metadata
			{Fingerprint: "cc", Reason: "forget"},
		}},
	}, "other")

	got := s.Get()
	if len(got.Revoked.Entries) != 3 {
		t.Fatalf("union size = %d, want 3 (aa,bb,cc)", len(got.Revoked.Entries))
	}
	// resident metadata wins for the overlapping fingerprint.
	for _, e := range got.Revoked.Entries {
		if e.Fingerprint == "bb" && e.Reason != "takeover" {
			t.Errorf("bb reason = %q, want resident takeover", e.Reason)
		}
	}
}

func TestMergeResurrectionGuard(t *testing.T) {
	s := New("self")
	// Local forgets node X: removes the record, revokes its cert, Version 50.
	if _, err := s.Apply(ConfigDoc{
		Version: 0,
		Nodes:   []NodeRecord{{ID: "keep"}},
		Revoked: RevokedSet{Entries: []RevokedCert{{Fingerprint: "xfp", NodeID: "X", Reason: "forget"}}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	<-s.Changed()
	cur := s.Get()
	// Bump local to Version 50 via repeated Apply is noisy; instead Merge a higher
	// local-authored version directly is not possible. Simulate by Applying again
	// to reach a known version, then Merge a *higher* stale remote that still lists X.
	cur.Version = 1
	if _, err := s.Apply(cur); err != nil { // -> Version 2
		t.Fatalf("apply2: %v", err)
	}
	<-s.Changed()

	// Remote at higher Version still lists X as a node (stale view) and lacks the revocation.
	s.Merge(ConfigDoc{
		Version: 99,
		Nodes:   []NodeRecord{{ID: "keep"}, {ID: "X"}},
	}, "other")

	got := s.Get()
	// LWW: X's NodeRecord may be back...
	hasX := false
	for _, n := range got.Nodes {
		if n.ID == "X" {
			hasX = true
		}
	}
	if !hasX {
		t.Error("expected LWW to readopt remote node list (incl X)")
	}
	// ...BUT the revocation must survive (grow-only union) — the §4.1 guard.
	hasRevoked := false
	for _, e := range got.Revoked.Entries {
		if e.Fingerprint == "xfp" {
			hasRevoked = true
		}
	}
	if !hasRevoked {
		t.Error("revocation of X lost after stale higher-version merge (resurrection guard failed)")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	s := Load("self", path)
	if got := s.Get(); got.Version != 0 {
		t.Errorf("empty dir Load gave Version %d, want 0", got.Version)
	}
	if _, err := s.Apply(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "persisted"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	s2 := Load("self", path)
	got := s2.Get()
	if got.Version != 1 || got.Cluster.Name != "persisted" {
		t.Errorf("reloaded doc = Version %d Name %q, want 1/persisted", got.Version, got.Cluster.Name)
	}
}

func TestPersistenceMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := Load("self", path)
	if _, err := s.Apply(ConfigDoc{Version: 0}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600 (D18 plaintext CA key)", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file not removed after rename: err=%v", err)
	}
}

func TestPersistenceCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	s := Load("self", path) // must not panic
	if got := s.Get(); got.Version != 0 {
		t.Errorf("corrupt file gave Version %d, want empty 0", got.Version)
	}
}

func TestPersistenceMissingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sub", "config.json")
	s := Load("self", path)
	if _, err := s.Apply(ConfigDoc{Version: 0}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written into created dir: %v", err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("created dir mode = %o, want 0700", perm)
	}
}

func TestGossipRoundTrip(t *testing.T) {
	a := New("nodeA")
	if _, err := a.Apply(ConfigDoc{Version: 0, Cluster: ClusterInfo{Name: "fromA"}}); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	env := a.MarshalGossip()
	if len(env) == 0 {
		t.Fatal("MarshalGossip returned empty")
	}

	b := New("nodeB")
	b.MergeGossip(env)
	got := b.Get()
	if got.Version != 1 || got.Cluster.Name != "fromA" {
		t.Errorf("B did not converge to A: %+v", got)
	}

	// Malformed / empty payloads are no-ops.
	b.MergeGossip(nil)
	b.MergeGossip([]byte("garbage"))
	if got2 := b.Get(); got2.Version != got.Version {
		t.Error("malformed gossip mutated the store")
	}
}

func TestChangedCoalescing(t *testing.T) {
	s := New("self")
	if _, err := s.Apply(ConfigDoc{Version: 0}); err != nil {
		t.Fatalf("apply1: %v", err)
	}
	cur := s.Get()
	if _, err := s.Apply(cur); err != nil { // second apply with channel already full
		t.Fatalf("apply2: %v", err)
	}
	// At most one pending signal.
	if !fired(s) {
		t.Fatal("expected one pending signal")
	}
	if fired(s) {
		t.Error("more than one pending signal; coalescing failed")
	}
}
