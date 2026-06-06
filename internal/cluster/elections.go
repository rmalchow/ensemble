package cluster

import (
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// Outcome is the result of a group's election after a Recompute, surfaced for
// the groups whose master changed.
type Outcome struct {
	MasterID   string
	Generation uint64
	IsSelf     bool
}

// GroupElections runs one Election per group (02 §5.2). Unlike mpvsync's single
// cluster-wide election, Ensemble elects a master per playback group: a node may
// master group A while another masters group B. Each group's Election is kept so
// Generation stays monotonic across recomputes; Drop forgets a deleted group's
// state so a re-add starts a fresh generation.
type GroupElections struct {
	selfID string

	mu sync.Mutex
	e  map[string]*Election
}

// NewGroupElections creates a per-group election set for the given self id.
func NewGroupElections(selfID string) *GroupElections {
	return &GroupElections{selfID: selfID, e: make(map[string]*Election)}
}

// Recompute runs each group's election over the alive members assigned to that
// group and returns only the groups whose master CHANGED (02 §5.2 steps 1–3,
// A.5). For each GroupRecord the candidate set is the intersection:
//
//	{ alive members whose id ∈ GroupRecord.MemberNodeIDs
//	  AND whose gossiped Meta.GroupID == GroupRecord.ID }
//
// The Meta.GroupID match excludes a node mid-move that still advertises a stale
// group id (02 §5.2 step 1 / §7). The group's soft MasterHint is honored only
// while it is an alive candidate, else the lowest stable id wins. A group with
// zero candidates elects "" (NO-MASTER / starting). The result is a pure,
// stateless function of (doc, alive), so all nodes converge on the same master
// per group once they share that pair (02 §5.1 / §6).
//
// Recompute is idempotent: an unchanged (doc, alive) returns an empty map (the
// 2s safety re-eval and every Changed() can call it without spurious churn).
func (g *GroupElections) Recompute(doc state.ConfigDoc, alive []Member) (changed map[string]Outcome) {
	// Index alive members by id with their advertised group, for O(1) lookup.
	type aliveInfo struct {
		member  Member
		groupID string
	}
	byID := make(map[string]aliveInfo, len(alive))
	for _, m := range alive {
		if m.Meta.NodeID == "" {
			continue
		}
		byID[m.Meta.NodeID] = aliveInfo{member: m, groupID: m.Meta.GroupID}
	}

	changed = make(map[string]Outcome)

	g.mu.Lock()
	defer g.mu.Unlock()
	for _, gr := range doc.Groups {
		candidates := make([]Member, 0, len(gr.MemberNodeIDs))
		for _, id := range gr.MemberNodeIDs {
			info, ok := byID[id]
			if !ok || info.groupID != gr.ID {
				continue
			}
			candidates = append(candidates, info.member)
		}

		el := g.e[gr.ID]
		if el == nil {
			el = NewElection(g.selfID)
			g.e[gr.ID] = el
		}
		master, didChange := el.Update(candidates, gr.MasterHint)
		if didChange {
			changed[gr.ID] = Outcome{
				MasterID:   master,
				Generation: el.Generation(),
				IsSelf:     master != "" && master == g.selfID,
			}
		}
	}
	return changed
}

// Master returns the current elected master id for a group ("" if unknown).
func (g *GroupElections) Master(groupID string) string {
	g.mu.Lock()
	el := g.e[groupID]
	g.mu.Unlock()
	if el == nil {
		return ""
	}
	return el.Master()
}

// Generation returns the current election generation for a group (0 if unknown).
func (g *GroupElections) Generation(groupID string) uint64 {
	g.mu.Lock()
	el := g.e[groupID]
	g.mu.Unlock()
	if el == nil {
		return 0
	}
	return el.Generation()
}

// Drop forgets a deleted group's Election so a later re-add starts fresh.
func (g *GroupElections) Drop(groupID string) {
	g.mu.Lock()
	delete(g.e, groupID)
	g.mu.Unlock()
}
