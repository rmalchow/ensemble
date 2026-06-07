package cluster

import (
	"net/netip"
	"strings"
	"testing"

	"ensemble/internal/id"
)

func TestSnapshotResolvesLiveness(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	c.mu.Lock()
	c.doc.Nodes[peer] = nodeRec(peer, 1, "peer")
	c.mu.Unlock()
	c.live.join(peer, 1000)

	snap := c.Snapshot()
	var selfAlive, peerAlive bool
	for _, n := range snap.Nodes {
		if n.ID == self {
			selfAlive = n.Alive
		}
		if n.ID == peer {
			peerAlive = n.Alive
		}
	}
	if !selfAlive {
		t.Fatal("self should be alive")
	}
	if !peerAlive {
		t.Fatal("joined peer should be alive")
	}
}

func TestSnapshotDeepCopy(t *testing.T) {
	self := id.New()
	c := newTestCluster(t, self, nil)
	snap := c.Snapshot()
	snap.Nodes[0].Name = "mutated"
	if c.Snapshot().Nodes[0].Name == "mutated" {
		t.Fatal("snapshot shares state with live doc")
	}
}

func TestDeriveGroupsSolo(t *testing.T) {
	self := id.New()
	nodes := map[id.ID]*NodeRecord{self: {ID: self, Following: id.Zero}}
	groups := DeriveGroups(nodes, nil, nil, nil, map[id.ID]bool{}, self)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.ID != self {
		t.Fatalf("solo group id should equal node id")
	}
	if g.Master != self || len(g.Members) != 1 {
		t.Fatalf("solo group wrong: %+v", g)
	}
}

func TestDeriveGroupsFollowerJoins(t *testing.T) {
	a := id.New()
	b := id.New()
	nodes := map[id.ID]*NodeRecord{
		a: {ID: a, Following: id.Zero},
		b: {ID: b, Following: a},
	}
	alive := map[id.ID]bool{a: true, b: true}
	groups := DeriveGroups(nodes, nil, nil, nil, alive, a)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Master != a {
		t.Fatalf("master should be a")
	}
	if len(g.Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(g.Members))
	}
	if g.ID != a {
		t.Fatal("group id should be the master id (D42)")
	}
}

func TestDeriveGroupsDeadMasterSolo(t *testing.T) {
	a := id.New() // dead master
	b := id.New() // follower of dead a
	nodes := map[id.ID]*NodeRecord{
		a: {ID: a, Following: id.Zero},
		b: {ID: b, Following: a},
	}
	alive := map[id.ID]bool{b: true} // a is dead
	groups := DeriveGroups(nodes, nil, nil, nil, alive, b)
	// b projected as its own solo group; a is dead so not a group.
	if len(groups) != 1 {
		t.Fatalf("want 1 group (b solo), got %d", len(groups))
	}
	if groups[0].Master != b || groups[0].ID != b {
		t.Fatalf("b should be projected solo: %+v", groups[0])
	}
}

func TestDeriveGroupsFollowingAFollowerSolo(t *testing.T) {
	a := id.New()  // master
	b := id.New()  // follows a
	cc := id.New() // follows b (a follower) → should be projected solo
	nodes := map[id.ID]*NodeRecord{
		a:  {ID: a, Following: id.Zero},
		b:  {ID: b, Following: a},
		cc: {ID: cc, Following: b},
	}
	alive := map[id.ID]bool{a: true, b: true, cc: true}
	groups := DeriveGroups(nodes, nil, nil, nil, alive, a)
	// {a,b} group, and cc solo.
	var ab, solo bool
	for _, g := range groups {
		if g.Master == a && len(g.Members) == 2 {
			ab = true
		}
		if g.Master == cc && len(g.Members) == 1 {
			solo = true
		}
	}
	if !ab || !solo {
		t.Fatalf("expected {a,b} group + cc solo; got %+v", groups)
	}
}

