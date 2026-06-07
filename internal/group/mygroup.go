package group

import (
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// role classifies this node within its derived group.
type role int

const (
	roleSolo     role = iota // master of a group of 1 (following == Zero, alone)
	roleMaster               // master of a multi-member group
	roleFollower             // a valid follower of someone else
)

// String renders a role for structured logging.
func (r role) String() string {
	switch r {
	case roleSolo:
		return "solo"
	case roleMaster:
		return "master"
	case roleFollower:
		return "follower"
	default:
		return "unknown"
	}
}

// myView is this node's resolved position in the pre-derived snapshot (D5).
// C already filled Snapshot.Groups; H only LOCATES this node's group + reads
// its own NodeView. No XOR, no membership computation here.
type myView struct {
	group  contracts.GroupView // the group containing self
	self   contracts.NodeView  // this node's own record
	role   role
	master id.ID // group.Master
	stale  bool  // self.Following points at an invalid target (§5 self-heal trigger)
	found  bool  // self located in some derived group + has a NodeView
}

// myGroup finds this node's group in snap.Groups (the unique group whose
// Members contain self), reads self's NodeView, and classifies role +
// staleness.
//
// stale == true iff self.Following != Zero AND C derived self into its OWN solo
// group (master == self, members == {self}) despite the non-empty Following —
// i.e. the follow target was dead/unknown/itself-following (§5). That is exactly
// the self-heal trigger.
//
// found == false when self is not yet present in any derived group (a transient
// snapshot before C derived self, or before self's own record exists): callers
// skip reconcile for that tick.
func myGroup(snap contracts.Snapshot, self id.ID) myView {
	var mv myView

	// Locate self's NodeView.
	var selfNode contracts.NodeView
	haveNode := false
	for _, n := range snap.Nodes {
		if n.ID == self {
			selfNode = n
			haveNode = true
			break
		}
	}

	// Locate the derived group containing self.
	var g contracts.GroupView
	haveGroup := false
	for _, grp := range snap.Groups {
		for _, m := range grp.Members {
			if m == self {
				g = grp
				haveGroup = true
				break
			}
		}
		if haveGroup {
			break
		}
	}

	if !haveGroup || !haveNode {
		return mv // found == false
	}

	mv.group = g
	mv.self = selfNode
	mv.master = g.Master
	mv.found = true

	switch {
	case g.Master != self:
		mv.role = roleFollower
	case len(g.Members) == 1:
		mv.role = roleSolo
		// Stale: self is solo (its own single-member group) yet Following != Zero
		// → C demoted a dangling follow to solo (§5).
		mv.stale = !selfNode.Following.IsZero()
	default:
		mv.role = roleMaster
	}
	return mv
}
