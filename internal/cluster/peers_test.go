package cluster

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func peersPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "peers.json")
}

func memberWith(id, name, ip string, port uint16) Member {
	return Member{
		Addr: net.ParseIP(ip),
		Port: port,
		Meta: Meta{NodeID: id, Name: name},
	}
}

func TestLoadPeerStoreMissing(t *testing.T) {
	s := LoadPeerStore(filepath.Join(t.TempDir(), "nope.json"))
	if s == nil {
		t.Fatal("LoadPeerStore returned nil")
	}
	if len(s.Snapshot()) != 0 {
		t.Errorf("missing file should yield empty store, got %d", len(s.Snapshot()))
	}
}

func TestLoadPeerStoreCorrupt(t *testing.T) {
	path := peersPath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadPeerStore(path) // must not panic
	if len(s.Snapshot()) != 0 {
		t.Errorf("corrupt file should yield empty store, got %d", len(s.Snapshot()))
	}
}

func TestUpsertPersistsModeAndAtomic(t *testing.T) {
	path := peersPath(t)
	s := LoadPeerStore(path)
	s.Upsert([]Member{
		memberWith("n1", "alpha", "127.0.0.1", 7946),
		memberWith("n2", "beta", "127.0.0.2", 7946),
		memberWith("", "skipme", "127.0.0.3", 7946), // empty id skipped
	})

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("peers.json not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("peers.json mode = %o, want 0644", perm)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not linger after atomic rename")
	}

	seeds := s.JoinSeeds()
	sort.Strings(seeds)
	want := []string{"127.0.0.1:7946", "127.0.0.2:7946"}
	if len(seeds) != len(want) {
		t.Fatalf("JoinSeeds = %v, want %v", seeds, want)
	}
	for i := range want {
		if seeds[i] != want[i] {
			t.Errorf("JoinSeeds[%d] = %q, want %q", i, seeds[i], want[i])
		}
	}
}

func TestPeerStoreRoundTrip(t *testing.T) {
	path := peersPath(t)
	s := LoadPeerStore(path)
	s.Upsert([]Member{
		memberWith("n1", "alpha", "127.0.0.1", 7946),
		memberWith("n2", "beta", "127.0.0.2", 7946),
	})

	reloaded := LoadPeerStore(path)
	got := idSet(reloaded.Snapshot())
	want := map[string]bool{"n1": true, "n2": true}
	if len(got) != len(want) {
		t.Fatalf("round-trip snapshot ids = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("round-trip missing id %q", id)
		}
	}
	for _, p := range reloaded.Snapshot() {
		if p.Name == "" || p.GossipAddr == "" || p.LastSeen == 0 {
			t.Errorf("round-trip peer %q lost fields: %+v", p.ID, p)
		}
	}
}

func TestPeerStoreRemove(t *testing.T) {
	path := peersPath(t)
	s := LoadPeerStore(path)
	s.Upsert([]Member{
		memberWith("n1", "alpha", "127.0.0.1", 7946),
		memberWith("n2", "beta", "127.0.0.2", 7946),
	})
	s.Remove("n1")
	if ids := idSet(s.Snapshot()); ids["n1"] || !ids["n2"] {
		t.Errorf("after Remove(n1): %v, want only n2", ids)
	}
	// Persisted: reload sees the removal.
	if ids := idSet(LoadPeerStore(path).Snapshot()); ids["n1"] {
		t.Errorf("Remove not persisted; n1 still present after reload")
	}
	s.Remove("nope") // unknown id is a no-op, no panic
}

func TestPeerStoreClear(t *testing.T) {
	path := peersPath(t)
	s := LoadPeerStore(path)
	s.Upsert([]Member{memberWith("n1", "alpha", "127.0.0.1", 7946)})
	s.Clear()
	if len(s.Snapshot()) != 0 {
		t.Errorf("Clear left %d peers", len(s.Snapshot()))
	}
	if len(LoadPeerStore(path).Snapshot()) != 0 {
		t.Errorf("Clear not persisted; peers present after reload")
	}
}

func idSet(peers []Peer) map[string]bool {
	out := make(map[string]bool, len(peers))
	for _, p := range peers {
		out[p.ID] = true
	}
	return out
}
