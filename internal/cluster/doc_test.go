package cluster

import (
	"testing"

	"ensemble/internal/id"
)

func nodeRec(nid id.ID, ver uint64, name string) *NodeRecord {
	return &NodeRecord{ID: nid, Name: name, Version: ver, UpdatedAt: 100, Observed: map[id.ID]obsEntry{}}
}

func TestVersionedLater(t *testing.T) {
	lo := id.MustParse("00000000000000000000000000000001")
	hi := id.MustParse("00000000000000000000000000000002")

	cases := []struct {
		aVer uint64
		aW   id.ID
		bVer uint64
		bW   id.ID
		want bool
	}{
		{1, lo, 2, lo, true},  // higher version wins
		{2, lo, 1, lo, false}, // lower version loses
		{1, lo, 1, hi, true},  // equal version, larger writer wins
		{1, hi, 1, lo, false}, // equal version, smaller writer loses
		{1, lo, 1, lo, false}, // identical → not later
	}
	for i, c := range cases {
		if got := versionedLater(c.aVer, c.aW, c.bVer, c.bW); got != c.want {
			t.Errorf("case %d: versionedLater = %v want %v", i, got, c.want)
		}
	}
}

func TestMergeNodeHigherVersionWins(t *testing.T) {
	self := id.New()
	peer := id.New()
	d := newDocument()
	d.Nodes[peer] = nodeRec(peer, 1, "old")

	if !d.mergeNode(self, nodeRec(peer, 2, "new")) {
		t.Fatal("expected change")
	}
	if d.Nodes[peer].Name != "new" {
		t.Fatalf("name = %q want new", d.Nodes[peer].Name)
	}
}

func TestMergeNodeLowerVersionIgnored(t *testing.T) {
	self := id.New()
	peer := id.New()
	d := newDocument()
	d.Nodes[peer] = nodeRec(peer, 5, "keep")

	if d.mergeNode(self, nodeRec(peer, 3, "stale")) {
		t.Fatal("expected no change")
	}
	if d.Nodes[peer].Name != "keep" {
		t.Fatalf("name = %q want keep", d.Nodes[peer].Name)
	}
}

func TestMergeNodeNeverOverwritesSelf(t *testing.T) {
	self := id.New()
	d := newDocument()
	d.Nodes[self] = nodeRec(self, 1, "me")

	if d.mergeNode(self, nodeRec(self, 99, "hijack")) {
		t.Fatal("self record must never be overwritten by merge")
	}
	if d.Nodes[self].Name != "me" {
		t.Fatalf("name = %q want me", d.Nodes[self].Name)
	}
}

func TestMergeGroupNameTieBreakByWriter(t *testing.T) {
	g := id.New()
	lo := id.MustParse("00000000000000000000000000000001")
	hi := id.MustParse("000000000000000000000000000000ff")
	d := newDocument()
	d.Groups[g] = &GroupNameRecord{Name: "lo", Version: 1, Writer: lo}

	if !d.mergeGroupName(g, &GroupNameRecord{Name: "hi", Version: 1, Writer: hi}) {
		t.Fatal("equal version larger writer should win")
	}
	if d.Groups[g].Name != "hi" {
		t.Fatalf("name = %q want hi", d.Groups[g].Name)
	}
	// reverse direction is a no-op
	if d.mergeGroupName(g, &GroupNameRecord{Name: "lo", Version: 1, Writer: lo}) {
		t.Fatal("smaller writer should not win at equal version")
	}
}

func TestMergePlaybackLWW(t *testing.T) {
	g := id.New()
	w := id.New()
	d := newDocument()
	d.Playback[g] = &PlaybackRecord{State: "idle", Version: 1, Writer: w}
	if !d.mergePlayback(g, &PlaybackRecord{State: "playing", Version: 2, Writer: w}) {
		t.Fatal("higher version should win")
	}
	if d.Playback[g].State != "playing" {
		t.Fatalf("state = %q", d.Playback[g].State)
	}
}

func TestMergeAllConvergence(t *testing.T) {
	self := id.New()
	a := id.New()
	b := id.New()

	docA := newDocument()
	docA.Nodes[a] = nodeRec(a, 1, "a")
	docB := newDocument()
	docB.Nodes[b] = nodeRec(b, 1, "b")

	// A ∪ B
	d1 := docA.clone()
	d1.mergeAll(self, docB)
	// B ∪ A
	d2 := docB.clone()
	d2.mergeAll(self, docA)

	if len(d1.Nodes) != 2 || len(d2.Nodes) != 2 {
		t.Fatalf("want 2 nodes each, got %d/%d", len(d1.Nodes), len(d2.Nodes))
	}
	if d1.Nodes[a].Name != d2.Nodes[a].Name || d1.Nodes[b].Name != d2.Nodes[b].Name {
		t.Fatal("merge not order-independent")
	}
}

func TestCloneIsDeep(t *testing.T) {
	a := id.New()
	d := newDocument()
	rec := nodeRec(a, 1, "a")
	rec.Addrs = []string{"10.0.0.1/24"}
	rec.Observed[id.New()] = obsEntry{IP: "1.2.3.4", LastSeenUnix: 1}
	d.Nodes[a] = rec

	cp := d.clone()
	cp.Nodes[a].Name = "changed"
	cp.Nodes[a].Addrs[0] = "9.9.9.9/24"
	if d.Nodes[a].Name == "changed" {
		t.Fatal("clone shares Name")
	}
	if d.Nodes[a].Addrs[0] == "9.9.9.9/24" {
		t.Fatal("clone shares Addrs backing array")
	}
}

func TestPurgeOldRecords(t *testing.T) {
	self := id.New()
	old := id.New()
	fresh := id.New()
	g := id.New()
	d := newDocument()
	d.Nodes[self] = nodeRec(self, 1, "self")
	d.Nodes[self].UpdatedAt = 0 // ancient but self must survive
	d.Nodes[old] = nodeRec(old, 1, "old")
	d.Nodes[old].UpdatedAt = 10
	d.Nodes[fresh] = nodeRec(fresh, 1, "fresh")
	d.Nodes[fresh].UpdatedAt = 10_000
	d.Groups[g] = &GroupNameRecord{Name: "g", Version: 1, UpdatedAt: 10}

	alive := map[id.ID]bool{}
	removed := d.purge(self, 1000, alive)
	if !removed {
		t.Fatal("expected removal")
	}
	if _, ok := d.Nodes[self]; !ok {
		t.Fatal("self must not be purged")
	}
	if _, ok := d.Nodes[old]; ok {
		t.Fatal("old node should be purged")
	}
	if _, ok := d.Nodes[fresh]; !ok {
		t.Fatal("fresh node should remain")
	}
	if _, ok := d.Groups[g]; ok {
		t.Fatal("old group name should be purged")
	}
}

func TestPurgeKeepsAliveNode(t *testing.T) {
	self := id.New()
	quiet := id.New()
	d := newDocument()
	d.Nodes[self] = nodeRec(self, 1, "self")
	d.Nodes[quiet] = nodeRec(quiet, 1, "quiet")
	d.Nodes[quiet].UpdatedAt = 10 // old record but node is alive

	alive := map[id.ID]bool{quiet: true}
	d.purge(self, 1000, alive)
	if _, ok := d.Nodes[quiet]; !ok {
		t.Fatal("alive node must not be purged despite old record")
	}
}
