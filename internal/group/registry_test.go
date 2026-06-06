package group

import (
	"sync"
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/cluster"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// hookRec records per-group lifecycle calls so the multi-group tests can assert
// cross-group isolation (a call on group A must never appear under group B). It is
// concurrency-safe so the -race integration test can drive registries in parallel.
type hookRec struct {
	mu    sync.Mutex
	calls map[string][]string // groupID -> ordered hook names
}

func newHookRec() *hookRec { return &hookRec{calls: map[string][]string{}} }

func (h *hookRec) log(group, name string) {
	h.mu.Lock()
	h.calls[group] = append(h.calls[group], name)
	h.mu.Unlock()
}

func (h *hookRec) get(group string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls[group]...)
}

func (h *hookRec) count(group, name string) int {
	n := 0
	for _, c := range h.get(group) {
		if c == name {
			n++
		}
	}
	return n
}

// hooksFor builds a Hooks whose every closure records into the recorder under the
// owning group id — the per-group binding cmd/ensemble does for real.
func (h *hookRec) hooksFor(group string) Hooks {
	return Hooks{
		StartClockServer:   func(string) error { h.log(group, "StartClockServer"); return nil },
		StopClockServer:    func() { h.log(group, "StopClockServer") },
		StartOrigin:        func(string, uint64) error { h.log(group, "StartOrigin"); return nil },
		StopOrigin:         func() { h.log(group, "StopOrigin") },
		OriginResumeAt:     func(int64, bool) { h.log(group, "OriginResumeAt") },
		StartClockFollower: func(string, string) error { h.log(group, "StartClockFollower"); return nil },
		StopClockFollower:  func() { h.log(group, "StopClockFollower") },
		StartReceiver:      func(string) error { h.log(group, "StartReceiver"); return nil },
		StopReceiver:       func() { h.log(group, "StopReceiver") },
		StartRender:        func() error { h.log(group, "StartRender"); return nil },
		StopRender:         func() { h.log(group, "StopRender") },
	}
}

// docWith builds a ConfigDoc with the given render-capable nodes and groups.
func docWith(nodes []state.NodeRecord, groups ...state.GroupRecord) state.ConfigDoc {
	return state.ConfigDoc{Nodes: nodes, Groups: groups}
}

func TestRegistry_AddTwoGroupsIndependentInputs(t *testing.T) {
	rec := newHookRec()
	r := NewRegistry("n1", rec.hooksFor)

	nodes := []state.NodeRecord{
		renderNode("n1"), renderNode("n2"), renderNode("n3"),
	}
	doc := docWith(nodes,
		state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1", "n2"}},
		state.GroupRecord{ID: "B", MemberNodeIDs: []string{"n3"}},
	)
	// n1 masters A; n3 masters B. Self is n1, so A renders (solo/master) and B is
	// observed only (self not a member ⇒ no MyCaps render path for B).
	outcomes := map[string]cluster.Outcome{
		"A": {MasterID: "n1", Generation: 1, IsSelf: true},
		"B": {MasterID: "n3", Generation: 1, IsSelf: false},
	}
	r.OnState(doc, outcomes)

	decs := r.Decisions()
	if len(decs) != 2 {
		t.Fatalf("Decisions has %d groups, want 2", len(decs))
	}
	if _, ok := decs["A"]; !ok {
		t.Fatal("group A missing from Decisions")
	}
	if _, ok := decs["B"]; !ok {
		t.Fatal("group B missing from Decisions")
	}

	// Self (n1) is A's master ⇒ A runs origin + clock server.
	if !decs["A"].IsMaster || !decs["A"].RunOrigin {
		t.Fatalf("group A decision = %+v, want self-master origin", decs["A"])
	}
	// Self is not in B and not B's master ⇒ B's engine resolves a non-master role.
	if decs["B"].IsMaster {
		t.Fatalf("group B decision = %+v, want self not master", decs["B"])
	}

	// Each engine saw ITS OWN members: A's runtime has n1,n2; B's has n3.
	r.mu.Lock()
	aMembers := r.groups["A"].last.Members
	bMembers := r.groups["B"].last.Members
	r.mu.Unlock()
	if len(aMembers) != 2 {
		t.Fatalf("group A members=%d, want 2", len(aMembers))
	}
	if len(bMembers) != 1 || bMembers[0].ID != "n3" {
		t.Fatalf("group B members=%v, want [n3]", bMembers)
	}

	// A started its master subsystems; B started its own — no cross-talk.
	if rec.count("A", "StartClockServer") != 1 {
		t.Fatalf("A StartClockServer calls=%d, want 1", rec.count("A", "StartClockServer"))
	}
	if rec.count("B", "StartClockServer") != 0 && rec.count("B", "StartOrigin") != 0 {
		// B is mastered by n3, not self ⇒ self runs no origin/clock-server for B.
		t.Fatalf("B started master parts on a non-master self: %v", rec.get("B"))
	}
}

