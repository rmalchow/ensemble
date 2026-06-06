package cluster

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// members builds a candidate slice from node ids (Meta only; election reads
// just Meta.NodeID).
func members(ids ...string) []Member {
	out := make([]Member, 0, len(ids))
	for _, id := range ids {
		out = append(out, Member{Meta: Meta{NodeID: id}})
	}
	return out
}

func TestElectionTable(t *testing.T) {
	// Each step applies Update and asserts the elected master, the changed flag,
	// and the generation delta from the previous step.
	type step struct {
		members  []Member
		hint     string
		master   string
		changed  bool
		genDelta uint64
	}
	tests := []struct {
		name   string
		selfID string
		steps  []step
	}{
		{
			name:   "empty set => no master",
			selfID: "n1",
			steps: []step{
				{members: members(), hint: "", master: "", changed: false, genDelta: 0},
			},
		},
		{
			name:   "single member",
			selfID: "n3",
			steps: []step{
				{members: members("n3"), hint: "", master: "n3", changed: true, genDelta: 1},
			},
		},
		{
			name:   "lowest wins",
			selfID: "n2",
			steps: []step{
				{members: members("n3", "n1", "n2"), hint: "", master: "n1", changed: true, genDelta: 1},
			},
		},
		{
			name:   "hint alive and member",
			selfID: "n1",
			steps: []step{
				{members: members("n1", "n2", "n3"), hint: "n2", master: "n2", changed: true, genDelta: 1},
			},
		},
		{
			name:   "hint dead => lowest",
			selfID: "n1",
			steps: []step{
				{members: members("n1", "n3"), hint: "n2", master: "n1", changed: true, genDelta: 1},
			},
		},
		{
			name:   "stable no-flap",
			selfID: "n1",
			steps: []step{
				{members: members("n1", "n2"), hint: "", master: "n1", changed: true, genDelta: 1},
				{members: members("n1", "n2"), hint: "", master: "n1", changed: false, genDelta: 0},
			},
		},
		{
			name:   "lower joins => change",
			selfID: "n2",
			steps: []step{
				{members: members("n2", "n3"), hint: "", master: "n2", changed: true, genDelta: 1},
				{members: members("n1", "n2", "n3"), hint: "", master: "n1", changed: true, genDelta: 1},
			},
		},
		{
			name:   "master dies => re-elect",
			selfID: "n2",
			steps: []step{
				{members: members("n1", "n2", "n3"), hint: "", master: "n1", changed: true, genDelta: 1},
				{members: members("n2", "n3"), hint: "", master: "n2", changed: true, genDelta: 1},
			},
		},
		{
			name:   "empty id skipped",
			selfID: "n2",
			steps: []step{
				{members: members("", "n2"), hint: "", master: "n2", changed: true, genDelta: 1},
			},
		},
		{
			name:   "hint becomes dead => fail back to lowest",
			selfID: "n1",
			steps: []step{
				{members: members("n1", "n2", "n3"), hint: "n3", master: "n3", changed: true, genDelta: 1},
				{members: members("n1", "n2"), hint: "n3", master: "n1", changed: true, genDelta: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewElection(tt.selfID)
			for i, s := range tt.steps {
				prevGen := e.Generation()
				master, changed := e.Update(s.members, s.hint)
				if master != s.master {
					t.Errorf("step %d: master = %q, want %q", i, master, s.master)
				}
				if changed != s.changed {
					t.Errorf("step %d: changed = %v, want %v", i, changed, s.changed)
				}
				if gotDelta := e.Generation() - prevGen; gotDelta != s.genDelta {
					t.Errorf("step %d: gen delta = %d, want %d", i, gotDelta, s.genDelta)
				}
				if got, want := e.IsMaster(), master != "" && master == tt.selfID; got != want {
					t.Errorf("step %d: IsMaster = %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestElectionGenerationMonotonic(t *testing.T) {
	e := NewElection("n2")
	last := e.Generation()
	seq := [][]Member{
		members("n2"),
		members("n2"),       // no change
		members("n1", "n2"), // change
		members("n1", "n2"), // no change
		members("n2"),       // change
	}
	for i, ms := range seq {
		e.Update(ms, "")
		g := e.Generation()
		if g < last {
			t.Fatalf("step %d: generation went backwards %d -> %d", i, last, g)
		}
		last = g
	}
}

func aliveMember(id, groupID string) Member {
	return Member{Meta: Meta{NodeID: id, GroupID: groupID}}
}

func docWithGroups(groups ...state.GroupRecord) state.ConfigDoc {
	return state.ConfigDoc{Groups: groups}
}

func TestGroupElectionsTwoGroups(t *testing.T) {
	g := NewGroupElections("n3")
	doc := docWithGroups(
		state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1", "n2"}},
		state.GroupRecord{ID: "B", MemberNodeIDs: []string{"n3", "n4"}},
	)
	alive := []Member{
		aliveMember("n1", "A"),
		aliveMember("n2", "A"),
		aliveMember("n3", "B"),
		aliveMember("n4", "B"),
	}
	changed := g.Recompute(doc, alive)
	if got := changed["A"].MasterID; got != "n1" {
		t.Errorf("group A master = %q, want n1", got)
	}
	if got := changed["B"].MasterID; got != "n3" {
		t.Errorf("group B master = %q, want n3", got)
	}
	if changed["A"].IsSelf {
		t.Errorf("group A IsSelf should be false (self=n3)")
	}
	if !changed["B"].IsSelf {
		t.Errorf("group B IsSelf should be true (self=n3, master=n3)")
	}
	if g.Master("A") != "n1" || g.Master("B") != "n3" {
		t.Errorf("Master() getters disagree: A=%q B=%q", g.Master("A"), g.Master("B"))
	}
}

func TestGroupElectionsIntersectionRule(t *testing.T) {
	// n2 is listed in group A's members but is mid-move and advertising
	// Meta.GroupID="B" => excluded from A's candidate set (02 §5.2 step 1).
	g := NewGroupElections("n1")
	doc := docWithGroups(state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1", "n2"}})
	alive := []Member{
		aliveMember("n1", "A"),
		aliveMember("n2", "B"), // stale gid
	}
	changed := g.Recompute(doc, alive)
	if got := changed["A"].MasterID; got != "n1" {
		t.Errorf("group A master = %q, want n1 (n2 excluded by stale gid)", got)
	}
}

func TestGroupElectionsHintPerGroup(t *testing.T) {
	g := NewGroupElections("n1")
	// Hint n2 honored while alive & member.
	doc := docWithGroups(state.GroupRecord{
		ID: "A", MemberNodeIDs: []string{"n1", "n2", "n3"}, MasterHint: "n2",
	})
	alive := []Member{aliveMember("n1", "A"), aliveMember("n2", "A"), aliveMember("n3", "A")}
	changed := g.Recompute(doc, alive)
	if got := changed["A"].MasterID; got != "n2" {
		t.Errorf("master = %q, want n2 (hint honored)", got)
	}

	// n2 dies => fail back to lowest (n1); hint ignored when not alive.
	alive = []Member{aliveMember("n1", "A"), aliveMember("n3", "A")}
	changed = g.Recompute(doc, alive)
	if got := changed["A"].MasterID; got != "n1" {
		t.Errorf("master = %q, want n1 (hint dead => lowest)", got)
	}
}

func TestGroupElectionsNoOpStability(t *testing.T) {
	g := NewGroupElections("n1")
	doc := docWithGroups(state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1", "n2"}})
	alive := []Member{aliveMember("n1", "A"), aliveMember("n2", "A")}

	first := g.Recompute(doc, alive)
	if len(first) != 1 {
		t.Fatalf("first recompute should report 1 changed group, got %d", len(first))
	}
	second := g.Recompute(doc, alive)
	if len(second) != 0 {
		t.Errorf("idempotent recompute should report 0 changed groups, got %d", len(second))
	}
}

func TestGroupElectionsZeroCandidates(t *testing.T) {
	g := NewGroupElections("n1")
	doc := docWithGroups(state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n9"}})
	alive := []Member{aliveMember("n1", "A")} // n9 not alive
	changed := g.Recompute(doc, alive)
	if got := changed["A"].MasterID; got != "" {
		t.Errorf("zero-candidate group master = %q, want \"\" (NO-MASTER)", got)
	}
}

func TestGroupElectionsSolo(t *testing.T) {
	g := NewGroupElections("n1")
	doc := docWithGroups(state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1"}})
	changed := g.Recompute(doc, []Member{aliveMember("n1", "A")})
	if got := changed["A"].MasterID; got != "n1" || !changed["A"].IsSelf {
		t.Errorf("solo group: master=%q isSelf=%v, want n1/true", got, changed["A"].IsSelf)
	}
}

func TestGroupElectionsDropResetsGeneration(t *testing.T) {
	g := NewGroupElections("n1")
	doc := docWithGroups(state.GroupRecord{ID: "A", MemberNodeIDs: []string{"n1"}})
	alive := []Member{aliveMember("n1", "A")}
	g.Recompute(doc, alive)
	if g.Generation("A") != 1 {
		t.Fatalf("generation = %d, want 1", g.Generation("A"))
	}
	g.Drop("A")
	if g.Generation("A") != 0 || g.Master("A") != "" {
		t.Errorf("after Drop: gen=%d master=%q, want 0/\"\"", g.Generation("A"), g.Master("A"))
	}
	// Re-add starts fresh: generation back to 1, not 2.
	g.Recompute(doc, alive)
	if g.Generation("A") != 1 {
		t.Errorf("re-added group generation = %d, want 1 (fresh)", g.Generation("A"))
	}
}
