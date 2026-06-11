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

// The queue ITEMS are no longer gossiped (they'd blow memberlist's UDP packet);
// only the length + a change marker ride the playback record, and the UI pulls the
// contents from the master on demand. These tests pin that the small markers
// replicate.
func TestSetPlaybackCarriesQueueMarkersIntoSnapshot(t *testing.T) {
	self := id.New()
	c := newTestCluster(t, self, nil)
	c.SetPlayback(self, contracts.Playback{
		State:    "playing",
		URI:      "file:a.mp3",
		Metadata: &contracts.TrackMetadata{Title: "A"},
		QueueLen: 2,
		QueueRev: 7,
	})

	pb := groupPlayback(c.Snapshot(), self)
	if pb.QueueLen != 2 {
		t.Fatalf("queueLen = %d, want 2", pb.QueueLen)
	}
	if pb.QueueRev != 7 {
		t.Fatalf("queueRev = %d, want 7", pb.QueueRev)
	}
}

func TestPlaybackQueueMarkersSurviveMergeAndClone(t *testing.T) {
	g := id.New()
	remote := newDocument()
	remote.Playback[g] = &PlaybackRecord{
		State:    "playing",
		URI:      "file:a.mp3",
		QueueLen: 2,
		QueueRev: 5,
		Version:  1,
		Writer:   id.New(),
	}

	d := newDocument()
	if !d.mergeAll(id.New(), remote) {
		t.Fatal("merge reported no change")
	}
	if got := d.Playback[g]; got == nil || got.QueueLen != 2 || got.QueueRev != 5 {
		t.Fatalf("markers not merged: %+v", d.Playback[g])
	}

	cl := d.clone()
	if got := cl.Playback[g]; got == nil || got.QueueLen != 2 || got.QueueRev != 5 {
		t.Fatalf("markers not cloned: %+v", cl.Playback[g])
	}
}
