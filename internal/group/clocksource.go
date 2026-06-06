package group

// ClockSource adapters (README §6.2, doc 04 §4.1.3). The group engine holds one
// uniform ClockSource across the three roles and feeds it to the Timeline
// projection (doc 04 §4.4.3: master_now = NowMono()+Offset). internal/clock
// already provides structurally-identical adapters; group re-exposes them under
// the canonical group API names so downstream callers (cmd, render) depend only
// on internal/group's contract.
//
//	role     Offset()    MinDelay()
//	master   0, true     0, true       (the reference is exact)
//	follower f.Offset()  f.MinDelay()  (EWMA + min-delay filter, A.1)
//	orphan   0, false    0, false      (no estimate yet ⇒ render holds, §4.2.3)

import (
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/clock"
)

// ClockSource is the per-group clock projection contract (README §6.2). Do not
// redefine.
type ClockSource interface {
	Offset() (d time.Duration, ok bool)
	MinDelay() (time.Duration, bool)
}

// masterClock is the master/solo self-reference: it IS the timebase.
type masterClock struct{}

func (masterClock) Offset() (time.Duration, bool)   { return 0, true }
func (masterClock) MinDelay() (time.Duration, bool) { return 0, true }

// followerClock wraps a running *clock.Follower (doc 04 §4.1.3 row follower).
type followerClock struct{ f *clock.Follower }

func (c followerClock) Offset() (time.Duration, bool)   { return c.f.Offset() }
func (c followerClock) MinDelay() (time.Duration, bool) { return c.f.MinDelay() }

// orphanClock is the no-estimate-yet zero value: ok=false on both.
type orphanClock struct{}

func (orphanClock) Offset() (time.Duration, bool)   { return 0, false }
func (orphanClock) MinDelay() (time.Duration, bool) { return 0, false }

// MasterClock returns the master self-reference clock source ((0,true)/(0,true)).
func MasterClock() ClockSource { return masterClock{} }

// FollowerClock wraps a running clock.Follower (P3.1) as a ClockSource.
func FollowerClock(f *clock.Follower) ClockSource { return followerClock{f: f} }

// OrphanClock returns the no-sync clock source ((0,false)/(0,false)).
func OrphanClock() ClockSource { return orphanClock{} }
