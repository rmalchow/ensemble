package group

import (
	"testing"
	"time"
)

func TestOrphanGate_Hysteresis(t *testing.T) {
	ms := time.Millisecond

	t.Run("single spike below N windows ⇒ stay follower", func(t *testing.T) {
		var g orphanGate
		// one isolated spike over the enter threshold, then recovery.
		if g.observe(true, 30*ms, true) {
			t.Fatalf("orphaned after 1 spike; need %d consecutive", orphanEnterWindows)
		}
		if g.observe(true, 2*ms, true) {
			t.Fatalf("orphaned after recovery")
		}
	})

	t.Run("sustained over enter for N windows ⇒ orphan", func(t *testing.T) {
		var g orphanGate
		for i := 0; i < orphanEnterWindows-1; i++ {
			if g.observe(true, 20*ms, true) {
				t.Fatalf("orphaned early at window %d", i)
			}
		}
		if !g.observe(true, 20*ms, true) {
			t.Fatalf("not orphaned after %d windows over enter threshold", orphanEnterWindows)
		}
	})

	t.Run("hysteresis: between exit and enter keeps orphan", func(t *testing.T) {
		var g orphanGate
		for i := 0; i < orphanEnterWindows; i++ {
			g.observe(true, 20*ms, true)
		}
		if !g.orphaned {
			t.Fatalf("setup: expected orphan")
		}
		// 10ms is below enter (15) but above exit (8): must remain orphan.
		if !g.observe(true, 10*ms, true) {
			t.Errorf("left orphan in the hysteresis band (exit<10ms<enter)")
		}
	})

	t.Run("recovery under exit threshold ⇒ follower", func(t *testing.T) {
		var g orphanGate
		for i := 0; i < orphanEnterWindows; i++ {
			g.observe(true, 20*ms, true)
		}
		if g.observe(true, 5*ms, true) {
			t.Errorf("still orphaned after MinDelay dropped under exit threshold")
		}
	})

	t.Run("offset not ok forces orphan immediately", func(t *testing.T) {
		var g orphanGate
		if !g.observe(false, 0, false) {
			t.Errorf("offset !ok must orphan immediately")
		}
	})

	t.Run("no min-delay measurement forces orphan", func(t *testing.T) {
		var g orphanGate
		if !g.observe(true, 0, false) {
			t.Errorf("missing MinDelay must orphan")
		}
	})
}
