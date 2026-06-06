package streamgen

import "testing"

// TestGateAccept is the adopt-newer / pass-equal / drop-older matrix (doc 05 §5.8)
// plus the gen-advance discipline: gen changes ONLY on Adopt, and Adopt jumps the
// gate straight to the inbound value (a receiver may have missed a generation).
func TestGateAccept(t *testing.T) {
	cases := []struct {
		name     string
		startGen uint64
		inbound  uint64
		want     Action
		afterGen uint64
	}{
		{"first packet at gen0 passes", 0, 0, Pass, 0},
		{"first live gen adopts from 0", 0, 7, Adopt, 7},
		{"equal passes, no advance", 5, 5, Pass, 5},
		{"older straggler drops", 5, 4, Drop, 5},
		{"much older straggler drops", 5, 1, Drop, 5},
		{"newer adopts +1", 5, 6, Adopt, 6},
		{"newer adopts jumping >1 (missed a gen)", 5, 9, Adopt, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Gate{gen: tc.startGen}
			got := g.Accept(tc.inbound)
			if got != tc.want {
				t.Errorf("Accept(%d) from gen %d = %v, want %v", tc.inbound, tc.startGen, got, tc.want)
			}
			if g.Current() != tc.afterGen {
				t.Errorf("after Accept, Current()=%d want %d", g.Current(), tc.afterGen)
			}
		})
	}
}

// TestGateStragglerAfterAdopt is the canonical monotonic-disambiguation case: a
// straggler from the prior generation arriving AFTER an adopt is dropped, and the
// gate stays on the new generation (doc 05 §5.8).
func TestGateStragglerAfterAdopt(t *testing.T) {
	g := NewGate()
	if got := g.Accept(7); got != Adopt { // lock onto generation 7
		t.Fatalf("first Accept(7)=%v want Adopt", got)
	}
	if got := g.Accept(7); got != Pass { // steady state on 7
		t.Fatalf("Accept(7) steady=%v want Pass", got)
	}
	if got := g.Accept(8); got != Adopt { // seek bumps to 8
		t.Fatalf("Accept(8)=%v want Adopt", got)
	}
	// A gen-7 chunk reordered late on the network after the bump: must Drop.
	if got := g.Accept(7); got != Drop {
		t.Errorf("post-adopt gen-7 straggler=%v want Drop", got)
	}
	// And a duplicate gen-8 just passes; the gate does not bounce.
	if got := g.Accept(8); got != Pass {
		t.Errorf("Accept(8) after adopt=%v want Pass", got)
	}
	if g.Current() != 8 {
		t.Errorf("Current()=%d want 8 (straggler must not rewind the gate)", g.Current())
	}
}

// TestNewGateZero documents the starting state.
func TestNewGateZero(t *testing.T) {
	if g := NewGate(); g.Current() != 0 {
		t.Errorf("NewGate Current()=%d want 0", g.Current())
	}
}
