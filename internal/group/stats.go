package group

import (
	"time"

	"ensemble/internal/id"
)

// logCompositionLocked diffs this node's freshly-derived group view against the
// previously observed one and logs membership joins/leaves, master changes, and
// our own role changes at INFO. Caller holds e.mu.
func (e *Engine) logCompositionLocked(mv myView) {
	members := make(map[id.ID]bool, len(mv.group.Members))
	for _, m := range mv.group.Members {
		members[m] = true
	}

	if !e.havePrev {
		e.havePrev = true
		e.prevRole = mv.role
		e.prevMaster = mv.master
		e.prevMembers = members
		e.log.Info("group composition",
			"group", mv.group.ID.String(), "role", mv.role.String(),
			"master", mv.master.String(), "members", len(members))
		return
	}

	for m := range members {
		if !e.prevMembers[m] {
			e.log.Info("group member joined", "group", mv.group.ID.String(), "member", m.String())
		}
	}
	for m := range e.prevMembers {
		if !members[m] {
			e.log.Info("group member left", "group", mv.group.ID.String(), "member", m.String())
		}
	}
	if mv.master != e.prevMaster {
		e.log.Info("group master changed", "group", mv.group.ID.String(),
			"from", e.prevMaster.String(), "to", mv.master.String())
	}
	if mv.role != e.prevRole {
		e.log.Info("role changed", "group", mv.group.ID.String(),
			"from", e.prevRole.String(), "to", mv.role.String())
	}

	e.prevRole = mv.role
	e.prevMaster = mv.master
	e.prevMembers = members
}

// logPlayingStatsLocked emits the master-side 1 Hz playing-stats line while this
// node runs a session. One INFO line per second; silent when idle. Caller holds
// e.mu. (The member side is logged from K's wiring, which owns the sink/clock/
// client directly.)
func (e *Engine) logPlayingStatsLocked(mv myView, isMaster bool, now time.Time) {
	if e.sess == nil || !isMaster {
		return
	}
	if now.Before(e.lastStats.Add(time.Second)) {
		return
	}
	e.lastStats = now

	st := e.p.Source.Stats()
	e.log.Info("playing",
		"side", "master",
		"gen", e.sess.gen.Load(),
		"released", st.Released,
		"clients", st.Clients,
		"parity", st.Parity,
		"restarts", st.Restarts,
		"primes", st.Primes,
	)
}
