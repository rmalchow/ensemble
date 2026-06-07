package cluster

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/hashicorp/memberlist"

	"ensemble/internal/id"
)

func TestEncodeDecodeDeltaAllKinds(t *testing.T) {
	g := id.New()
	n := id.New()
	cases := []struct {
		kind byte
		d    delta
	}{
		{kindNodeDelta, delta{Node: nodeRec(n, 2, "x")}},
		{kindGroupName, delta{Group: g, Name: &GroupNameRecord{Name: "G", Version: 1, Writer: n}}},
		{kindPlayback, delta{Group: g, Playback: &PlaybackRecord{State: "playing", Version: 1, Writer: n}}},
		{kindSettings, delta{Group: g, Settings: &GroupSettingsRecord{Codec: "pcm", Version: 1, Writer: n}}},
	}
	for _, tc := range cases {
		k, d, err := decodeDelta(encodeDelta(tc.kind, tc.d))
		if err != nil {
			t.Fatalf("kind %c: %v", tc.kind, err)
		}
		if k != tc.kind {
			t.Fatalf("kind mismatch: %c != %c", k, tc.kind)
		}
		switch tc.kind {
		case kindNodeDelta:
			if d.Node == nil || d.Node.Name != "x" {
				t.Fatalf("node delta lost: %+v", d.Node)
			}
		case kindGroupName:
			if d.Name == nil || d.Name.Name != "G" || d.Group != g {
				t.Fatalf("name delta lost: %+v", d)
			}
		case kindPlayback:
			if d.Playback == nil || d.Playback.State != "playing" {
				t.Fatalf("playback delta lost")
			}
		case kindSettings:
			if d.Settings == nil || d.Settings.Codec != "pcm" {
				t.Fatalf("settings delta lost")
			}
		}
	}
}

func TestBroadcastInvalidates(t *testing.T) {
	n := id.New()
	b1 := &broadcast{key: broadcastKey(kindNodeDelta, n)}
	b2 := &broadcast{key: broadcastKey(kindNodeDelta, n)}
	b3 := &broadcast{key: broadcastKey(kindNodeDelta, id.New())}
	if !b1.Invalidates(b2) {
		t.Fatal("same key should invalidate")
	}
	if b1.Invalidates(b3) {
		t.Fatal("different key should not invalidate")
	}
}

func TestNotifyMsgAppliesDelta(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	sub := c.Subscribe()

	msg := encodeDelta(kindNodeDelta, delta{Node: nodeRec(peer, 1, "remote")})
	c.deleg.NotifyMsg(msg)

	c.mu.Lock()
	_, ok := c.doc.Nodes[peer]
	c.mu.Unlock()
	if !ok {
		t.Fatal("peer delta not merged")
	}
	select {
	case <-sub:
	default:
		t.Fatal("no notify on applied delta")
	}
}

func TestNotifyMsgBadPayloadIgnored(t *testing.T) {
	c := newTestCluster(t, id.New(), nil)
	c.deleg.NotifyMsg([]byte("x{not-json")) // must not panic
	c.deleg.NotifyMsg(nil)
}

func TestLocalStateMergeRemoteRoundTrip(t *testing.T) {
	self := id.New()
	peer := id.New()
	src := newTestCluster(t, self, nil)
	src.mu.Lock()
	src.doc.Nodes[peer] = nodeRec(peer, 4, "peer")
	src.mu.Unlock()

	state := src.deleg.LocalState(false)

	// Decode into a fresh document and verify it matches.
	var got Document
	if err := json.Unmarshal(state, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Nodes[peer].Name != "peer" || got.Nodes[self] == nil {
		t.Fatal("local state round-trip lost records")
	}
}

func TestMergeRemoteStateMerges(t *testing.T) {
	self := id.New()
	peer := id.New()
	dst := newTestCluster(t, self, nil)

	remote := newDocument()
	remote.Nodes[peer] = nodeRec(peer, 1, "peer")
	buf, _ := json.Marshal(remote)
	dst.deleg.MergeRemoteState(buf, true)

	dst.mu.Lock()
	_, ok := dst.doc.Nodes[peer]
	dst.mu.Unlock()
	if !ok {
		t.Fatal("MergeRemoteState did not merge peer")
	}
}

func TestEventDelegateLiveness(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	n := &memberlist.Node{Name: peer.String(), Addr: net.ParseIP("10.0.0.3")}

	c.deleg.NotifyJoin(n)
	alive, _ := c.live.snapshot()
	if !alive[peer] {
		t.Fatal("join should mark alive")
	}
	c.deleg.NotifyLeave(n)
	alive, _ = c.live.snapshot()
	if alive[peer] {
		t.Fatal("leave should mark dead")
	}
	c.deleg.NotifyUpdate(n)
	alive, _ = c.live.snapshot()
	if !alive[peer] {
		t.Fatal("update should mark alive")
	}
}

func TestEventJoinObservesIP(t *testing.T) {
	self := id.New()
	peer := id.New()
	c := newTestCluster(t, self, nil)
	n := &memberlist.Node{Name: peer.String(), Addr: net.ParseIP("192.168.5.5")}
	c.deleg.NotifyJoin(n)

	obs := c.Snapshot().Nodes[0].Observed
	if e, ok := obs[peer]; !ok || e.IP != "192.168.5.5" {
		t.Fatalf("join should observe peer IP: %+v", obs)
	}
}

func TestNodeMetaCarriesID(t *testing.T) {
	self := id.New()
	c := newTestCluster(t, self, nil)
	meta := c.deleg.NodeMeta(512)
	var got id.ID
	copy(got[:], meta)
	if got != self {
		t.Fatal("NodeMeta should carry our id")
	}
	// Recover via peerID through Meta path (empty Name).
	pid, ok := peerID(&memberlist.Node{Meta: meta})
	if !ok || pid != self {
		t.Fatal("peerID should recover id from Meta")
	}
}
