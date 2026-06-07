package group

import (
	"time"

	"ensemble/internal/id"
)

// reconcileHeal implements the 10 s self-heal grace (§5). Called under e.mu from
// the reconcile loop after each myGroup.
//
// A node whose Following points at an invalid target already behaves as solo for
// derivation (C, D5) immediately; only the write-back reset of its own Following
// waits Grace, so a momentary master flap does not destroy the follow.
func (e *Engine) reconcileHeal(mv myView, now time.Time) {
	if !mv.stale {
		// Valid follower, or already solo with empty Following: cancel any
		// pending heal (the master flapped back).
		e.healAt = time.Time{}
		return
	}
	if e.healAt.IsZero() {
		e.healAt = now.Add(e.p.Grace)
		return
	}
	if !now.Before(e.healAt) {
		e.p.Cluster.SetFollowing(id.Zero)
		e.healAt = time.Time{}
		e.log.Info("self-healed dangling follow to solo")
	}
}
