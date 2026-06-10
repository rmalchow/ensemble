package spotify

import (
	"sync"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

type fakeEngine struct {
	mu        sync.Mutex
	plays     []string
	stops     int
	refresh   int
	follows   []id.ID
	unfollows int
}

func (f *fakeEngine) Play(uri string) error {
	f.mu.Lock()
	f.plays = append(f.plays, uri)
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) Stop() error      { f.mu.Lock(); f.stops++; f.mu.Unlock(); return nil }
func (f *fakeEngine) RefreshPlayback() { f.mu.Lock(); f.refresh++; f.mu.Unlock() }
func (f *fakeEngine) Follow(t id.ID) error {
	f.mu.Lock()
	f.follows = append(f.follows, t)
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) Unfollow() error { f.mu.Lock(); f.unfollows++; f.mu.Unlock(); return nil }

type fakeCl struct {
	self    id.ID
	snap    contracts.Snapshot
	mu      sync.Mutex
	assigns [][2]id.ID
}

func (f *fakeCl) Self() id.ID                  { return f.self }
func (f *fakeCl) Snapshot() contracts.Snapshot { return f.snap }
func (f *fakeCl) AssignPlaybackNode(node, target id.ID) bool {
	f.mu.Lock()
	f.assigns = append(f.assigns, [2]id.ID{node, target})
	f.mu.Unlock()
	return true
}

func mkID(n byte) id.ID { var x id.ID; x[15] = n; return x }

func newTestMgr(eng *fakeEngine, cl *fakeCl) *Manager {
	m := NewManager("/bin/true", t1Dir, "lr", eng, cl, nil)
	m.started = true
	return m
}

const t1Dir = "/tmp/ensemble-spotify-test" // unused at the orchestration layer (no real bridges)

// A preset's OnPlay regroups to its players (join wanted, unjoin others) and
// plays its endpoint URI; the default endpoint plays without regrouping.
func TestOnPlayPresetRegroupsAndPlays(t *testing.T) {
	self, p1, p2, p3 := mkID(1), mkID(2), mkID(3), mkID(4)
	cl := &fakeCl{self: self, snap: contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: p1, PlaybackNode: true},
		{ID: p2, PlaybackNode: true},
		{ID: p3, PlaybackNode: true, Following: self}, // currently in our group, not wanted
	}}}
	eng := &fakeEngine{}
	m := newTestMgr(eng, cl)
	m.bridges["kitchen"] = &managed{ep: contracts.SpotifyEndpoint{ID: "kitchen", Players: []id.ID{p1, p2}}}

	m.onPlay("kitchen")

	if len(eng.plays) != 1 || eng.plays[0] != "spotify:kitchen" {
		t.Fatalf("plays=%v, want [spotify:kitchen]", eng.plays)
	}
	want := map[id.ID]id.ID{p1: self, p2: self, p3: id.ID{}} // p3 unjoined (→ Zero)
	got := map[id.ID]id.ID{}
	for _, a := range cl.assigns {
		got[a[0]] = a[1]
	}
	for n, tgt := range want {
		if got[n] != tgt {
			t.Fatalf("assign[%v]=%v, want %v (all=%v)", n, got[n], tgt, cl.assigns)
		}
	}
}

// When the master's own node is a selected player it joins by following itself
// (gossiping node, not a playback node); other playback nodes still assign.
func TestOnPlayIncludesMasterSelf(t *testing.T) {
	self, p1 := mkID(1), mkID(2)
	cl := &fakeCl{self: self, snap: contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: self},                   // gossiping master (NOT a PlaybackNode)
		{ID: p1, PlaybackNode: true}, // non-gossiping speaker
	}}}
	eng := &fakeEngine{}
	m := newTestMgr(eng, cl)
	m.bridges["all"] = &managed{ep: contracts.SpotifyEndpoint{ID: "all", Players: []id.ID{self, p1}}}

	m.onPlay("all")

	if len(eng.follows) != 1 || eng.follows[0] != self {
		t.Fatalf("master self not joined via Follow(self): follows=%v", eng.follows)
	}
	found := false
	for _, a := range cl.assigns {
		if a == [2]id.ID{p1, self} {
			found = true
		}
	}
	if !found {
		t.Fatalf("playback node p1 not assigned to self: %v", cl.assigns)
	}
}

