package group

import (
	"testing"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

func TestMyGroupSolo(t *testing.T) {
	self := idN(1)
	mv := myGroup(soloSnap(self), self)
	if !mv.found {
		t.Fatal("want found")
	}
	if mv.role != roleSolo {
		t.Fatalf("role = %v, want solo", mv.role)
	}
	if mv.master != self {
		t.Fatalf("master = %v, want self", mv.master)
	}
	if mv.stale {
		t.Fatal("want not stale")
	}
}

func TestMyGroupMaster(t *testing.T) {
	self, f := idN(1), idN(2)
	mv := myGroup(masterSnap(self, defaultSettings(), f), self)
	if mv.role != roleMaster {
		t.Fatalf("role = %v, want master", mv.role)
	}
	if mv.master != self {
		t.Fatalf("master = %v, want self", mv.master)
	}
}

func TestMyGroupFollower(t *testing.T) {
	master, self := idN(1), idN(2)
	mv := myGroup(masterSnap(master, defaultSettings(), self), self)
	if mv.role != roleFollower {
		t.Fatalf("role = %v, want follower", mv.role)
	}
	if mv.master != master {
		t.Fatalf("master = %v, want %v", mv.master, master)
	}
}

func TestMyGroupStaleFollowing(t *testing.T) {
	self, deadTarget := idN(1), idN(9)
	// self follows a target that C derived as invalid → self is in its own solo
	// group despite Following != Zero.
	n := node(self, deadTarget, true)
	g := contracts.GroupView{
		ID:      id.XOR(self),
		Master:  self,
		Members: []id.ID{self},
	}
	snap := contracts.Snapshot{Nodes: []contracts.NodeView{n}, Groups: []contracts.GroupView{g}}
	mv := myGroup(snap, self)
	if !mv.stale {
		t.Fatal("want stale (dangling follow)")
	}
	if mv.role != roleSolo {
		t.Fatalf("role = %v, want solo", mv.role)
	}
}

func TestMyGroupNotYetDerived(t *testing.T) {
	self := idN(1)
	mv := myGroup(contracts.Snapshot{}, self)
	if mv.found {
		t.Fatal("want not found")
	}
}
