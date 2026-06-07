package group

import (
	"context"
	"errors"
	"testing"

	"ensemble/internal/id"
)

func TestMakeMasterFanout(t *testing.T) {
	self, b, c := idN(1), idN(2), idN(3)
	r := newRig(self, 0, false)
	// self is master of {self, b, c}; make b the new master.
	r.cl.setSnap(masterSnap(self, defaultSettings(), b, c))

	if err := r.e.MakeMaster(context.Background(), b); err != nil {
		t.Fatalf("MakeMaster: %v", err)
	}
	calls := r.fc.snapshot()
	// Expect: Follow(c -> b) and Unfollow(b); self follows b locally (SetFollowing).
	var followC, unfollowB bool
	for _, cc := range calls {
		if cc.peer == c && cc.target == b && !cc.unfollow {
			followC = true
		}
		if cc.peer == b && cc.unfollow {
			unfollowB = true
		}
	}
	if !followC {
		t.Error("missing Follow(c -> b)")
	}
	if !unfollowB {
		t.Error("missing Unfollow(b)")
	}
	got, ok := r.cl.lastFollowing()
	if !ok || got != b {
		t.Fatalf("self SetFollowing = %v,%v want %v", got, ok, b)
	}
}

func TestMakeMasterSelfUsesLocalUnfollow(t *testing.T) {
	self, b := idN(1), idN(2)
	r := newRig(self, 0, false)
	r.cl.setSnap(masterSnap(self, defaultSettings(), b))

	if err := r.e.MakeMaster(context.Background(), self); err != nil {
		t.Fatalf("MakeMaster: %v", err)
	}
	// self stays master: local Unfollow (SetFollowing Zero), b told to follow self.
	got, ok := r.cl.lastFollowing()
	if !ok || got != id.Zero {
		t.Fatalf("self SetFollowing = %v,%v want Zero", got, ok)
	}
	var followedB bool
	for _, cc := range r.fc.snapshot() {
		if cc.peer == b && cc.target == self && !cc.unfollow {
			followedB = true
		}
		if cc.peer == self {
			t.Error("no HTTP self-call expected for new master == self")
		}
	}
	if !followedB {
		t.Error("missing Follow(b -> self)")
	}
}

func TestMakeMasterToleratesMemberError(t *testing.T) {
	self, b, c := idN(1), idN(2), idN(3)
	r := newRig(self, 0, false)
	r.cl.setSnap(masterSnap(self, defaultSettings(), b, c))
	r.fc.errFn = func(peer id.ID) error {
		if peer == c {
			return errors.New("boom")
		}
		return nil
	}
	if err := r.e.MakeMaster(context.Background(), b); err != nil {
		t.Fatalf("MakeMaster should tolerate member error, got %v", err)
	}
}

func TestMakeMasterOnNonMasterRejected(t *testing.T) {
	master, self := idN(1), idN(2)
	r := newRig(self, 0, false)
	r.cl.setSnap(masterSnap(master, defaultSettings(), self)) // self is a follower
	if err := r.e.MakeMaster(context.Background(), self); !errors.Is(err, ErrNotMaster) {
		t.Fatalf("err = %v, want ErrNotMaster", err)
	}
}

func TestMakeMasterUnknownNode(t *testing.T) {
	self, b, outsider := idN(1), idN(2), idN(7)
	r := newRig(self, 0, false)
	r.cl.setSnap(masterSnap(self, defaultSettings(), b))
	if err := r.e.MakeMaster(context.Background(), outsider); !errors.Is(err, ErrTargetUnknown) {
		t.Fatalf("err = %v, want ErrTargetUnknown", err)
	}
}
