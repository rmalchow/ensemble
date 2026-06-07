package group

import (
	"testing"
	"time"
)

// TestGroupIDStableAcrossMembershipChurn verifies D42: the group id is the
// MASTER's node id, so it does NOT drift when members join/leave. The master's
// session keeps its groupID and never re-points the playback record across churn
// (the old XOR-id re-point block is removed). This supersedes the former
// TestMasterRePointsPlaybackOnGroupIDChange regression.
func TestGroupIDStableAcrossMembershipChurn(t *testing.T) {
	self, b, c := idN(1), idN(2), idN(3)
	r := newRig(self, 1_000_000, true) // endless live source
	r.now = time.Unix(1_700_000_000, 0)

	// self masters a 3-node group {self,b,c}; play.
	full := masterSnap(self, defaultSettings(), b, c)
	r.cl.setSnap(withPlaying(full))
	if err := r.e.Play("input:"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	defer r.e.Close()

	if r.e.sess.groupID != self {
		t.Fatalf("session groupID = %v, want master id %v", r.e.sess.groupID, self)
	}

	// b and c disconnect → group is now just {self}. Master is unchanged, so the
	// group id (master id) and playback record must be unchanged.
	r.cl.setSnap(withPlaying(soloSnap(self)))
	r.advance(100 * time.Millisecond)
	r.e.reconcile()

	if r.e.sess.groupID != self {
		t.Fatalf("session groupID changed across churn = %v, want stable %v", r.e.sess.groupID, self)
	}

	// No playback record was ever written under any id OTHER than the master id —
	// in particular, nothing was cleared to idle by a churn re-point.
	for _, p := range r.cl.playbackHistory() {
		if p.group != self {
			t.Fatalf("playback written under non-master id %v (churn re-point should be gone)", p.group)
		}
	}
}

// playbackHistory exposes the full record of SetPlayback calls for assertions.
func (f *fakeCluster) playbackHistory() []playbackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]playbackCall(nil), f.playback...)
}
