package group

import (
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/clock"
)

func TestClockSourceAdapters(t *testing.T) {
	// master / orphan are constant; follower delegates to a real clock.Follower.
	m := MasterClock()
	if off, ok := m.Offset(); off != 0 || !ok {
		t.Errorf("master Offset = (%v,%v), want (0,true)", off, ok)
	}
	if d, ok := m.MinDelay(); d != 0 || !ok {
		t.Errorf("master MinDelay = (%v,%v), want (0,true)", d, ok)
	}

	o := OrphanClock()
	if off, ok := o.Offset(); off != 0 || ok {
		t.Errorf("orphan Offset = (%v,%v), want (0,false)", off, ok)
	}
	if d, ok := o.MinDelay(); d != 0 || ok {
		t.Errorf("orphan MinDelay = (%v,%v), want (0,false)", d, ok)
	}

	// Follower with no samples yet ⇒ delegates ok=false (matches doc 04 §4.1.3
	// orphan-until-locked behavior at the clock layer).
	f := clock.NewFollower(clock.WithEstimator(16, 0.10))
	fc := FollowerClock(f)
	if _, ok := fc.Offset(); ok {
		t.Errorf("fresh follower Offset ok=true; want false until first sample")
	}
	if _, ok := fc.MinDelay(); ok {
		t.Errorf("fresh follower MinDelay ok=true; want false")
	}
	_ = time.Duration(0)
}

func TestClockSourceInterfaceSatisfied(t *testing.T) {
	var _ ClockSource = MasterClock()
	var _ ClockSource = OrphanClock()
	var _ ClockSource = FollowerClock(clock.NewFollower())
}
