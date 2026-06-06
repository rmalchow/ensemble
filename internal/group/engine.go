package group

// The group engine state machine (doc 04 §4.2). Recompute is a PURE function of
// Inputs (election result + membership + profile + self caps + clock health): it
// is recomputed on every gossip/election tick and never infers role from packets
// (the control plane is authoritative, doc 04 §4.2.2). The resolved Decision says
// which role/posture this node occupies and which subsystems applyRole must run.
//
// States (doc 04 §4.2): solo = master with an empty follower set; master; an
// elected-other render-capable member is follower (or orphan when sync is bad);
// a Render=false non-master member is the control-only member posture (clock
// follower for liveness only — no receiver/render, doc 04 §4.2.4). A
// Render=false master/solo is the "master, no local render" posture.

import "gitlab.rand0m.me/ruben/go/ensemble/internal/state"

// Role is the per-node group role (doc 04 §4.2).
type Role int

const (
	// RoleStarting is the pre-decision zero value (∅ in the state diagram).
	RoleStarting Role = iota
	RoleSolo
	RoleMaster
	RoleFollower
	RoleOrphan
)

// Posture overlays Role for the render-orthogonal sink-less variants (doc 04
// §4.2 modifiers).
type Posture int

const (
	PostureNormal Posture = iota
	// PostureNoLocalRender: a Render=false master/solo (origin + clock, no sink).
	PostureNoLocalRender
	// PostureControlOnlyMember: a Render=false non-master member (clock follower
	// for liveness only — no receiver/decode/ring/render, doc 04 §4.2.4).
	PostureControlOnlyMember
)

// Inputs is the complete pure-function input (doc 04 §4.2.2 "Determinism").
type Inputs struct {
	SelfID      string
	GroupID     string
	Members     []state.NodeRecord // this group's members (from ConfigDoc; P2.x state)
	MasterID    string             // election outcome (P2.3 cluster.GroupElections)
	Generation  uint64             // per-group election generation (P2.3; A.5)
	Profile     Profile            // published GroupRecord.profile
	Playing     bool               // replicated GroupRecord.Playing (R4)
	SampleIndex int64              // replicated/last-projected sample for Seed continuity
	MyCaps      state.Capabilities // self effective caps (README §6.5)
	ClockOK     bool               // Offset().ok
	MinDelayOK  bool               // MinDelay under the orphan threshold (orphan.go resolved)
	GroupMoved  bool               // own GroupID just changed (R13 m1 transient) ⇒ orphan/silence
}

// Decision is the resolved role/posture plus the subsystems applyRole must run.
type Decision struct {
	Role        Role
	Posture     Posture
	IsMaster    bool
	RunOrigin   bool // start stream origin + clock server
	RunReceiver bool // start stream receiver
	RunRender   bool // start local render (gated on MyCaps.Render)
	RunClockSrv bool
	RunClockFol bool
	StreamGen   uint64 // bumped on master change / profile change (A.5/§4.3.3)

	// Provenance carried so Recompute stays a pure function of (prev, in): the
	// profile and election generation that the current StreamGen was keyed to.
	// Unexported, so they do not widen the public API and default to zero in
	// test-constructed Decisions.
	streamProfile Profile
	electionGen   uint64
}

// Recompute is the pure transition function (doc 04 §4.2.2, transitions T1-T11);
// safe to call on every gossip/election tick. prev carries the StreamGen and the
// previously-resolved role so a master change / profile change bumps StreamGen
// exactly once (A.5/§4.3.3), and so promotion from solo→master reuses the
// existing origin/clock (T2/T10 need no restart).
func Recompute(prev Decision, in Inputs) Decision {
	d := Decision{StreamGen: prev.StreamGen}

	iAmMaster := in.MasterID != "" && in.MasterID == in.SelfID
	render := in.MyCaps.Render

	switch {
	// Group-move transient (R13 m1 / doc 04 §4.6): on observing our own GroupID
	// change, orphan (silence) until the new group's master+clock+stream are in
	// hand. A Render=false node has nothing to render, so it skips straight to
	// its normal posture below rather than orphaning.
	case in.GroupMoved && render:
		d.Role = RoleOrphan
		d.Posture = PostureNormal
		d.RunClockFol = true // keep trying to acquire the new master's clock

	case iAmMaster:
		// solo = master with empty follower set (doc 04 §4.2). "Members" includes
		// self; a lone member (or none) ⇒ solo.
		if otherMembers(in) == 0 {
			d.Role = RoleSolo
		} else {
			d.Role = RoleMaster
		}
		d.IsMaster = true
		d.RunOrigin = true
		d.RunClockSrv = true
		if render {
			d.RunRender = true // solo/master loopback render
		} else {
			d.Posture = PostureNoLocalRender // "master, no local render"
		}

	case !render:
		// Render=false non-master member: control-only posture (doc 04 §4.2.4) —
		// clock follower for liveness only, never follower/orphan.
		d.Role = RoleFollower
		d.Posture = PostureControlOnlyMember
		d.RunClockFol = true

	case in.MasterID == "" || !in.ClockOK || !in.MinDelayOK:
		// Render-capable member with no usable sync ⇒ orphan (T6): hold render,
		// keep the follower running to re-acquire (doc 04 §4.2.3).
		d.Role = RoleOrphan
		d.RunClockFol = true
		d.RunReceiver = true // keep receiving to anchor the timeline on recovery

	default:
		// Render-capable member, elected other, healthy sync ⇒ follower (T3/T7).
		d.Role = RoleFollower
		d.RunClockFol = true
		d.RunReceiver = true
		d.RunRender = true
	}

	d.StreamGen, d.streamProfile, d.electionGen = nextStreamGen(prev, in, d)
	return d
}

// otherMembers counts members of the group other than self.
func otherMembers(in Inputs) int {
	n := 0
	for _, m := range in.Members {
		if m.ID != in.SelfID {
			n++
		}
	}
	return n
}

// nextStreamGen advances StreamGen exactly when this node, AS master, must start
// a new generation: on becoming master (master change, A.5/R11) or on a profile
// change while master (renegotiation, doc 04 §4.3.3). Followers never bump it;
// they adopt the master's generation from the wire. Inputs.Generation (the
// election generation) is mixed in so that re-election to the same node under a
// new generation also re-keys the stream (a superseded master cannot reuse a
// stale streamGen — the applyRole fence relies on this monotonicity).
func nextStreamGen(prev Decision, in Inputs, d Decision) (gen uint64, prof Profile, elGen uint64) {
	if !d.IsMaster {
		// Followers/control-only/orphan adopt the master's generation off the
		// wire; the engine carries prev.StreamGen unchanged and keys provenance
		// to the current inputs so a later promotion bumps correctly.
		return prev.StreamGen, in.Profile, in.Generation
	}
	becameMaster := !prev.IsMaster
	profileChanged := prev.IsMaster && prev.streamProfile != in.Profile
	genChanged := prev.IsMaster && prev.electionGen != in.Generation
	if becameMaster || profileChanged || genChanged {
		return prev.StreamGen + 1, in.Profile, in.Generation
	}
	return prev.StreamGen, in.Profile, in.Generation
}
