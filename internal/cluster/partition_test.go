package cluster

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// partitionDoc builds a one-group ConfigDoc whose group members are the given ids.
func partitionDoc(groupID string, memberIDs ...string) state.ConfigDoc {
	return state.ConfigDoc{
		Groups: []state.GroupRecord{{ID: groupID, MemberNodeIDs: memberIDs}},
	}
}

// aliveIn builds the alive Member set for a group side (each advertises groupID).
func aliveIn(groupID string, ids ...string) []Member {
	out := make([]Member, 0, len(ids))
	for _, id := range ids {
		out = append(out, aliveMember(id, groupID))
	}
	return out
}

// TestPartitionSplitElectsPerSide: during a partition each side independently
// elects min(alive∩group) on its own visible member subset (02 §6.2 — two masters
// for one group is accepted; no quorum, LWW not Raft).
func TestPartitionSplitElectsPerSide(t *testing.T) {
	const gid = "g1"
	doc := partitionDoc(gid, "n-a", "n-b", "n-c", "n-d")

	// Side 1 sees {a, b}; side 2 sees {c, d}. Each side runs its own election
	// instance (a separate node's view).
	side1 := NewGroupElections("n-a")
	side2 := NewGroupElections("n-c")

	c1 := side1.Recompute(doc, aliveIn(gid, "n-a", "n-b"))
	c2 := side2.Recompute(doc, aliveIn(gid, "n-c", "n-d"))

	if got := c1[gid].MasterID; got != "n-a" {
		t.Errorf("side1 master=%q, want n-a (min of {a,b})", got)
	}
	if got := c2[gid].MasterID; got != "n-c" {
		t.Errorf("side2 master=%q, want n-c (min of {c,d})", got)
	}
	// Two distinct masters during the split — the accepted split-brain (02 §6.2).
	if c1[gid].MasterID == c2[gid].MasterID {
		t.Error("split did not yield two distinct per-side masters")
	}
}

// TestPartitionHealConverges: on heal the losing-side election sees the UNION of
// alive members, recomputes min, and the loser demotes; Generation bumps on the
// side whose master changed (02 §6.3 — structural, automatic convergence).
func TestPartitionHealConverges(t *testing.T) {
	const gid = "g1"
	doc := partitionDoc(gid, "n-a", "n-b", "n-c", "n-d")

	// Side 2 was isolated and elected n-c during the split.
	side2 := NewGroupElections("n-c")
	side2.Recompute(doc, aliveIn(gid, "n-c", "n-d"))
	if m := side2.Master(gid); m != "n-c" {
		t.Fatalf("pre-heal side2 master=%q, want n-c", m)
	}
	genBefore := side2.Generation(gid)

	// Heal: side2 now sees the full union {a,b,c,d}. Global min is n-a.
	changed := side2.Recompute(doc, aliveIn(gid, "n-a", "n-b", "n-c", "n-d"))

	if m := side2.Master(gid); m != "n-a" {
		t.Errorf("post-heal side2 master=%q, want n-a (global min)", m)
	}
	out, ok := changed[gid]
	if !ok {
		t.Fatal("heal did not report a master change on the losing side")
	}
	if out.MasterID != "n-a" {
		t.Errorf("heal outcome master=%q, want n-a", out.MasterID)
	}
	if out.Generation <= genBefore {
		t.Errorf("heal Generation=%d, want > %d (bump on change)", out.Generation, genBefore)
	}
	// The winning side (n-a's view) already had n-a as master, so on heal it sees
	// no change — both sides now agree on n-a.
	side1 := NewGroupElections("n-a")
	side1.Recompute(doc, aliveIn(gid, "n-a", "n-b"))
	side1.Recompute(doc, aliveIn(gid, "n-a", "n-b", "n-c", "n-d"))
	if side1.Master(gid) != side2.Master(gid) {
		t.Errorf("post-heal masters disagree: side1=%q side2=%q", side1.Master(gid), side2.Master(gid))
	}
}

// TestPartitionDeterministic: every node computing over the SAME (doc, alive) pair
// elects the same master — election is a pure stateless function (02 §5.1).
func TestPartitionDeterministic(t *testing.T) {
	const gid = "g1"
	doc := partitionDoc(gid, "n-a", "n-b", "n-c")
	alive := aliveIn(gid, "n-b", "n-c", "n-a") // order shuffled

	masters := make(map[string]string)
	for _, self := range []string{"n-a", "n-b", "n-c"} {
		ge := NewGroupElections(self)
		ge.Recompute(doc, alive)
		masters[self] = ge.Master(gid)
	}
	for self, m := range masters {
		if m != "n-a" {
			t.Errorf("node %q elected master=%q, want n-a (deterministic min)", self, m)
		}
	}
}

// TestPartitionHonorsHint: a MasterHint that is an alive candidate wins on both
// sides; if the hint is isolated on one side, that side falls back to its local
// min (A.5) — convergence to the hint resumes on heal.
func TestPartitionHonorsHint(t *testing.T) {
	const gid = "g1"
	doc := partitionDoc(gid, "n-a", "n-b", "n-c")
	doc.Groups[0].MasterHint = "n-c" // operator pins n-c

	// Side without n-c falls back to its local min (n-a).
	side1 := NewGroupElections("n-a")
	side1.Recompute(doc, aliveIn(gid, "n-a", "n-b"))
	if m := side1.Master(gid); m != "n-a" {
		t.Errorf("hint-isolated side master=%q, want n-a (fallback min)", m)
	}

	// On heal, the hint candidate is alive again → it wins.
	side1.Recompute(doc, aliveIn(gid, "n-a", "n-b", "n-c"))
	if m := side1.Master(gid); m != "n-c" {
		t.Errorf("post-heal master=%q, want n-c (hint honored, A.5)", m)
	}
}
