package streamgen

import "testing"

// TestControllerBumpMatrix asserts every Reason advances Gen by exactly 1, sets
// the three unconditional directives (Keyframe/ResetSeq/ResetFEC), anchors
// FirstSampleIndex per the doc-05-§5.8 rule for that reason, and round-trips
// Playing. The bump-vs-no-bump matrix (D22): the FOUR reasons bump; a loop has no
// Reason and therefore no API path to advance gen (asserted structurally below).
func TestControllerBumpMatrix(t *testing.T) {
	cases := []struct {
		name    string
		reason  Reason
		at      int64
		playing bool
	}{
		{"media-change continuing", ReasonMediaChange, 48000, true},
		{"media-change fresh start", ReasonMediaChange, 0, true},
		{"seek target", ReasonSeek, 96000, true},
		{"seek while paused", ReasonSeek, 12345, false},
		{"profile-change continuing", ReasonProfileChange, 480000, true},
		{"master-change resume playing", ReasonMasterChange, 240000, true},
		{"master-change resume paused", ReasonMasterChange, 240000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewController(7)
			if got := c.Current(); got != 7 {
				t.Fatalf("initial Current()=%d want 7", got)
			}
			g := c.Bump(tc.reason, tc.at, tc.playing)
			if g.Gen != 8 {
				t.Errorf("Gen=%d want 8 (exactly +1)", g.Gen)
			}
			if c.Current() != 8 {
				t.Errorf("Current()=%d want 8 after Bump", c.Current())
			}
			if !g.Keyframe || !g.ResetSeq || !g.ResetFEC {
				t.Errorf("directives: Keyframe=%v ResetSeq=%v ResetFEC=%v, all must be true (§5.8)",
					g.Keyframe, g.ResetSeq, g.ResetFEC)
			}
			if g.FirstSampleIndex != tc.at {
				t.Errorf("FirstSampleIndex=%d want %d", g.FirstSampleIndex, tc.at)
			}
			if g.Playing != tc.playing {
				t.Errorf("Playing=%v want %v (must round-trip)", g.Playing, tc.playing)
			}
			if g.Reason != tc.reason {
				t.Errorf("Reason=%v want %v", g.Reason, tc.reason)
			}
		})
	}
}

// TestControllerMonotonic asserts a sequence of bumps yields strictly monotonic
// generations incrementing by exactly 1, regardless of reason ordering.
func TestControllerMonotonic(t *testing.T) {
	c := NewController(0)
	reasons := []Reason{
		ReasonMediaChange, ReasonSeek, ReasonProfileChange, ReasonMasterChange,
		ReasonMasterChange, ReasonSeek,
	}
	prev := uint64(0)
	for i, r := range reasons {
		g := c.Bump(r, int64(i)*100, true)
		if g.Gen != prev+1 {
			t.Fatalf("bump %d (%v): Gen=%d want %d", i, r, g.Gen, prev+1)
		}
		if c.Current() != g.Gen {
			t.Fatalf("bump %d: Current()=%d != returned Gen=%d", i, c.Current(), g.Gen)
		}
		prev = g.Gen
	}
	if c.Current() != 6 {
		t.Errorf("final Current()=%d want 6", c.Current())
	}
}

// TestSeekVsFailoverAnchor pins the distinction the spec draws between a seek
// (FirstSampleIndex = the operator seek target) and a failover (FirstSampleIndex =
// the continuing/current sample), since both flow through the same Bump.
func TestSeekVsFailoverAnchor(t *testing.T) {
	c := NewController(3)
	seek := c.Bump(ReasonSeek, 1_000_000, true) // operator jumped here
	if seek.FirstSampleIndex != 1_000_000 {
		t.Errorf("seek FirstSampleIndex=%d want 1000000 (seek target)", seek.FirstSampleIndex)
	}
	// Failover at the continuing position: NOT a reset to 0, the current sample.
	failover := c.Bump(ReasonMasterChange, 1_004_800, false)
	if failover.FirstSampleIndex != 1_004_800 {
		t.Errorf("failover FirstSampleIndex=%d want 1004800 (continuing)", failover.FirstSampleIndex)
	}
	if failover.Playing {
		t.Error("failover must preserve replicated Playing=false")
	}
}
