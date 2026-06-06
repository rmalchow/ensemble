package group

import (
	"testing"
)

// fakeResumer records ResumeAt calls and returns a monotonically bumped gen, so
// SeedAndResume's bump + anchor threading can be asserted without a real origin.
type fakeResumer struct {
	gen     uint64
	calls   int
	lastIdx int64
	lastPl  bool
}

func (f *fakeResumer) ResumeAt(sampleIndex int64, playing bool) uint64 {
	f.calls++
	f.lastIdx = sampleIndex
	f.lastPl = playing
	f.gen++
	return f.gen
}

// TestSeedAndResumeContinuity: promotion seeds the timeline from lastSample (not
// 0) and drives ResumeAt with the same anchor + playing flag (01 §4.2 / R4).
func TestSeedAndResumeContinuity(t *testing.T) {
	tests := []struct {
		name       string
		lastSample int64
		playing    bool
	}{
		{"playing mid-stream", 480000, true},
		{"paused", 96000, false},
		{"cold promotion at 0", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tl := NewMasterTimeline(48000)
			o := &fakeResumer{gen: 7}

			gotGen := SeedAndResume(tl, o, tc.lastSample, tc.playing)

			// Timeline seeded from lastSample, not reset to 0.
			s, playing, ok := tl.NowSample()
			if !ok {
				t.Fatal("master timeline ok=false")
			}
			if !tc.playing && s != tc.lastSample {
				t.Errorf("paused timeline at %d, want %d (seed continuity)", s, tc.lastSample)
			}
			if tc.playing && s < tc.lastSample {
				t.Errorf("playing timeline at %d, want >= %d (no reset to 0)", s, tc.lastSample)
			}
			if playing != tc.playing {
				t.Errorf("timeline playing=%v, want %v (R4)", playing, tc.playing)
			}

			// ResumeAt called exactly once with the anchor + playing flag.
			if o.calls != 1 {
				t.Errorf("ResumeAt calls=%d, want 1", o.calls)
			}
			if o.lastIdx != tc.lastSample {
				t.Errorf("ResumeAt idx=%d, want %d", o.lastIdx, tc.lastSample)
			}
			if o.lastPl != tc.playing {
				t.Errorf("ResumeAt playing=%v, want %v", o.lastPl, tc.playing)
			}
			if gotGen != o.gen {
				t.Errorf("returned gen=%d, want %d", gotGen, o.gen)
			}
		})
	}
}

// TestSeedAndResumeAlwaysBumps: master failover ALWAYS bumps streamGen, even when
// the media is identical (R11 / 05 §5.8 — there is no "continue same gen" path).
func TestSeedAndResumeAlwaysBumps(t *testing.T) {
	tl := NewMasterTimeline(48000)
	o := &fakeResumer{gen: 42}
	before := o.gen
	got := SeedAndResume(tl, o, 1000, true)
	if got <= before {
		t.Errorf("failover gen=%d, want > %d (always bump, R11)", got, before)
	}
}

// TestSeedAndResumeNilOrigin: a nil origin is tolerated (timeline still seeds);
// returns 0. Guards the wiring during a partial-startup window.
func TestSeedAndResumeNilOrigin(t *testing.T) {
	tl := NewMasterTimeline(48000)
	if got := SeedAndResume(tl, nil, 5000, false); got != 0 {
		t.Errorf("nil-origin gen=%d, want 0", got)
	}
	if s, _, _ := tl.NowSample(); s != 5000 {
		t.Errorf("timeline not seeded with nil origin: %d, want 5000", s)
	}
}

// TestSeedAndResumeGapBound (modeled): with the canonical lead (300 ms, A.12), the
// continuity anchor means the resume position equals where the follower left off,
// so the only gap is the suspicion window + one buffer refill — never a rewind to
// 0. We model this by asserting the post-promotion timeline position is within one
// lead of the pre-promotion position (no permanent desync). The suspicion-window
// term is a memberlist property, not owned here (Q5).
func TestSeedAndResumeGapBound(t *testing.T) {
	const rate = 48000
	const lastSample = int64(1_440_000) // 30 s in
	tl := NewMasterTimeline(rate)
	o := &fakeResumer{}

	SeedAndResume(tl, o, lastSample, true)

	// Immediately after promotion the timeline reads ~lastSample (it advances at
	// the crystal rate from the seed instant). The resume gap relative to the
	// follower's last position is bounded by one buffer lead = 300 ms of frames.
	s, _, _ := tl.NowSample()
	leadFrames := int64(300) * rate / 1000 // 14400 frames
	if s < lastSample || s > lastSample+leadFrames {
		t.Errorf("resume sample=%d, want within [%d, %d] (bounded gap, no reset)",
			s, lastSample, lastSample+leadFrames)
	}
}