// When the master is NOT a selected player but currently follows itself, it
// unfollows (leaves its own group); a crosswise follow is left alone.
func TestOnPlayMasterSelfUnfollowsWhenNotWanted(t *testing.T) {
	self, p1 := mkID(1), mkID(2)
	cl := &fakeCl{self: self, snap: contracts.Snapshot{Nodes: []contracts.NodeView{
		{ID: self, Following: self}, // currently in its own group
		{ID: p1, PlaybackNode: true},
	}}}
	eng := &fakeEngine{}
	m := newTestMgr(eng, cl)
	m.bridges["x"] = &managed{ep: contracts.SpotifyEndpoint{ID: "x", Players: []id.ID{p1}}}

	m.onPlay("x")
	if eng.unfollows != 1 {
		t.Fatalf("master should unfollow when not a wanted player, unfollows=%d", eng.unfollows)
	}
}

func TestOnPlayDefaultNoRegroup(t *testing.T) {
	cl := &fakeCl{self: mkID(1)}
	eng := &fakeEngine{}
	m := newTestMgr(eng, cl)
	m.bridges[""] = &managed{}

	m.onPlay("")
	if len(eng.plays) != 1 || eng.plays[0] != "spotify:" {
		t.Fatalf("plays=%v, want [spotify:]", eng.plays)
	}
	if len(cl.assigns) != 0 {
		t.Fatalf("default endpoint must not regroup, got %v", cl.assigns)
	}
}

// Starting endpoint B preempts A: a later OnStop from A (the deselected device)
// must NOT stop B's playback.
func TestOnStopOnlyStopsActiveEndpoint(t *testing.T) {
	cl := &fakeCl{self: mkID(1)}
	eng := &fakeEngine{}
	m := newTestMgr(eng, cl)
	m.bridges["a"] = &managed{ep: contracts.SpotifyEndpoint{ID: "a"}}
	m.bridges["b"] = &managed{ep: contracts.SpotifyEndpoint{ID: "b"}}

	m.onPlay("a")
	m.onPlay("b") // preempts a (active = b)
	m.onStop("a") // stale stop from the deselected device
	if eng.stops != 0 {
		t.Fatalf("stop fired %d times for the inactive endpoint, want 0", eng.stops)
	}
	m.onStop("b")
	if eng.stops != 1 {
		t.Fatalf("stop fired %d times for the active endpoint, want 1", eng.stops)
	}
}

// Only the active endpoint's metadata events refresh playback.
func TestOnMetadataActiveOnly(t *testing.T) {
	cl := &fakeCl{self: mkID(1)}
	eng := &fakeEngine{}
	m := newTestMgr(eng, cl)
	m.bridges["a"] = &managed{ep: contracts.SpotifyEndpoint{ID: "a"}}
	m.bridges["b"] = &managed{ep: contracts.SpotifyEndpoint{ID: "b"}}

	m.onPlay("a")
	m.onMetadata("b") // not active → ignored
	m.onMetadata("a") // active → refresh
	if eng.refresh != 1 {
		t.Fatalf("refresh=%d, want 1", eng.refresh)
	}
}

func TestDeviceName(t *testing.T) {
	m := &Manager{nodeName: "lr"}
	if got := m.deviceName("", ""); got != "ensemble lr" {
		t.Fatalf("default device = %q", got)
	}
	if got := m.deviceName("kitchen", "Kitchen"); got != "ensemble lr: Kitchen" {
		t.Fatalf("preset device = %q", got)
	}
}