func TestRegistry_DropGroupTearsDownOnlyThatGroup(t *testing.T) {
	rec := newHookRec()
	r := NewRegistry("n1", rec.hooksFor)

	nodes := []state.NodeRecord{renderNode("n1"), renderNode("n3")}
	doc := docWith(nodes,
		state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1"}},
		state.GroupRecord{ID: "B", MemberNodeIDs: []string{"n1", "n3"}},
	)
	outcomes := map[string]cluster.Outcome{
		"A": {MasterID: "n1", Generation: 1, IsSelf: true},
		"B": {MasterID: "n1", Generation: 1, IsSelf: true},
	}
	r.OnState(doc, outcomes)

	// Now remove group A from the doc; B stays.
	doc2 := docWith(nodes,
		state.GroupRecord{ID: "B", MemberNodeIDs: []string{"n1", "n3"}},
	)
	out2 := map[string]cluster.Outcome{} // no election change this round
	r.OnState(doc2, out2)

	decs := r.Decisions()
	if _, ok := decs["A"]; ok {
		t.Fatal("group A still present after drop")
	}
	if _, ok := decs["B"]; !ok {
		t.Fatal("group B dropped unexpectedly")
	}

	// A's master subsystems must have been stopped exactly once on drop.
	if got := rec.count("A", "StopClockServer"); got != 1 {
		t.Fatalf("A StopClockServer=%d, want 1", got)
	}
	if got := rec.count("A", "StopOrigin"); got != 1 {
		t.Fatalf("A StopOrigin=%d, want 1", got)
	}
	// B untouched by the drop: no stop calls on B.
	if got := rec.count("B", "StopClockServer"); got != 0 {
		t.Fatalf("B StopClockServer=%d, want 0 (untouched)", got)
	}
	if got := rec.count("B", "StopOrigin"); got != 0 {
		t.Fatalf("B StopOrigin=%d, want 0 (untouched)", got)
	}
}

func TestRegistry_ElectionChangeInAOnlyIsolatesB(t *testing.T) {
	rec := newHookRec()
	// Self is n2: a render-capable follower in both groups.
	r := NewRegistry("n2", rec.hooksFor)

	nodes := []state.NodeRecord{
		renderNode("n1"), renderNode("n2"), renderNode("n3"),
	}
	doc := docWith(nodes,
		state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1", "n2"}},
		state.GroupRecord{ID: "B", MemberNodeIDs: []string{"n2", "n3"}},
	)
	// Initial: A mastered by n1, B mastered by n3; self n2 is a follower in both.
	r.OnState(doc, map[string]cluster.Outcome{
		"A": {MasterID: "n1", Generation: 1},
		"B": {MasterID: "n3", Generation: 1},
	})
	// Feed clock health so the follower engines resolve to RoleFollower not orphan.
	r.mu.Lock()
	r.groups["A"].engine.SetClockHealth(0, true)
	r.groups["B"].engine.SetClockHealth(0, true)
	r.mu.Unlock()
	// Re-apply with ClockOK so both become followers (carry election forward).
	r.applyClockOK()

	bBefore := r.Decisions()["B"]
	bCallsBefore := rec.get("B")

	// Election change in A ONLY: master flips n1 -> n2 (self), new generation.
	r.OnState(doc, map[string]cluster.Outcome{
		"A": {MasterID: "n2", Generation: 2, IsSelf: true},
		// B absent ⇒ its master/generation carried forward unchanged.
	})

	aAfter := r.Decisions()["A"]
	bAfter := r.Decisions()["B"]

	// A's engine re-applied: self is now A's master ⇒ origin runs + streamGen bumped.
	if !aAfter.IsMaster || !aAfter.RunOrigin {
		t.Fatalf("group A after flip = %+v, want self-master origin", aAfter)
	}
	// B's decision is identical (no-op): same role, same streamGen.
	if bAfter != bBefore {
		t.Fatalf("group B decision changed: before=%+v after=%+v", bBefore, bAfter)
	}
	// B saw NO new lifecycle calls during A's election change.
	if got := rec.get("B"); len(got) != len(bCallsBefore) {
		t.Fatalf("group B got new hook calls during A election change: %v -> %v", bCallsBefore, got)
	}
}

// applyClockOK re-applies each runtime's last Inputs with ClockOK=true so a fresh
// follower engine (which starts orphaned with no clock health) advances to
// RoleFollower. Test-only convenience that mimics cmd refreshing clock health then
// re-applying.
func (r *Registry) applyClockOK() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rt := range r.groups {
		in := rt.last
		in.ClockOK = true
		rt.dec = rt.engine.Apply(in)
		rt.last = in
	}
}

func TestRegistry_IdempotentOnState(t *testing.T) {
	rec := newHookRec()
	r := NewRegistry("n1", rec.hooksFor)

	nodes := []state.NodeRecord{renderNode("n1"), renderNode("n2")}
	doc := docWith(nodes,
		state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1", "n2"}},
	)
	outcomes := map[string]cluster.Outcome{"A": {MasterID: "n1", Generation: 1, IsSelf: true}}

	r.OnState(doc, outcomes)
	callsAfterFirst := len(rec.get("A"))

	// Identical inputs three more times: no redundant start/stop.
	r.OnState(doc, outcomes)
	r.OnState(doc, map[string]cluster.Outcome{}) // election unchanged ⇒ carry forward
	r.OnState(doc, outcomes)

	if got := len(rec.get("A")); got != callsAfterFirst {
		t.Fatalf("idempotent OnState produced extra hook calls: %d -> %d (%v)",
			callsAfterFirst, got, rec.get("A"))
	}
}
