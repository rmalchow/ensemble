package group

import (
	"testing"
	"time"

	"ensemble/internal/id"
)

// TestMasterRePointsPlaybackOnGroupIDChange is the regression test for the
// fan-out bug: when members leave, the group ID (XOR of the member set) changes;
// the master must re-write its playback record under the NEW group id and clear
// the OLD one, so the surviving members don't see state="idle" and stop.
func TestMasterRePointsPlaybackOnGroupIDChange(t *testing.T) {
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

	fullGID := id.XOR(self, b, c)
	if r.e.sess.groupID != fullGID {
		t.Fatalf("session groupID = %v, want full %v", r.e.sess.groupID, fullGID)
	}

	// b and c disconnect → group is now just {self}; its ID changes.
	r.cl.setSnap(withPlaying(soloSnap(self)))
	soloGID := id.XOR(self)

	// Advance the heartbeat clock so the reconcile-driven re-point fires and is
	// distinguishable, then reconcile.
	r.advance(100 * time.Millisecond)
	r.e.reconcile()

	if r.e.sess.groupID != soloGID {
		t.Fatalf("session groupID after members left = %v, want solo %v", r.e.sess.groupID, soloGID)
	}

	// The most recent playback write must be PLAYING under the NEW (solo) group id.
	pc, ok := r.cl.lastPlayback()
	if !ok {
		t.Fatal("no playback written")
	}
	if pc.group != soloGID || pc.pb.State != "playing" {
		t.Fatalf("last playback = group %v state %q, want solo playing", pc.group, pc.pb.State)
	}

	// The OLD group id must have been cleared to idle (somewhere in the history).
	clearedOld := false
	for _, p := range r.cl.playbackHistory() {
		if p.group == fullGID && p.pb.State == "idle" {
			clearedOld = true
		}
	}
	if !clearedOld {
		t.Fatal("old group id playback not cleared to idle")
	}
}

// playbackHistory exposes the full record of SetPlayback calls for assertions.
func (f *fakeCluster) playbackHistory() []playbackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]playbackCall(nil), f.playback...)
}
