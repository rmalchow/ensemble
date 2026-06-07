package group

import (
	"testing"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// staleMV builds a myView with stale=true (dangling follow).
func staleMV(self id.ID) myView {
	n := node(self, idN(9), true)
	g := contracts.GroupView{ID: id.XOR(self), Master: self, Members: []id.ID{self}}
	snap := contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}}
	return myGroup(snap, self)
}

func TestHealNoResetBeforeGrace(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	mv := staleMV(self)

	now := r.now
	r.e.reconcileHeal(mv, now)                    // arms healAt = now+10s
	r.e.reconcileHeal(mv, now.Add(9*time.Second)) // still before
	if r.cl.followCount() != 0 {
		t.Fatal("no reset expected before grace")
	}
}

func TestHealResetsAfterGrace(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	mv := staleMV(self)

	now := r.now
	r.e.reconcileHeal(mv, now)
	r.e.reconcileHeal(mv, now.Add(10*time.Second))
	got, ok := r.cl.lastFollowing()
	if !ok || got != id.Zero {
		t.Fatalf("SetFollowing = %v,%v want Zero", got, ok)
	}
	if !r.e.healAt.IsZero() {
		t.Fatal("healAt should be cleared")
	}
}

func TestHealCancelsWhenTargetRecovers(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	staleM := staleMV(self)
	now := r.now
	r.e.reconcileHeal(staleM, now) // arm

	// Target recovered: now a valid follower (not stale).
	master := idN(2)
	mv := myGroup(masterSnap(master, defaultSettings(), self), self)
	r.e.reconcileHeal(mv, now.Add(3*time.Second))
	if !r.e.healAt.IsZero() {
		t.Fatal("healAt should be cleared on recover")
	}

	// Even past the original grace, no reset (timer was cancelled).
	r.e.reconcileHeal(mv, now.Add(20*time.Second))
	if r.cl.followCount() != 0 {
		t.Fatal("no reset expected after cancel")
	}
}
