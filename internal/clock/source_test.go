package clock

import (
	"testing"
	"time"
)

// clockSource mirrors the canonical README §6.2 ClockSource interface. The spine
// owns the real definition; this local alias only proves the three adapters (and
// *Follower) satisfy that shape structurally.
type clockSource interface {
	Offset() (time.Duration, bool)
	MinDelay() (time.Duration, bool)
}

// Compile-time interface satisfaction (test plan §7.2).
var (
	_ clockSource = MasterSource{}
	_ clockSource = OrphanSource{}
	_ clockSource = FollowerSource{}
	_ clockSource = (*Follower)(nil)
)

func TestClockSourceAdapters(t *testing.T) {
	tests := []struct {
		name      string
		src       clockSource
		wantOff   time.Duration
		wantOffOK bool
		wantMD    time.Duration
		wantMDOK  bool
	}{
		{"master", MasterSource{}, 0, true, 0, true},
		{"orphan", OrphanSource{}, 0, false, 0, false},
		// FollowerSource over an un-run Follower: pass-through ok=false.
		{"follower no samples", FollowerSource{F: NewFollower()}, 0, false, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			off, ok := tc.src.Offset()
			if off != tc.wantOff || ok != tc.wantOffOK {
				t.Errorf("Offset() = (%v, %v), want (%v, %v)", off, ok, tc.wantOff, tc.wantOffOK)
			}
			md, ok := tc.src.MinDelay()
			if md != tc.wantMD || ok != tc.wantMDOK {
				t.Errorf("MinDelay() = (%v, %v), want (%v, %v)", md, ok, tc.wantMD, tc.wantMDOK)
			}
		})
	}
}

func TestFollowerSourcePassThrough(t *testing.T) {
	f := NewFollower()
	// Drive the underlying estimator directly (no socket needed).
	f.est.Add(Sample{Offset: 2_000_000, Delay: 400_000})
	src := FollowerSource{F: f}

	wantOff, wantOK := f.Offset()
	gotOff, gotOK := src.Offset()
	if gotOff != wantOff || gotOK != wantOK {
		t.Errorf("Offset pass-through = (%v, %v), want (%v, %v)", gotOff, gotOK, wantOff, wantOK)
	}
	if !gotOK {
		t.Fatal("expected ok=true after a sample")
	}

	wantMD, wantMDOK := f.MinDelay()
	gotMD, gotMDOK := src.MinDelay()
	if gotMD != wantMD || gotMDOK != wantMDOK {
		t.Errorf("MinDelay pass-through = (%v, %v), want (%v, %v)", gotMD, gotMDOK, wantMD, wantMDOK)
	}
}