func TestDeriveGroupsStableOrder(t *testing.T) {
	a := id.New()
	b := id.New()
	nodes := map[id.ID]*NodeRecord{
		a: {ID: a, Following: id.Zero},
		b: {ID: b, Following: id.Zero},
	}
	alive := map[id.ID]bool{a: true, b: true}
	g1 := DeriveGroups(nodes, nil, nil, nil, alive, a)
	g2 := DeriveGroups(nodes, nil, nil, nil, alive, a)
	for i := range g1 {
		if g1[i].ID != g2[i].ID {
			t.Fatal("group order not deterministic")
		}
	}
	// sorted ascending
	if len(g1) == 2 && idLess(g1[1].ID, g1[0].ID) {
		t.Fatal("groups not sorted by id")
	}
}

func TestDeriveGroupsJoinsNamePlaybackSettings(t *testing.T) {
	a := id.New()
	nodes := map[id.ID]*NodeRecord{a: {ID: a, Name: "alpha", Following: id.Zero}}
	gid := a // solo: group id == master id == node id; member XOR == node id too
	names := map[id.ID]*GroupNameRecord{gid: {Name: "Kitchen"}}
	playback := map[id.ID]*PlaybackRecord{gid: {State: "playing", URI: "file:s.wav"}}
	settings := map[id.ID]*GroupSettingsRecord{gid: {Codec: "opus", Transport: "tcp", BufferMs: 200}}
	groups := DeriveGroups(nodes, names, playback, settings, map[id.ID]bool{a: true}, a)
	g := groups[0]
	if g.Name != "Kitchen" {
		t.Fatalf("name = %q", g.Name)
	}
	if g.NameIsDerived {
		t.Fatal("an explicit override must not be flagged derived")
	}
	if g.Playback.State != "playing" || g.Playback.URI != "file:s.wav" {
		t.Fatalf("playback = %+v", g.Playback)
	}
	if g.Settings.Codec != "opus" || g.Settings.BufferMs != 200 {
		t.Fatalf("settings = %+v", g.Settings)
	}
}

// TestDeriveGroupsIDIsMasterPlaybackSettingsKeyed verifies D42: a MULTI-member
// group is keyed by the master id for playback + settings, while the explicit
// name OVERRIDE is keyed by the member-set XOR.
func TestDeriveGroupsIDIsMasterPlaybackSettingsKeyed(t *testing.T) {
	a := id.New() // master
	b := id.New() // follower
	nodes := map[id.ID]*NodeRecord{
		a: {ID: a, Name: "alpha", Following: id.Zero},
		b: {ID: b, Name: "bravo", Following: a},
	}
	alive := map[id.ID]bool{a: true, b: true}
	xor := id.XOR(a, b)
	names := map[id.ID]*GroupNameRecord{xor: {Name: "Den"}}        // override keyed by XOR
	playback := map[id.ID]*PlaybackRecord{a: {State: "playing"}}   // keyed by master id
	settings := map[id.ID]*GroupSettingsRecord{a: {Codec: "opus"}} // keyed by master id
	groups := DeriveGroups(nodes, names, playback, settings, alive, a)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.ID != a {
		t.Fatalf("group id must be master id, got %s", g.ID)
	}
	if g.Name != "Den" || g.NameIsDerived {
		t.Fatalf("override (XOR-keyed) name not resolved: %q derived=%v", g.Name, g.NameIsDerived)
	}
	if g.Playback.State != "playing" {
		t.Fatalf("playback (master-keyed) not resolved: %+v", g.Playback)
	}
	if g.Settings.Codec != "opus" {
		t.Fatalf("settings (master-keyed) not resolved: %+v", g.Settings)
	}
}

// TestDeriveGroupsDerivedLabel verifies the server-side derived label: sorted
// member names joined with " + ", solo = node name, " +N more" past the cap,
// short-id fallback for a member missing from the snapshot.
func TestDeriveGroupsDerivedLabel(t *testing.T) {
	a := id.New()
	b := id.New()
	nodes := map[id.ID]*NodeRecord{
		a: {ID: a, Name: "kitchen", Following: id.Zero},
		b: {ID: b, Name: "bedroom", Following: a},
	}
	alive := map[id.ID]bool{a: true, b: true}
	groups := DeriveGroups(nodes, nil, nil, nil, alive, a)
	g := groups[0]
	if g.Name != "bedroom + kitchen" { // names sorted, master-independent
		t.Fatalf("derived label = %q", g.Name)
	}
	if !g.NameIsDerived {
		t.Fatal("derived label must be flagged NameIsDerived")
	}

	// Solo derived label == node name.
	solo := DeriveGroups(map[id.ID]*NodeRecord{a: {ID: a, Name: "kitchen", Following: id.Zero}},
		nil, nil, nil, map[id.ID]bool{a: true}, a)
	if solo[0].Name != "kitchen" || !solo[0].NameIsDerived {
		t.Fatalf("solo derived label = %q derived=%v", solo[0].Name, solo[0].NameIsDerived)
	}
}

