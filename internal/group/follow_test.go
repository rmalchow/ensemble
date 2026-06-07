package group

import (
	"errors"
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

func TestFollowAliveMaster(t *testing.T) {
	self, target := idN(1), idN(2)
	r := newRig(self, 0, false)
	// self is solo; target is a separate alive master.
	snap := soloSnap(self)
	snap.Nodes = append(snap.Nodes, node(target, id.Zero, true))
	r.cl.setSnap(snap)

	if err := r.e.Follow(target); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got, ok := r.cl.lastFollowing()
	if !ok || got != target {
		t.Fatalf("SetFollowing = %v,%v want %v", got, ok, target)
	}
}

func TestFollowRejectsSelf(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Follow(self); !errors.Is(err, ErrSelfFollow) {
		t.Fatalf("err = %v, want ErrSelfFollow", err)
	}
	if r.cl.followCount() != 0 {
		t.Fatal("no setter expected")
	}
}

func TestFollowRejectsUnknown(t *testing.T) {
	self, target := idN(1), idN(2)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self)) // target absent
	if err := r.e.Follow(target); !errors.Is(err, ErrTargetUnknown) {
		t.Fatalf("err = %v, want ErrTargetUnknown", err)
	}
}

func TestFollowRejectsDead(t *testing.T) {
	self, target := idN(1), idN(2)
	r := newRig(self, 0, false)
	snap := soloSnap(self)
	snap.Nodes = append(snap.Nodes, node(target, id.Zero, false))
	r.cl.setSnap(snap)
	if err := r.e.Follow(target); !errors.Is(err, ErrTargetDead) {
		t.Fatalf("err = %v, want ErrTargetDead", err)
	}
}

func TestFollowRejectsFollower(t *testing.T) {
	self, target, other := idN(1), idN(2), idN(3)
	r := newRig(self, 0, false)
	snap := soloSnap(self)
	snap.Nodes = append(snap.Nodes, node(target, other, true)) // target follows other
	r.cl.setSnap(snap)
	if err := r.e.Follow(target); !errors.Is(err, ErrTargetFollower) {
		t.Fatalf("err = %v, want ErrTargetFollower", err)
	}
}

func TestFollowRepoint(t *testing.T) {
	self, a, b := idN(1), idN(2), idN(3)
	r := newRig(self, 0, false)
	// self currently follows a; b is also a master. Re-point to b is allowed.
	snap := contracts.Snapshot{
		Nodes: []contracts.NodeView{
			node(self, a, true),
			node(a, id.Zero, true),
			node(b, id.Zero, true),
		},
		Groups: []contracts.GroupView{{ID: id.XOR(a, self), Master: a, Members: []id.ID{a, self}}},
	}
	r.cl.setSnap(snap)
	if err := r.e.Follow(b); err != nil {
		t.Fatalf("Follow(b): %v", err)
	}
	got, _ := r.cl.lastFollowing()
	if got != b {
		t.Fatalf("SetFollowing = %v, want %v", got, b)
	}
}

func TestUnfollow(t *testing.T) {
	self := idN(1)
	r := newRig(self, 0, false)
	r.cl.setSnap(soloSnap(self))
	if err := r.e.Unfollow(); err != nil {
		t.Fatalf("Unfollow: %v", err)
	}
	got, ok := r.cl.lastFollowing()
	if !ok || got != id.Zero {
		t.Fatalf("SetFollowing = %v,%v want Zero", got, ok)
	}
}
