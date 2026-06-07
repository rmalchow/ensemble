package cluster

import (
	"net/netip"
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// newTestCluster builds a Cluster via New without starting memberlist. Its queue
// and doc are usable for setter/snapshot tests.
func newTestCluster(t *testing.T, self id.ID, now func() time.Time) *Cluster {
	t.Helper()
	c, err := New(Config{
		Self:       self,
		Name:       "n",
		Volume:     1.0,
		GossipPort: 7946,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func ownVersion(c *Cluster) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doc.Nodes[c.self].Version
}

func queued(c *Cluster) int { return c.queue.NumQueued() }

func TestSetNameBumpsVersionAndBroadcasts(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	sub := c.Subscribe()
	v0 := ownVersion(c)
	c.SetName("alice")
	if ownVersion(c) != v0+1 {
		t.Fatalf("version not bumped: %d -> %d", v0, ownVersion(c))
	}
	if queued(c) == 0 {
		t.Fatal("no broadcast queued")
	}
	select {
	case <-sub:
	default:
		t.Fatal("no notify")
	}
	if c.Snapshot().Nodes[0].Name != "alice" {
		t.Fatal("snapshot name not updated")
	}
}

func TestSetNameNoOpWhenUnchanged(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	c.SetName("bob")
	v := ownVersion(c)
	q := queued(c)
	c.SetName("bob")
	if ownVersion(c) != v {
		t.Fatal("version bumped on unchanged name")
	}
	if queued(c) != q {
		t.Fatal("broadcast queued on unchanged name")
	}
}

func TestSetVolumeBumpsVersionAndShowsInSnapshot(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	v0 := ownVersion(c)
	c.SetVolume(0.5)
	if ownVersion(c) != v0+1 {
		t.Fatal("version not bumped")
	}
	if got := c.Snapshot().Nodes[0].Volume; got != 0.5 {
		t.Fatalf("volume = %v want 0.5", got)
	}
}

func TestSetVolumeNoOpWhenUnchanged(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	c.SetVolume(0.3)
	v := ownVersion(c)
	c.SetVolume(0.3)
	if ownVersion(c) != v {
		t.Fatal("version bumped on unchanged volume")
	}
}

func TestSetOutputDelayMsBumpsVersionAndShowsInSnapshot(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	v0 := ownVersion(c)
	c.SetOutputDelayMs(120)
	if ownVersion(c) != v0+1 {
		t.Fatal("version not bumped")
	}
	if got := c.Snapshot().Nodes[0].OutputDelayMs; got != 120 {
		t.Fatalf("outputDelayMs = %d want 120", got)
	}
}

func TestSetOutputDelayMsNoOpWhenUnchanged(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	c.SetOutputDelayMs(50)
	v := ownVersion(c)
	c.SetOutputDelayMs(50)
	if ownVersion(c) != v {
		t.Fatal("version bumped on unchanged value")
	}
}

func TestVolumeAndDelayMergeFromRemote(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)

	rec := nodeRec(peer, 3, "peer")
	rec.Volume = 0.25
	rec.OutputDelayMs = -80
	c.mu.Lock()
	c.doc.mergeNode(self, rec)
	c.mu.Unlock()

	var pv *contracts.NodeView
	for i := range c.Snapshot().Nodes {
		if c.Snapshot().Nodes[i].ID == peer {
			n := c.Snapshot().Nodes[i]
			pv = &n
		}
	}
	if pv == nil {
		t.Fatal("peer not in snapshot")
	}
	if pv.Volume != 0.25 || pv.OutputDelayMs != -80 {
		t.Fatalf("merged volume/delay wrong: %v %d", pv.Volume, pv.OutputDelayMs)
	}
}

func TestSetFollowingZeroIsSolo(t *testing.T) {
	self := id.New()
	target := id.New()
	c := newTestCluster(t, self, nil)
	c.SetFollowing(target)
	if c.Snapshot().Nodes[0].Following != target {
		t.Fatal("following not set")
	}
	c.SetFollowing(id.Zero)
	if !c.Snapshot().Nodes[0].Following.IsZero() {
		t.Fatal("following not cleared to solo")
	}
}

func TestSetPlaybackWritesGroupKey(t *testing.T) {
	self := id.New()
	g := id.New()
	c := newTestCluster(t, self, nil)
	c.SetPlayback(g, contracts.Playback{State: "playing", URI: "file:x.wav"})
	c.mu.Lock()
	rec := c.doc.Playback[g]
	c.mu.Unlock()
	if rec == nil || rec.State != "playing" || rec.Writer != self {
		t.Fatalf("playback record wrong: %+v", rec)
	}
}

func TestSetGroupSettingsFillsDefaults(t *testing.T) {
	self := id.New()
	g := id.New()
	c := newTestCluster(t, self, nil)
	c.SetGroupSettings(g, contracts.GroupSettings{Codec: "opus"})
	c.mu.Lock()
	rec := c.doc.Settings[g]
	c.mu.Unlock()
	if rec.Codec != "opus" || rec.Transport != contracts.DefaultTransport || rec.BufferMs != contracts.DefaultBufferMs {
		t.Fatalf("defaults not filled: %+v", rec)
	}
}

func TestObserveNewIPBroadcasts(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	v0 := ownVersion(c)
	c.Observe(peer, netip.MustParseAddr("192.168.1.5"))
	if ownVersion(c) != v0+1 {
		t.Fatal("first observation should bump version")
	}
}

func TestObserveSameIPThrottled(t *testing.T) {
	self := id.New()
	peer := id.New()
	now := time.Now()
	c := newTestCluster(t, self, func() time.Time { return now })
	c.Observe(peer, netip.MustParseAddr("10.0.0.7"))
	v := ownVersion(c)
	c.Observe(peer, netip.MustParseAddr("10.0.0.7")) // same ip, same clock
	if ownVersion(c) != v {
		t.Fatal("repeat observation within interval should be throttled")
	}
}

func TestObserveUpdatesOwnRecordMap(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	c.Observe(peer, netip.MustParseAddr("172.16.0.9"))
	obs := c.Snapshot().Nodes[0].Observed
	if e, ok := obs[peer]; !ok || e.IP != "172.16.0.9" {
		t.Fatalf("own observed map missing peer: %+v", obs)
	}
}

func TestObserveNewIPAfterChangeBroadcasts(t *testing.T) {
	self := id.New()
	peer := id.New()
	now := time.Now()
	c := newTestCluster(t, self, func() time.Time { return now })
	c.Observe(peer, netip.MustParseAddr("10.0.0.1"))
	v := ownVersion(c)
	c.Observe(peer, netip.MustParseAddr("10.0.0.2")) // different ip, even same clock
	if ownVersion(c) != v+1 {
		t.Fatal("a new IP should always re-broadcast")
	}
}
