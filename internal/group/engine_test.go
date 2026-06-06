package group

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

func renderNode(id string) state.NodeRecord {
	return state.NodeRecord{ID: id, Caps: state.Capabilities{Render: true}}
}
func sinklessNode(id string) state.NodeRecord {
	return state.NodeRecord{ID: id, Caps: state.Capabilities{Render: false}}
}

func TestRecompute_Transitions(t *testing.T) {
	render := state.Capabilities{Render: true}
	sinkless := state.Capabilities{Render: false}

	tests := []struct {
		name string
		prev Decision
		in   Inputs
		want Decision // exported fields only; StreamGen checked separately where noted
	}{
		{
			name: "T1 create ⇒ solo (render)",
			prev: Decision{},
			in: Inputs{
				SelfID: "n1", MasterID: "n1", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n1")},
			},
			want: Decision{
				Role: RoleSolo, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 1,
			},
		},
		{
			name: "T1 create sink-less ⇒ solo, no local render",
			prev: Decision{},
			in: Inputs{
				SelfID: "n1", MasterID: "n1", MyCaps: sinkless,
				Members: []state.NodeRecord{sinklessNode("n1")},
			},
			want: Decision{
				Role: RoleSolo, Posture: PostureNoLocalRender, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, StreamGen: 1,
			},
		},
		{
			name: "T2 solo ⇒ master (second member, still me master) no streamGen bump",
			prev: Decision{Role: RoleSolo, IsMaster: true, RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 1},
			in: Inputs{
				SelfID: "n1", MasterID: "n1", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n1"), renderNode("n2")},
			},
			want: Decision{
				Role: RoleMaster, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 1,
			},
		},
		{
			name: "T3 ⇒ follower (render, other elected, healthy)",
			prev: Decision{Role: RoleSolo, IsMaster: true, RunOrigin: true, RunClockSrv: true, StreamGen: 1},
			in: Inputs{
				SelfID: "n2", MasterID: "n1", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n1"), renderNode("n2")},
				ClockOK: true, MinDelayOK: true,
			},
			want: Decision{
				Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 1,
			},
		},
		{
			name: "T3 sink-less non-master ⇒ control-only member",
			prev: Decision{},
			in: Inputs{
				SelfID: "n2", MasterID: "n1", MyCaps: sinkless,
				Members: []state.NodeRecord{renderNode("n1"), sinklessNode("n2")},
				ClockOK: true, MinDelayOK: true,
			},
			want: Decision{
				Role: RoleFollower, Posture: PostureControlOnlyMember, RunClockFol: true,
			},
		},
		{
			name: "T4 follower ⇒ master (promotion bumps streamGen)",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n2", MyCaps: render,
				Members:    []state.NodeRecord{renderNode("n2"), renderNode("n3")},
				Generation: 9,
			},
			want: Decision{
				Role: RoleMaster, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 6,
			},
		},
		{
			name: "T5 re-point follower on master change (no streamGen bump on follower)",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n3", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n2"), renderNode("n3")},
				ClockOK: true, MinDelayOK: true,
			},
			want: Decision{
				Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5,
			},
		},
		{
			name: "T6 follower ⇒ orphan (sync degraded)",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n1", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n1"), renderNode("n2")},
				ClockOK: true, MinDelayOK: false,
			},
			want: Decision{
				Role: RoleOrphan, RunClockFol: true, RunReceiver: true, StreamGen: 5,
			},
		},
		{
			name: "T6 orphan: no elected master",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true},
			in: Inputs{
				SelfID: "n2", MasterID: "", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n2")},
				ClockOK: true, MinDelayOK: true,
			},
			want: Decision{Role: RoleOrphan, RunClockFol: true, RunReceiver: true},
		},
		{
			name: "T7 orphan ⇒ follower (sync re-acquired)",
			prev: Decision{Role: RoleOrphan, RunClockFol: true, RunReceiver: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n1", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n1"), renderNode("n2")},
				ClockOK: true, MinDelayOK: true,
			},
			want: Decision{
				Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5,
			},
		},
		{
			name: "T8 orphan ⇒ master (promotion bumps streamGen)",
			prev: Decision{Role: RoleOrphan, RunClockFol: true, RunReceiver: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n2", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n2"), renderNode("n3")},
			},
			want: Decision{
				Role: RoleMaster, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 6,
			},
		},
		{
			name: "T9 follower removed ⇒ own solo group",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n2", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n2")}, // alone now
			},
			want: Decision{
				Role: RoleSolo, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 6,
			},
		},
		{
			name: "T10 master ⇒ solo (other members gone, no restart, no bump)",
			prev: Decision{Role: RoleMaster, IsMaster: true, RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n1", MasterID: "n1", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n1")},
			},
			want: Decision{
				Role: RoleSolo, IsMaster: true,
				RunOrigin: true, RunClockSrv: true, RunRender: true, StreamGen: 5,
			},
		},
		{
			name: "T11 render toggle true→false on follower ⇒ control-only member",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n1", MyCaps: sinkless,
				Members: []state.NodeRecord{renderNode("n1"), sinklessNode("n2")},
				ClockOK: true, MinDelayOK: true,
			},
			want: Decision{
				Role: RoleFollower, Posture: PostureControlOnlyMember, RunClockFol: true, StreamGen: 5,
			},
		},
		{
			name: "group-move transient ⇒ orphan/silence (render)",
			prev: Decision{Role: RoleFollower, RunClockFol: true, RunReceiver: true, RunRender: true, StreamGen: 5},
			in: Inputs{
				SelfID: "n2", MasterID: "n3", MyCaps: render,
				Members: []state.NodeRecord{renderNode("n3"), renderNode("n2")},
				ClockOK: true, MinDelayOK: true,
				GroupMoved: true,
			},
			want: Decision{Role: RoleOrphan, RunClockFol: true, StreamGen: 5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Recompute(tc.prev, tc.in)
			assertDecision(t, got, tc.want)
		})
	}
}

