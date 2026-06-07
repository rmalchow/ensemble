package cluster

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// TestStateSaveLoadRoundTrip: a SetGroupName persists (debounced) and a fresh
// cluster started against the same path loads it. D47: SetGroupSettings persists
// ONLY when keyed by self id (the node's OWN group-settings record, D44: group id
// == master id) — a non-self settings write is master-keyed live state and does
// NOT persist.
func TestStateSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	self := id.New()
	g := id.New()     // a foreign group key (not self)
	other := id.New() // a foreign settings key (not self)

	saved := make(chan struct{}, 4)
	c1, err := New(Config{
		Self:         self,
		GossipPort:   freeUDPPort(t),
		BindAddr:     "127.0.0.1",
		StatePath:    path,
		SaveDebounce: 10 * time.Millisecond,
		saveNotify:   saved,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c1.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c1.SetGroupName(g, "kitchen")
	// OWN settings (key == self) persist; a foreign group's settings do NOT.
	c1.SetGroupSettings(self, contracts.GroupSettings{Codec: "opus", Transport: "tcp", BufferMs: 250})
	c1.SetGroupSettings(other, contracts.GroupSettings{Codec: "pcm", Transport: "udp", BufferMs: 100})
	waitSave(t, saved)
	_ = c1.Close()

	// Fresh cluster, SAME self + path, no gossip: must load the NAME and the OWN
	// settings record; the foreign settings record must NOT have persisted.
	c2, err := New(Config{Self: self, GossipPort: 7946, StatePath: path})
	if err != nil {
		t.Fatalf("New2: %v", err)
	}
	defer c2.Close()
	c2.mu.Lock()
	gn := c2.doc.Groups[g]
	own := c2.doc.Settings[self]
	foreign := c2.doc.Settings[other]
	c2.mu.Unlock()
	if gn == nil || gn.Name != "kitchen" {
		t.Fatalf("loaded name = %+v, want kitchen", gn)
	}
	if own == nil || own.Codec != "opus" || own.Transport != "tcp" || own.BufferMs != 250 {
		t.Fatalf("own settings = %+v, want opus/tcp/250 (D47)", own)
	}
	if foreign != nil {
		t.Fatalf("foreign settings must NOT persist (D47), got %+v", foreign)
	}
}

// TestLoadedVsGossipedLWW: an OLDER loaded version loses to a newer gossiped
// record; a NEWER loaded version wins over an older gossiped one.
func TestLoadedVsGossipedLWW(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	g := id.New()
	writer := id.New()

	// Persist version 5 directly.
	st := clusterState{
		Groups: map[id.ID]*GroupNameRecord{
			g: {Name: "loaded-v5", Version: 5, UpdatedAt: 100, Writer: writer},
		},
	}
	writeStateFile(t, path, st)

	c, err := New(Config{Self: id.New(), GossipPort: 7946, StatePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Older gossiped (v3) must LOSE.
	c.mu.Lock()
	c.doc.mergeGroupName(g, &GroupNameRecord{Name: "gossip-v3", Version: 3, UpdatedAt: 50, Writer: writer})
	got := c.doc.Groups[g].Name
	c.mu.Unlock()
	if got != "loaded-v5" {
		t.Fatalf("older gossip won: name = %q, want loaded-v5", got)
	}

	// Newer gossiped (v9) must WIN.
	c.mu.Lock()
	c.doc.mergeGroupName(g, &GroupNameRecord{Name: "gossip-v9", Version: 9, UpdatedAt: 200, Writer: writer})
	got = c.doc.Groups[g].Name
	c.mu.Unlock()
	if got != "gossip-v9" {
		t.Fatalf("newer gossip lost: name = %q, want gossip-v9", got)
	}
}

// TestOwnSettingsLoadedVsGossipedLWW: a loaded OWN settings record reconciles
// against a gossiped copy by the same LWW rule (older gossip loses, newer wins).
func TestOwnSettingsLoadedVsGossipedLWW(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	self := id.New()

	// Persist an own-settings record at version 5 (writer == self).
	st := clusterState{
		Settings: map[id.ID]*GroupSettingsRecord{
			self: {Codec: "opus", Transport: "tcp", BufferMs: 250, Version: 5, UpdatedAt: 100, Writer: self},
		},
	}
	writeSettingsStateFile(t, path, st)

	c, err := New(Config{Self: self, GossipPort: 7946, StatePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Loaded value present.
	c.mu.Lock()
	loaded := c.doc.Settings[self]
	c.mu.Unlock()
	if loaded == nil || loaded.BufferMs != 250 {
		t.Fatalf("loaded own settings = %+v, want bufferMs 250", loaded)
	}

	// Older gossiped (v3) must LOSE.
	c.mu.Lock()
	c.doc.mergeSettings(self, &GroupSettingsRecord{Codec: "pcm", Transport: "udp", BufferMs: 100, Version: 3, UpdatedAt: 50, Writer: self})
	got := c.doc.Settings[self].BufferMs
	c.mu.Unlock()
	if got != 250 {
		t.Fatalf("older gossip won: bufferMs = %d, want 250", got)
	}

	// Newer gossiped (v9) must WIN.
	c.mu.Lock()
	c.doc.mergeSettings(self, &GroupSettingsRecord{Codec: "pcm", Transport: "udp", BufferMs: 80, Version: 9, UpdatedAt: 200, Writer: self})
	got = c.doc.Settings[self].BufferMs
	c.mu.Unlock()
	if got != 80 {
		t.Fatalf("newer gossip lost: bufferMs = %d, want 80", got)
	}
}

// TestReconcileOwnSettingsVersion: after a restart loads our settings record at
// version N, a peer holding version >= N bumps our local counter above it (D7/D47)
// so our next local write wins.
func TestReconcileOwnSettingsVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	self := id.New()

	st := clusterState{
		Settings: map[id.ID]*GroupSettingsRecord{
			self: {Codec: "opus", Transport: "tcp", BufferMs: 250, Version: 4, UpdatedAt: 100, Writer: self},
		},
	}
	writeSettingsStateFile(t, path, st)

	c, err := New(Config{Self: self, GossipPort: freeUDPPort(t), BindAddr: "127.0.0.1", StatePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Close()

	// A peer holds our settings record at version 7 (> loaded 4).
	c.reconcileOwnSettingsVersion(7)

	c.mu.Lock()
	v := c.doc.Settings[self].Version
	c.mu.Unlock()
	if v != 8 {
		t.Fatalf("reconciled version = %d, want 8 (peer 7 + 1)", v)
	}
}

func writeSettingsStateFile(t *testing.T, path string, st clusterState) {
	t.Helper()
	c := &Cluster{statePath: path}
	c.doc = newDocument()
	for k, v := range st.Groups {
		c.doc.Groups[k] = v
	}
	// snapshotState only persists the SELF-keyed settings record, so stamp self.
	for k, v := range st.Settings {
		c.self = k
		c.doc.Settings[k] = v
	}
	if err := c.saveState(); err != nil {
		t.Fatalf("writeSettingsStateFile: %v", err)
	}
}

// TestCorruptStateFileNonFatal: a corrupt cluster.json warns + starts empty,
// never fatal.
func TestCorruptStateFileNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Self: id.New(), GossipPort: 7946, StatePath: path})
	if err != nil {
		t.Fatalf("New must not fail on corrupt state: %v", err)
	}
	defer c.Close()
	c.mu.Lock()
	n := len(c.doc.Groups)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("corrupt file should start empty, got %d records", n)
	}
}

// TestSaveDebounceCoalesces: a storm of changes saves a bounded number of times
// (not once per change).
func TestSaveDebounceCoalesces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	saved := make(chan struct{}, 64)
	c, err := New(Config{
		Self:         id.New(),
		GossipPort:   freeUDPPort(t),
		BindAddr:     "127.0.0.1",
		StatePath:    path,
		SaveDebounce: 40 * time.Millisecond,
		saveNotify:   saved,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Close()

	// 20 rapid renames of the same group within one debounce window.
	g := id.New()
	for i := 0; i < 20; i++ {
		c.SetGroupName(g, "name-"+string(rune('a'+i)))
	}
	waitSave(t, saved)
	// Give a moment for any spurious extra saves.
	time.Sleep(80 * time.Millisecond)
	n := drain(saved) + 1 // +1 for the one we already consumed
	if n > 3 {
		t.Fatalf("debounce did not coalesce: %d saves for a 20-change storm", n)
	}
}

// --- helpers ---

func waitSave(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for save")
	}
}

func drain(ch <-chan struct{}) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

func writeStateFile(t *testing.T, path string, st clusterState) {
	t.Helper()
	c := &Cluster{statePath: path}
	// Inject via a temp doc + snapshot is overkill; write directly using the same
	// marshaling path the saver uses by stuffing the doc.
	c.doc = newDocument()
	for k, v := range st.Groups {
		c.doc.Groups[k] = v
	}
	if err := c.saveState(); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
}
