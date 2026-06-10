package cluster

import (
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// groupPlayback finds the playback record for the group mastered by master.
func groupPlayback(snap contracts.Snapshot, master id.ID) contracts.Playback {
	for _, g := range snap.Groups {
		if g.Master == master {
			return g.Playback
		}
	}
	return contracts.Playback{}
}

func TestSetPlaybackCarriesQueueIntoSnapshot(t *testing.T) {
	self := id.New()
	c := newTestCluster(t, self, nil)
	c.SetPlayback(self, contracts.Playback{
		State:    "playing",
		URI:      "file:a.mp3",
		Metadata: &contracts.TrackMetadata{Title: "A"},
		Queue: []contracts.QueueItem{
			{URI: "file:b.mp3", Metadata: &contracts.TrackMetadata{Title: "B", Artist: "Bee"}},
			{URI: "file:c.mp3"},
		},
	})

	pb := groupPlayback(c.Snapshot(), self)
	if len(pb.Queue) != 2 {
		t.Fatalf("queue len = %d, want 2", len(pb.Queue))
	}
	if pb.Queue[0].URI != "file:b.mp3" || pb.Queue[0].Metadata == nil || pb.Queue[0].Metadata.Artist != "Bee" {
		t.Fatalf("queue[0] = %+v", pb.Queue[0])
	}
	if pb.Queue[1].URI != "file:c.mp3" || pb.Queue[1].Metadata != nil {
		t.Fatalf("queue[1] = %+v", pb.Queue[1])
	}
}

func TestPlaybackQueueSurvivesMergeAndClone(t *testing.T) {
	g := id.New()
	remote := newDocument()
	remote.Playback[g] = &PlaybackRecord{
		State:   "playing",
		URI:     "file:a.mp3",
		Queue:   []contracts.QueueItem{{URI: "file:b.mp3"}, {URI: "file:c.mp3"}},
		Version: 1,
		Writer:  id.New(),
	}

	d := newDocument()
	if !d.mergeAll(id.New(), remote) {
		t.Fatal("merge reported no change")
	}
	if got := d.Playback[g]; got == nil || len(got.Queue) != 2 || got.Queue[1].URI != "file:c.mp3" {
		t.Fatalf("queue not merged: %+v", d.Playback[g])
	}

	cl := d.clone()
	if got := cl.Playback[g]; got == nil || len(got.Queue) != 2 {
		t.Fatalf("queue not cloned: %+v", cl.Playback[g])
	}
}
