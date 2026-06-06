package group

// registry.go is the multi-group concurrency surface (doc 04 §4.1.2, §4.6, A.5):
// it runs ONE group.Engine per GroupRecord the node knows about, fanning a single
// replicated-config + per-group election update out to each INDEPENDENT engine. N
// groups ⇒ N Engines, N Timelines, N clock roles, N origins — fully isolated: a
// master change / streamGen bump / profile renegotiation / failover in group A
// never touches group B's engine, timeline, clock server, or origin (mirrors 04
// §4.1.2 "one clock server per group" and 04 §4.6's per-operation "re-run for both
// groups", which here means "re-run isolates to the affected group").
//
// Per README §6.2 a node is a member of exactly one group and renders only that
// one (06 §1.5: a Render=false / non-self group never starts the local render
// loop). The Registry nonetheless models every observed group uniformly so a
// master can originate for its group while the wiring stays uniform, and so
// multi-group concurrency is exercisable in a single process (P6.1 §5 R2).
//
// Import discipline (doc 01 §2.2 / P6.1 §6): registry imports only internal/state
// and internal/cluster (the election-result type, consumed as a plain input). The
// per-group clock/origin/receiver/render lifecycle is reached ONLY through the
// Hooks function-value seam supplied by the RuntimeFactory (wired in cmd/ensemble),
// keeping the dependency graph acyclic and the registry unit-testable with fakes.

import (
	"sort"
	"sync"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// RuntimeFactory builds the per-group Hooks for a given group id, wired by
// cmd/ensemble to that group's clock.Server/Follower, stream origin/receiver, and
// (only for self's own group, when Render=true) the audio render loop. Keeping it
// a function value is what keeps internal/group free of clock/stream/audio imports
// (the Hooks seam, doc 01 §2 / A.14.3 discipline).
type RuntimeFactory func(groupID string) Hooks

// groupRuntime bundles one group's independent engine + the last Inputs applied to
// it + the last resolved Decision. There is exactly one per known GroupRecord.
type groupRuntime struct {
	engine *Engine
	last   Inputs
	dec    Decision
}

// Registry runs one Engine per group the node observes (doc 04 §4.1.2). It is the
// multi-group concurrency surface cmd drives: N groups, N Engines, N Timelines, N
// clock roles, N origins — fully independent.
type Registry struct {
	selfID string
	mk     RuntimeFactory

	mu     sync.Mutex
	groups map[string]*groupRuntime
}

// NewRegistry creates an empty registry for selfID. mk builds the per-group Hooks
// on demand as groups appear (it may be nil in tests that only inspect Decisions,
// in which case engines run with empty no-op hooks).
func NewRegistry(selfID string, mk RuntimeFactory) *Registry {
	return &Registry{
		selfID: selfID,
		mk:     mk,
		groups: make(map[string]*groupRuntime),
	}
}

// OnState is called on every gossip/election event. doc is the replicated
// ConfigDoc; outcomes is the per-group election result (cluster.GroupElections.
// Recompute output, keyed by groupID — only the groups whose master CHANGED appear,
// so an absent group carries its previously-applied election forward). OnState:
//
//	(a) creates a runtime for any new GroupRecord,
//	(b) drops runtimes for groups no longer in doc (mirrors GroupElections.Drop),
//	(c) derives a SEPARATE Inputs for each group from (doc.Groups[g], outcomes[g])
//	    and calls that group's Engine.Apply — each group independently.
//
// Because the per-group Inputs is derived only from that group's record + outcome,
// a change confined to group A produces a no-op Apply on group B (per-group
// election independence, P6.1 §7).
func (r *Registry) OnState(doc state.ConfigDoc, outcomes map[string]cluster.Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()

	present := make(map[string]struct{}, len(doc.Groups))
	for i := range doc.Groups {
		gr := doc.Groups[i]
		present[gr.ID] = struct{}{}

		rt := r.groups[gr.ID]
		if rt == nil {
			// New group: build its hooks and an independent engine (b).
			var hooks Hooks
			if r.mk != nil {
				hooks = r.mk(gr.ID)
			}
			rt = &groupRuntime{engine: NewEngine(r.selfID, hooks)}
			r.groups[gr.ID] = rt
		}

		// (c) Derive this group's Inputs and apply it independently. An election
		// outcome is only present in `outcomes` when it CHANGED; otherwise carry
		// the last-applied master/generation forward so an unrelated group's
		// engine sees identical Inputs and reconciles to a no-op.
		in := r.inputsFor(doc, gr, outcomes, rt)
		rt.dec = rt.engine.Apply(in)
		rt.last = in
	}

	// (a)/Drop: tear down + forget any group no longer in the doc.
	for id, rt := range r.groups {
		if _, ok := present[id]; ok {
			continue
		}
		rt.engine.Shutdown()
		delete(r.groups, id)
	}
}

// inputsFor derives the pure-function Inputs for one group from the replicated doc,
// the group record, and that group's election outcome. When the group's outcome is
// absent (its master did not change this round), the previously-applied
// master/generation are carried forward so the engine sees stable Inputs.
func (r *Registry) inputsFor(
	doc state.ConfigDoc,
	gr state.GroupRecord,
	outcomes map[string]cluster.Outcome,
	rt *groupRuntime,
) Inputs {
	masterID := rt.last.MasterID
	generation := rt.last.Generation
	if oc, ok := outcomes[gr.ID]; ok {
		masterID = oc.MasterID
		generation = oc.Generation
	}

	members := membersOf(doc, gr)
	self := nodeRecord(doc, r.selfID)

	in := Inputs{
		SelfID:      r.selfID,
		GroupID:     gr.ID,
		Members:     members,
		MasterID:    masterID,
		Generation:  generation,
		Profile:     profileFrom(gr.Profile),
		Playing:     gr.Playing,
		SampleIndex: rt.last.SampleIndex, // carried; the timeline owns the live value
		MyCaps:      self.Caps,
		ClockOK:     rt.last.ClockOK, // cmd refreshes via Engine.SetClockHealth
	}
	return in
}

// Decisions returns the last applied Decision per group (read-only snapshot, for
// UI/status surfaced via cmd; web never imports group — A.14.3).
func (r *Registry) Decisions() map[string]Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Decision, len(r.groups))
	for id, rt := range r.groups {
		out[id] = rt.dec
	}
	return out
}