func assertDecision(t *testing.T, got, want Decision) {
	t.Helper()
	if got.Role != want.Role {
		t.Errorf("Role = %v, want %v", got.Role, want.Role)
	}
	if got.Posture != want.Posture {
		t.Errorf("Posture = %v, want %v", got.Posture, want.Posture)
	}
	if got.IsMaster != want.IsMaster {
		t.Errorf("IsMaster = %v, want %v", got.IsMaster, want.IsMaster)
	}
	if got.RunOrigin != want.RunOrigin {
		t.Errorf("RunOrigin = %v, want %v", got.RunOrigin, want.RunOrigin)
	}
	if got.RunReceiver != want.RunReceiver {
		t.Errorf("RunReceiver = %v, want %v", got.RunReceiver, want.RunReceiver)
	}
	if got.RunRender != want.RunRender {
		t.Errorf("RunRender = %v, want %v", got.RunRender, want.RunRender)
	}
	if got.RunClockSrv != want.RunClockSrv {
		t.Errorf("RunClockSrv = %v, want %v", got.RunClockSrv, want.RunClockSrv)
	}
	if got.RunClockFol != want.RunClockFol {
		t.Errorf("RunClockFol = %v, want %v", got.RunClockFol, want.RunClockFol)
	}
	if got.StreamGen != want.StreamGen {
		t.Errorf("StreamGen = %d, want %d", got.StreamGen, want.StreamGen)
	}
}

func TestRecompute_Determinism(t *testing.T) {
	in := Inputs{
		SelfID: "n2", MasterID: "n1", MyCaps: state.Capabilities{Render: true},
		Members: []state.NodeRecord{renderNode("n1"), renderNode("n2")},
		ClockOK: true, MinDelayOK: true,
	}
	a := Recompute(Decision{}, in)
	b := Recompute(Decision{}, in)
	if a != b {
		t.Errorf("Recompute not deterministic: %+v vs %+v", a, b)
	}
}

func TestRecompute_NeverTwoMasters(t *testing.T) {
	// Two nodes computing from the same agreed election (MasterID=n1) must yield
	// exactly one IsMaster.
	members := []state.NodeRecord{renderNode("n1"), renderNode("n2")}
	in1 := Inputs{SelfID: "n1", MasterID: "n1", MyCaps: state.Capabilities{Render: true}, Members: members, ClockOK: true, MinDelayOK: true}
	in2 := Inputs{SelfID: "n2", MasterID: "n1", MyCaps: state.Capabilities{Render: true}, Members: members, ClockOK: true, MinDelayOK: true}
	d1 := Recompute(Decision{}, in1)
	d2 := Recompute(Decision{}, in2)
	if d1.IsMaster == d2.IsMaster {
		t.Errorf("expected exactly one master; got n1=%v n2=%v", d1.IsMaster, d2.IsMaster)
	}
	if !d1.IsMaster {
		t.Errorf("elected n1 should be master")
	}
}

func TestRecompute_ProfileChangeBumpsStreamGen(t *testing.T) {
	base := Inputs{
		SelfID: "n1", MasterID: "n1", MyCaps: state.Capabilities{Render: true},
		Members:    []state.NodeRecord{renderNode("n1"), renderNode("n2")},
		Generation: 3,
		Profile:    Profile{Codec: "opus", FEC: "xorParity", Rate: 48000, FramesPerChunk: 480},
	}
	d0 := Recompute(Decision{}, base) // becomes master ⇒ streamGen 1
	if d0.StreamGen != 1 {
		t.Fatalf("first master streamGen = %d, want 1", d0.StreamGen)
	}
	// Re-apply identical inputs ⇒ no bump.
	d1 := Recompute(d0, base)
	if d1.StreamGen != 1 {
		t.Errorf("idempotent recompute bumped streamGen to %d, want 1", d1.StreamGen)
	}
	// Profile change ⇒ bump.
	changed := base
	changed.Profile.Codec = "pcm"
	d2 := Recompute(d1, changed)
	if d2.StreamGen != 2 {
		t.Errorf("profile change streamGen = %d, want 2", d2.StreamGen)
	}
	// Election generation change (re-election to same node) ⇒ bump.
	regen := changed
	regen.Generation = 4
	d3 := Recompute(d2, regen)
	if d3.StreamGen != 3 {
		t.Errorf("generation change streamGen = %d, want 3", d3.StreamGen)
	}
}
