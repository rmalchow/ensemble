package group

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// recHooks records every subsystem lifecycle call in order, for asserting
// exactly-once start/stop, idempotency, and the generation fence.
type recHooks struct {
	calls []string
}

func (r *recHooks) log(s string) { r.calls = append(r.calls, s) }

func (r *recHooks) hooks() Hooks {
	return Hooks{
		StartClockServer:     func(g string) error { r.log("clksrv.start"); return nil },
		StopClockServer:      func() { r.log("clksrv.stop") },
		StartOrigin:          func(g string, gen uint64) error { r.log("origin.start"); return nil },
		StopOrigin:           func() { r.log("origin.stop") },
		OriginResumeAt:       func(s int64, p bool) { r.log("origin.resume") },
		StartClockFollower:   func(g, a string) error { r.log("clkfol.start"); return nil },
		StopClockFollower:    func() { r.log("clkfol.stop") },
		StartReceiver:        func(g string) error { r.log("recv.start"); return nil },
		StopReceiver:         func() { r.log("recv.stop") },
		ReceiverFlushReprime: func() { r.log("recv.reprime") },
		StartRender:          func() error { r.log("render.start"); return nil },
		StopRender:           func() { r.log("render.stop") },
	}
}

func (r *recHooks) reset() { r.calls = nil }

func (r *recHooks) has(s string) bool {
	for _, c := range r.calls {
		if c == s {
			return true
		}
	}
	return false
}

func (r *recHooks) count(s string) int {
	n := 0
	for _, c := range r.calls {
		if c == s {
			n++
		}
	}
	return n
}

func soloIn(self string) Inputs {
	return Inputs{
		SelfID: self, GroupID: "g", MasterID: self,
		MyCaps:  state.Capabilities{Render: true},
		Members: []state.NodeRecord{renderNode(self)},
		ClockOK: true,
	}
}

func followerIn(self, master string) Inputs {
	return Inputs{
		SelfID: self, GroupID: "g", MasterID: master,
		MyCaps:  state.Capabilities{Render: true},
		Members: []state.NodeRecord{renderNode(master), renderNode(self)},
		ClockOK: true,
	}
}

func TestApply_SoloStartsOriginAndClockServer(t *testing.T) {
	r := &recHooks{}
	e := NewEngine("n1", r.hooks())
	e.SetClockHealth(0, true)

	d := e.Apply(soloIn("n1"))
	if d.Role != RoleSolo || !d.IsMaster {
		t.Fatalf("role=%v master=%v, want solo master", d.Role, d.IsMaster)
	}
	for _, want := range []string{"clksrv.start", "origin.start", "origin.resume", "render.start"} {
		if !r.has(want) {
			t.Errorf("missing %q in %v", want, r.calls)
		}
	}
	if r.has("clkfol.start") || r.has("recv.start") {
		t.Errorf("solo started follower parts: %v", r.calls)
	}
}

func TestApply_FollowerStartsClockFollowerReceiverReprime(t *testing.T) {
	r := &recHooks{}
	e := NewEngine("n2", r.hooks())
	e.SetClockHealth(1, true) // healthy

	d := e.Apply(followerIn("n2", "n1"))
	if d.Role != RoleFollower {
		t.Fatalf("role=%v, want follower", d.Role)
	}
	for _, want := range []string{"clkfol.start", "recv.start", "recv.reprime", "render.start"} {
		if !r.has(want) {
			t.Errorf("missing %q in %v", want, r.calls)
		}
	}
	if r.has("clksrv.start") || r.has("origin.start") {
		t.Errorf("follower started master parts: %v", r.calls)
	}
}

func TestApply_Idempotent(t *testing.T) {
	r := &recHooks{}
	e := NewEngine("n1", r.hooks())
	e.SetClockHealth(0, true)

	e.Apply(soloIn("n1"))
	r.reset()
	// Re-apply identical inputs ⇒ no redundant start/stop.
	e.Apply(soloIn("n1"))
	if len(r.calls) != 0 {
		t.Errorf("redundant calls on idempotent re-Apply: %v", r.calls)
	}
}

func TestApply_FollowerToMasterFence(t *testing.T) {
	r := &recHooks{}
	e := NewEngine("n2", r.hooks())
	e.SetClockHealth(1, true)

	e.Apply(followerIn("n2", "n1")) // follower
	r.reset()

	// n2 promoted to master: must stop follower parts THEN start master parts.
	promote := Inputs{
		SelfID: "n2", GroupID: "g", MasterID: "n2", Generation: 2,
		MyCaps:  state.Capabilities{Render: true},
		Members: []state.NodeRecord{renderNode("n2"), renderNode("n3")},
	}
	d := e.Apply(promote)
	if !d.IsMaster {
		t.Fatalf("not master after promotion")
	}
	// follower parts stopped before origin start.
	if !r.has("clkfol.stop") || !r.has("recv.stop") {
		t.Errorf("follower parts not stopped on promotion: %v", r.calls)
	}
	if !r.has("clksrv.start") || !r.has("origin.start") || !r.has("origin.resume") {
		t.Errorf("master parts not started on promotion: %v", r.calls)
	}
	if idxBefore(r.calls, "render.stop") > idxBefore(r.calls, "origin.start") {
		// render must be torn down as part of follower parts before origin
		t.Errorf("ordering: render.stop should precede origin.start: %v", r.calls)
	}
}

