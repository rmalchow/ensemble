package daemon

import (
	"context"
	"testing"
	"time"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/group"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/state"
)

// healthyClock returns a MinDelay comfortably under the orphan exit threshold so
// a render-capable follower resolves to the healthy follower (not orphan) state.
const healthyClock = 2 * time.Millisecond

// feedHealthy feeds good clock health into the node's engine so a follower exits
// the (default) orphan posture and reaches RunRender.
func feedHealthy(n *Node) {
	if re := n.engineFor(); re != nil && re.engine != nil {
		re.engine.SetClockHealth(healthyClock, true)
	}
}

// role_test.go drives the P4.9 role wiring (role.go) through a REAL group.Engine
// wired to a recording Hooks, asserting that runMaster/runFollower resolve the
// right group.Inputs so the engine starts/stops the correct subsystems
// (transitions T1-T5 + the sink-less postures, P4.9 §7.1). No sockets: the hooks
// only record calls. The generation fence itself is covered by the existing
// TestApplyRoleFence (daemon_test.go) over the roleState shape.

// recordHooks records which engine hooks fired.
type recordHooks struct {
	clockSrv     int
	origin       int
	resumeAt     []resumeArg
	clockFol     int
	receiver     int
	flushReprime int
	render       int
	stopOrigin   int
	stopRender   int
	stopRecv     int
}

type resumeArg struct {
	sample  int64
	playing bool
}

func (h *recordHooks) hooks() group.Hooks {
	return group.Hooks{
		StartClockServer:     func(string) error { h.clockSrv++; return nil },
		StopClockServer:      func() {},
		StartOrigin:          func(string, uint64) error { h.origin++; return nil },
		StopOrigin:           func() { h.stopOrigin++ },
		OriginResumeAt:       func(s int64, p bool) { h.resumeAt = append(h.resumeAt, resumeArg{s, p}) },
		StartClockFollower:   func(string, string) error { h.clockFol++; return nil },
		StopClockFollower:    func() {},
		StartReceiver:        func(string) error { h.receiver++; return nil },
		StopReceiver:         func() { h.stopRecv++ },
		ReceiverFlushReprime: func() { h.flushReprime++ },
		StartRender:          func() error { h.render++; return nil },
		StopRender:           func() { h.stopRender++ },
	}
}

// roleNode builds a Node whose transport.roleEngine is a real engine wired to rec,
// with an inputs resolver projecting from doc. render controls self's Caps.Render.
func roleNode(self string, doc state.ConfigDoc, rec *recordHooks) *Node {
	n := New(Options{NodeID: self})
	engine := group.NewEngine(self, rec.hooks())
	n.tx = &transport{
		self: self,
		roleEngine: &roleEngine{
			engine: engine,
			inputs: func(s, master string, gen uint64) group.Inputs {
				return resolveInputs(doc, "g1", s, master, gen)
			},
		},
	}
	return n
}

func TestRoleTransitions(t *testing.T) {
	const self = "node-a"

	t.Run("T1 init->solo: clock server + origin + local render", func(t *testing.T) {
		doc := docWithGroup("g1", []string{self}, "t.mp3", false)
		rec := &recordHooks{}
		n := roleNode(self, doc, rec)
		n.runMaster(context.Background(), 1)
		if rec.clockSrv != 1 || rec.origin != 1 || rec.render != 1 {
			t.Fatalf("solo start: clockSrv=%d origin=%d render=%d, want 1/1/1", rec.clockSrv, rec.origin, rec.render)
		}
		if len(rec.resumeAt) != 1 {
			t.Fatalf("OriginResumeAt called %d times, want 1", len(rec.resumeAt))
		}
	})

	t.Run("master sink-less: origin+clock, NO local render", func(t *testing.T) {
		doc := state.ConfigDoc{
			Nodes:  []state.NodeRecord{{ID: self, Caps: state.Capabilities{Render: false}}},
			Groups: []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{self}, Media: state.MediaSelection{File: "t.mp3"}}},
		}
		rec := &recordHooks{}
		n := roleNode(self, doc, rec)
		n.runMaster(context.Background(), 1)
		if rec.clockSrv != 1 || rec.origin != 1 {
			t.Fatalf("sink-less master: clockSrv=%d origin=%d, want 1/1", rec.clockSrv, rec.origin)
		}
		if rec.render != 0 {
			t.Fatalf("sink-less master started local render (D17 violation): render=%d", rec.render)
		}
	})

	t.Run("T3 ->follower: clock follower + receiver + render + FlushReprime", func(t *testing.T) {
		// Two members, master is node-z (other), self is render-capable follower.
		doc := docWithGroup("g1", []string{self, "node-z"}, "t.mp3", true)
		rec := &recordHooks{}
		n := roleNode(self, doc, rec)
		feedHealthy(n)
		n.runFollower(context.Background(), 1, "node-z")
		if rec.clockFol != 1 || rec.receiver != 1 || rec.render != 1 {
			t.Fatalf("follower: clockFol=%d receiver=%d render=%d, want 1/1/1", rec.clockFol, rec.receiver, rec.render)
		}
		if rec.flushReprime != 1 {
			t.Fatalf("FlushAndReprime called %d times, want 1 (R11)", rec.flushReprime)
		}
		if rec.origin != 0 {
			t.Fatalf("follower started origin: origin=%d", rec.origin)
		}
	})

	t.Run("T3' ->member sink-less: clock follower ONLY", func(t *testing.T) {
		doc := state.ConfigDoc{
			Nodes: []state.NodeRecord{
				{ID: self, Caps: state.Capabilities{Render: false}},
				{ID: "node-z", Caps: state.Capabilities{Render: true}},
			},
			Groups: []state.GroupRecord{{ID: "g1", MemberNodeIDs: []string{self, "node-z"}, Playing: true}},
		}
		rec := &recordHooks{}
		n := roleNode(self, doc, rec)
		n.runFollower(context.Background(), 1, "node-z")
		if rec.clockFol != 1 {
			t.Fatalf("control-only member: clockFol=%d, want 1", rec.clockFol)
		}
		if rec.receiver != 0 || rec.render != 0 {
			t.Fatalf("control-only member started receiver/render: receiver=%d render=%d (04 §4.2.4)", rec.receiver, rec.render)
		}
	})

	t.Run("T4 promote follower->master: Seed + origin (resume) + render", func(t *testing.T) {
		doc := docWithGroup("g1", []string{self, "node-z"}, "t.mp3", true)
		rec := &recordHooks{}
		n := roleNode(self, doc, rec)
		feedHealthy(n)
		// Start as follower against node-z.
		n.runFollower(context.Background(), 1, "node-z")
		// Promote: elected master is now self under a new generation.
		n.runMaster(context.Background(), 2)
		if rec.origin != 1 || rec.clockSrv != 1 {
			t.Fatalf("promotion: origin=%d clockSrv=%d, want 1/1", rec.origin, rec.clockSrv)
		}
		if rec.stopRecv == 0 || rec.stopRender == 0 {
			t.Fatalf("promotion did not stop follower parts: stopRecv=%d stopRender=%d", rec.stopRecv, rec.stopRender)
		}
		// OriginResumeAt carries the replicated Playing=true (04 §4.4.4).
		last := rec.resumeAt[len(rec.resumeAt)-1]
		if !last.playing {
			t.Fatalf("OriginResumeAt playing=%v, want true (resume from sampleIndex)", last.playing)
		}
	})
}
