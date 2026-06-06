package clock

// ClockSource adapters (doc 04 §4.1.3, README §6.2). The canonical ClockSource
// interface lives in the spine; clock does not import it (it is satisfied
// structurally / duck-typed by consumers). These three adapters let cmd/group
// hold one uniform interface across the master, follower, and orphan roles and
// feed it to the Timeline projection (doc 04 §4.4.3): master_now = NowMono()+Offset.

import "time"

// MasterSource is the master/solo self-reference: the master IS the timebase, so
// its offset and delay are exactly zero (doc 04 §4.1.3 row `master`).
type MasterSource struct{}

// Offset returns (0, true): the master's offset against itself is exactly zero.
func (MasterSource) Offset() (time.Duration, bool) { return 0, true }

// MinDelay returns (0, true): the reference clock has no measurement delay.
func (MasterSource) MinDelay() (time.Duration, bool) { return 0, true }

// FollowerSource adapts a running *Follower to ClockSource (doc 04 §4.1.3 row
// `follower`). A thin pass-through; *Follower already has the Offset/MinDelay
// shape, but the named adapter lets callers hold the interface uniformly.
type FollowerSource struct{ F *Follower }

// Offset passes through the Follower's EWMA offset (master - follower).
func (s FollowerSource) Offset() (time.Duration, bool) { return s.F.Offset() }

// MinDelay passes through the Follower's sync-quality proxy.
func (s FollowerSource) MinDelay() (time.Duration, bool) { return s.F.MinDelay() }

// OrphanSource is the no-estimate-yet zero value: ok=false on both (doc 04
// §4.1.3 row `orphan`). A consumer seeing ok=false holds render (doc 04 §4.2.3).
type OrphanSource struct{}

// Offset returns (0, false): no timebase yet, render is held.
func (OrphanSource) Offset() (time.Duration, bool) { return 0, false }

// MinDelay returns (0, false): no sync-quality measurement yet.
func (OrphanSource) MinDelay() (time.Duration, bool) { return 0, false }