func TestApply_GenerationFenceRestartsOrigin(t *testing.T) {
	r := &recHooks{}
	e := NewEngine("n1", r.hooks())
	e.SetClockHealth(0, true)

	// Master with two members, generation 1.
	in := Inputs{
		SelfID: "n1", GroupID: "g", MasterID: "n1", Generation: 1,
		MyCaps:  state.Capabilities{Render: true},
		Members: []state.NodeRecord{renderNode("n1"), renderNode("n2")},
	}
	e.Apply(in)
	r.reset()

	// New election generation (still n1 master) ⇒ streamGen bump ⇒ origin torn
	// down and restarted under the new generation (fence).
	in.Generation = 2
	d := e.Apply(in)
	if d.StreamGen != 2 {
		t.Fatalf("streamGen = %d, want 2", d.StreamGen)
	}
	if r.count("origin.stop") != 1 || r.count("origin.start") != 1 {
		t.Errorf("origin not re-keyed exactly once: %v", r.calls)
	}
	// clock server is NOT restarted (master stays master) — A.14.4.
	if r.has("clksrv.stop") || r.has("clksrv.start") {
		t.Errorf("clock server churned across generation bump: %v", r.calls)
	}
}

func TestApply_RenderToggleOnly(t *testing.T) {
	r := &recHooks{}
	e := NewEngine("n2", r.hooks())
	e.SetClockHealth(1, true)

	e.Apply(followerIn("n2", "n1")) // render-capable follower
	r.reset()

	// Disable render: T11 follower → control-only member; only render/receiver
	// teardown, clock follower stays up.
	off := followerIn("n2", "n1")
	off.MyCaps.Render = false
	e.Apply(off)
	if !r.has("render.stop") {
		t.Errorf("render not stopped on T11 disable: %v", r.calls)
	}
	if r.has("clkfol.stop") {
		t.Errorf("clock follower wrongly stopped on render toggle: %v", r.calls)
	}
	r.reset()

	// Re-enable: control-only member → follower; start receiver + render.
	e.Apply(followerIn("n2", "n1"))
	if !r.has("render.start") || !r.has("recv.start") {
		t.Errorf("render/receiver not restarted on T11 enable: %v", r.calls)
	}
}

func idxBefore(calls []string, s string) int {
	for i, c := range calls {
		if c == s {
			return i
		}
	}
	return len(calls)
}

// TestApply_NilHooksSafe verifies partial wiring (e.g. P3, no audio) is a no-op,
// not a panic.
func TestApply_NilHooksSafe(t *testing.T) {
	e := NewEngine("n1", Hooks{}) // all nil
	e.SetClockHealth(0, true)
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("nil hooks panicked: %v", v)
		}
	}()
	e.Apply(soloIn("n1"))
	e.Apply(followerIn("n1", "n2"))
}

func TestApply_IntegrationLifecycle(t *testing.T) {
	// Scripted sequence mirroring doc 04 §4.6: create→add member→master lost→
	// re-point→remove. Asserts the role/streamGen trajectory.
	r := &recHooks{}
	e := NewEngine("n2", r.hooks())
	e.SetClockHealth(1, true)

	// 1. create solo (n2 alone, master itself)
	d := e.Apply(soloIn("n2"))
	if d.Role != RoleSolo || d.StreamGen != 1 {
		t.Fatalf("step1: role=%v gen=%d", d.Role, d.StreamGen)
	}

	// 2. add member n1 who wins election ⇒ n2 becomes follower
	d = e.Apply(followerIn("n2", "n1"))
	if d.Role != RoleFollower {
		t.Fatalf("step2: role=%v want follower", d.Role)
	}

	// 3. master lost ⇒ n2 promoted (T4): streamGen bumps
	promote := Inputs{
		SelfID: "n2", GroupID: "g", MasterID: "n2", Generation: 5,
		MyCaps: state.Capabilities{Render: true}, ClockOK: true,
		Members: []state.NodeRecord{renderNode("n2"), renderNode("n3")},
	}
	d = e.Apply(promote)
	if d.Role != RoleMaster || !d.IsMaster {
		t.Fatalf("step3: role=%v master=%v", d.Role, d.IsMaster)
	}
	genAfterPromote := d.StreamGen

	// 4. n3 removed ⇒ n2 becomes solo (T10), no streamGen change, no restart.
	// Election generation is unchanged (membership shrink does not re-elect n2).
	soloStep := soloIn("n2")
	soloStep.Generation = 5
	d = e.Apply(soloStep)
	if d.Role != RoleSolo {
		t.Fatalf("step4: role=%v want solo", d.Role)
	}
	if d.StreamGen != genAfterPromote {
		t.Errorf("step4: streamGen changed on master→solo: %d→%d", genAfterPromote, d.StreamGen)
	}
}
