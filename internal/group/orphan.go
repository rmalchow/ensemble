package group

// orphan hysteresis (doc 04 §4.2.3): "silence > skew". A follower enters orphan
// when MinDelay() stays over a HIGH enter threshold for N consecutive windows,
// and only exits when MinDelay() drops under a LOWER exit threshold. The
// enter≠exit gap plus the N-window requirement keep a single WiFi spike (the
// 10-50 ms envelope of doc 04 §4.1.4) from flapping the role.
//
// A.12 pins the clock window/alpha/ping but NOT the orphan thresholds (P3.2 risk
// 1); the values below are the doc-proposed defaults pending orchestrator
// ratification — see the report. They are package consts so a single edit
// updates them when A.12 is amended.

import "time"

const (
	// orphanEnterThreshold: MinDelay above this for orphanEnterWindows
	// consecutive windows trips orphan (T6).
	orphanEnterThreshold = 15 * time.Millisecond
	// orphanExitThreshold: MinDelay must drop below this to leave orphan (T7).
	orphanExitThreshold = 8 * time.Millisecond
	// orphanEnterWindows: consecutive over-threshold windows required to enter.
	orphanEnterWindows = 3
)

// orphanGate is the hysteretic MinDelay filter. The zero value is a healthy
// (non-orphan) gate with no over-threshold history.
type orphanGate struct {
	orphaned  bool
	overCount int // consecutive windows with MinDelay >= enter threshold
}

// observe feeds one window's clock health and returns whether the follower
// should be held in orphan after this observation. ok is Offset().ok; ok=false
// (no master / no offset yet) forces orphan immediately. minDelay is the window's
// MinDelay() and minOK whether a delay measurement exists yet.
//
// Determinism note: this is the only stateful (hysteretic) part of the engine;
// Recompute consults the resolved boolean via Inputs.MinDelayOK so the transition
// function itself stays pure (doc 04 §4.2.2).
func (g *orphanGate) observe(offsetOK bool, minDelay time.Duration, minOK bool) bool {
	if !offsetOK || !minOK {
		// No usable sync at all ⇒ orphan, and any over-streak is moot.
		g.overCount = 0
		g.orphaned = true
		return true
	}

	if minDelay >= orphanEnterThreshold {
		if g.overCount < orphanEnterWindows {
			g.overCount++
		}
	} else {
		g.overCount = 0
	}

	if g.orphaned {
		// Exit only when comfortably below the lower threshold (hysteresis).
		if minDelay < orphanExitThreshold {
			g.orphaned = false
		}
	} else if g.overCount >= orphanEnterWindows {
		g.orphaned = true
	}
	return g.orphaned
}

// reset clears the gate to healthy state (used when leaving follower/orphan, so
// a later re-entry starts with a clean window history).
func (g *orphanGate) reset() {
	g.orphaned = false
	g.overCount = 0
}