// TestDeriveGroupsDerivedLabelCapAndFallback exercises the " +N more" truncation
// and the short-id fallback for a member missing from the snapshot.
func TestDeriveGroupsDerivedLabelCapAndFallback(t *testing.T) {
	master := id.New()
	nodes := map[id.ID]*NodeRecord{master: {ID: master, Name: "m", Following: id.Zero}}
	followers := make([]id.ID, 0, 5)
	for i := 0; i < 5; i++ {
		f := id.New()
		followers = append(followers, f)
		nodes[f] = &NodeRecord{ID: f, Name: "", Following: master} // empty name → short-id fallback
	}
	// give names to a few so the label is deterministic-ish; leave the rest empty.
	alive := map[id.ID]bool{master: true}
	for _, f := range followers {
		alive[f] = true
	}
	groups := DeriveGroups(nodes, nil, nil, nil, alive, master)
	g := groups[0]
	if len(g.Members) != 6 {
		t.Fatalf("want 6 members, got %d", len(g.Members))
	}
	// 6 names → first 3 shown + " +3 more".
	if !strings.Contains(g.Name, "+3 more") {
		t.Fatalf("expected truncation in label, got %q", g.Name)
	}
	if !g.NameIsDerived {
		t.Fatal("derived label must be flagged NameIsDerived")
	}
}

func TestDialCandidatesIntersection(t *testing.T) {
	self := id.New()
	peer := id.New()
	other := id.New()
	c := newTestCluster(t, self, nil)
	c.mu.Lock()
	// peer self-reports two IPs
	c.doc.Nodes[peer] = &NodeRecord{ID: peer, Addrs: []string{"10.0.0.5/24", "10.0.0.6/24"}}
	// self observed 10.0.0.5 recently; other observed 10.0.0.6 earlier
	c.doc.Nodes[self].Observed = map[id.ID]obsEntry{peer: {IP: "10.0.0.5", LastSeenUnix: 200}}
	c.doc.Nodes[other] = &NodeRecord{ID: other, Observed: map[id.ID]obsEntry{peer: {IP: "10.0.0.6", LastSeenUnix: 100}}}
	c.mu.Unlock()

	got := c.DialCandidates(peer)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %v", len(got), got)
	}
	if got[0] != netip.MustParseAddr("10.0.0.5") {
		t.Fatalf("most-recent first failed: %v", got)
	}
}

func TestDialCandidatesFallbackEmptyObservations(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	c.mu.Lock()
	c.doc.Nodes[peer] = &NodeRecord{ID: peer, Addrs: []string{"192.168.0.10/24"}}
	c.mu.Unlock()
	got := c.DialCandidates(peer)
	if len(got) != 1 || got[0] != netip.MustParseAddr("192.168.0.10") {
		t.Fatalf("cold-peer fallback failed: %v", got)
	}
}

func TestDialCandidatesSkipsUnparseableCIDR(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	c.mu.Lock()
	c.doc.Nodes[peer] = &NodeRecord{ID: peer, Addrs: []string{"garbage", "10.1.1.1/24"}}
	c.mu.Unlock()
	got := c.DialCandidates(peer)
	if len(got) != 1 || got[0] != netip.MustParseAddr("10.1.1.1") {
		t.Fatalf("bad CIDR not skipped: %v", got)
	}
}

func TestDialCandidatesUnknownPeer(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	if got := c.DialCandidates(id.New()); got != nil {
		t.Fatalf("unknown peer should yield nil, got %v", got)
	}
}
