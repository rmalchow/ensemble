package cluster

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// TestStateSaveLoadRoundTrip: a SetGroupName/SetGroupSettings persists (debounced)
// and a fresh cluster started against the same path loads the records.
func TestStateSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	g := id.New()

	saved := make(chan struct{}, 4)
	c1, err := New(Config{
		Self:         id.New(),
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
	c1.SetGroupSettings(g, contracts.GroupSettings{Codec: "opus", Transport: "tcp", BufferMs: 200})
	waitSave(t, saved)
	_ = c1.Close()

	// Fresh cluster, same path, no gossip: must load both records.
	c2, err := New(Config{Self: id.New(), GossipPort: 7946, StatePath: path})
	if err != nil {
		t.Fatalf("New2: %v", err)
	}
	defer c2.Close()
	c2.mu.Lock()
	gn := c2.doc.Groups[g]
	gs := c2.doc.Settings[g]
	c2.mu.Unlock()
	if gn == nil || gn.Name != "kitchen" {
		t.Fatalf("loaded name = %+v, want kitchen", gn)
	}
	if gs == nil || gs.Codec != "opus" || gs.Transport != "tcp" || gs.BufferMs != 200 {
		t.Fatalf("loaded settings = %+v", gs)
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
	n := len(c.doc.Groups) + len(c.doc.Settings)
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
	for k, v := range st.Settings {
		c.doc.Settings[k] = v
	}
	if err := c.saveState(); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
}