// membersOf collects the NodeRecords of a group's members from the doc, in the
// group's MemberNodeIDs order (deterministic so Inputs is stable across calls). An
// id with no matching node record is skipped (it has not joined the doc yet).
func membersOf(doc state.ConfigDoc, gr state.GroupRecord) []state.NodeRecord {
	if len(gr.MemberNodeIDs) == 0 {
		return nil
	}
	byID := make(map[string]state.NodeRecord, len(doc.Nodes))
	for i := range doc.Nodes {
		byID[doc.Nodes[i].ID] = doc.Nodes[i]
	}
	members := make([]state.NodeRecord, 0, len(gr.MemberNodeIDs))
	ids := append([]string(nil), gr.MemberNodeIDs...)
	sort.Strings(ids)
	for _, id := range ids {
		if n, ok := byID[id]; ok {
			members = append(members, n)
		}
	}
	return members
}

// nodeRecord finds id's record in the doc, or a zero record (safe default caps).
func nodeRecord(doc state.ConfigDoc, id string) state.NodeRecord {
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == id {
			return doc.Nodes[i]
		}
	}
	return state.NodeRecord{}
}

// profileFrom adapts the replicated state.TransportProfile to the engine's Profile
// (the four playback-relevant fields; the wire-only fields are not engine inputs).
func profileFrom(tp state.TransportProfile) Profile {
	return Profile{
		Codec:          tp.Codec,
		FEC:            tp.FEC,
		Rate:           tp.Rate,
		FramesPerChunk: tp.FramesPerChunk,
	}
}
