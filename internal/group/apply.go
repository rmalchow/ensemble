package group

// applyRole (A.14.4): the idempotent reconciler that drives the role subsystems
// (clock server/origin vs clock follower/receiver/render) from the resolved
// Decision, fenced by Generation/StreamGen so a superseded master cannot keep
// emitting on the group's planes. The engine imports none of stream/*+audio/*;
// the side-effecting lifecycle is reached only through the Hooks function-value
// seam, wired in cmd/ensemble (mirroring web.Deps, doc 01 §2 / README A.14.3).

import (
	"sync"
	"time"
)

// Hooks is the side-effecting seam (origin/receiver/clock/render lifecycle). cmd
// wires it to stream/* + audio/render + clock; tests use a fake. Any nil hook is
// treated as a no-op so partial wiring (e.g. P3, no audio) is safe.
type Hooks struct {
	StartClockServer func(group string) error
	StopClockServer  func()

	StartOrigin    func(group string, streamGen uint64) error
	StopOrigin     func()
	OriginResumeAt func(sampleIndex int64, playing bool) // A.14.4 / R4

	StartClockFollower func(group, masterAddr string) error
	StopClockFollower  func()

	StartReceiver        func(group string) error
	StopReceiver         func()
	ReceiverFlushReprime func() // R11

	StartRender func() error
	StopRender  func()
}

// Engine owns the role lifecycle for the local node's one group. Apply is the
// single entry point: it recomputes the Decision and idempotently reconciles the
// subsystems. It is safe for concurrent callers (Apply serializes under mu).
type Engine struct {
	selfID string
	hooks  Hooks

	mu         sync.Mutex
	running    Decision                            // the currently-applied decision (zero value = nothing running)
	started    bool                                // whether running reflects an applied decision yet
	orphan     orphanGate                          // hysteretic MinDelay gate (doc 04 §4.2.3)
	masterAddr func(group, masterID string) string // resolves the elected master's clk addr

	// Latest clock-quality window fed via SetClockHealth (guarded by mu).
	lastMinDelay time.Duration
	haveMinDelay bool
}

// NewEngine creates an engine for selfID wired to hooks. The master clock-address
// resolver is supplied separately via WithMasterAddr (cmd closes over cluster);
// without it StartClockFollower receives an empty address (acceptable for tests
// using a fake Hooks).
func NewEngine(selfID string, hooks Hooks) *Engine {
	return &Engine{selfID: selfID, hooks: hooks}
}

// WithMasterAddr sets the resolver mapping (group, masterID) → the master's clock
// plane address, used when starting the clock follower. Returns the engine for
// chaining. cmd wires this from cluster/state; group never resolves addresses
// itself (keeps the engine pure of network knowledge, P3.2 risk 4).
func (e *Engine) WithMasterAddr(fn func(group, masterID string) string) *Engine {
	e.masterAddr = fn
	return e
}

// Apply recomputes the decision from in, folds in the hysteretic orphan gate, and
// idempotently reconciles subsystems fenced by Generation/StreamGen (A.14.4).
// Returns the applied Decision.
func (e *Engine) Apply(in Inputs) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Resolve the hysteretic orphan signal into the pure-function input. The gate
	// only matters for a render-capable non-master member with a master elected;
	// in every other case Recompute ignores MinDelayOK (master/solo are never
	// orphan; a control-only member never renders).
	in.MinDelayOK = !e.orphan.observe(in.ClockOK, e.lastMinDelay, e.haveMinDelay)

	prev := e.running
	d := Recompute(prev, in)

	e.reconcile(prev, d, in)

	e.running = d
	e.started = true
	return d
}

// Shutdown tears the engine's running subsystems down to nothing (used when a
// group is removed from the ConfigDoc, mirroring cluster.GroupElections.Drop).
// It reconciles the currently-running decision to the all-stopped zero decision,
// so exactly the started subsystems are stopped once, then marks the engine
// un-started. Idempotent: a second call (or a call on a never-applied engine) is
// a no-op.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return
	}
	prev := e.running
	e.stopFollowerParts(prev)
	e.stopMasterParts(prev)
	e.running = Decision{}
	e.started = false
}

// SetClockHealth feeds the latest clock-quality window into the engine's orphan
// gate (doc 04 §4.2.3). cmd calls this from the clock.Follower sample hook; the
// next Apply consults the resolved hysteresis. Kept separate from Inputs so the
// gate's window history lives in the (stateful) engine, not the pure function.
func (e *Engine) SetClockHealth(minDelay time.Duration, ok bool) {
	e.mu.Lock()
	e.lastMinDelay = minDelay
	e.haveMinDelay = ok
	e.mu.Unlock()
}

// reconcile diffs prev→next and calls only the hooks needed to converge, honoring
// the generation/streamGen fence: a master subsystem started under an older
// streamGen is torn down before the new one starts (A.14.4).
func (e *Engine) reconcile(prev, next Decision, in Inputs) {
	first := !e.started

	// ── Generation fence ────────────────────────────────────────────────────
	// If we were master and the stream generation advanced (master change /
	// profile change / re-election), tear the origin down before re-starting it
	// under the new streamGen so nothing emits on a stale generation.
	masterRestart := next.IsMaster && prev.IsMaster && next.StreamGen != prev.StreamGen

	// ── Origin + clock server (master/solo) ──────────────────────────────────
	switch {
	case next.RunOrigin && (first || !prev.RunOrigin || masterRestart):
		// Becoming (or re-keying) master: stop any follower parts first (A.14.4).
		e.stopFollowerParts(prev)
		if masterRestart {
			call(e.hooks.StopOrigin)
		}
		if !prev.RunClockSrv || first {
			callErr(e.hooks.StartClockServer, in.GroupID)
		}
		if e.hooks.StartOrigin != nil {
			_ = e.hooks.StartOrigin(in.GroupID, next.StreamGen)
		}
		if e.hooks.OriginResumeAt != nil {
			e.hooks.OriginResumeAt(in.SampleIndex, in.Playing) // R4
		}
	case !next.RunOrigin && prev.RunOrigin:
		call(e.hooks.StopOrigin)
		if !next.RunClockSrv {
			call(e.hooks.StopClockServer)
		}
	}

	// ── Clock follower (follower / control-only member / orphan) ─────────────
	switch {
	case next.RunClockFol && (first || !prev.RunClockFol):
		e.stopMasterParts(prev)
		if e.hooks.StartClockFollower != nil {
			e.hooks.StartClockFollower(in.GroupID, e.resolveMaster(in))
		}
	case !next.RunClockFol && prev.RunClockFol:
		call(e.hooks.StopClockFollower)
	case next.RunClockFol && prev.RunClockFol && prev.IsMaster != next.IsMaster:
		// (defensive) role flipped but both keep a follower — re-point.
		call(e.hooks.StopClockFollower)
		if e.hooks.StartClockFollower != nil {
			e.hooks.StartClockFollower(in.GroupID, e.resolveMaster(in))
		}
	}

	// ── Receiver (follower / orphan, never control-only member) ──────────────
	switch {
	case next.RunReceiver && (first || !prev.RunReceiver):
		if e.hooks.StartReceiver != nil {
			e.hooks.StartReceiver(in.GroupID)
		}
		call(e.hooks.ReceiverFlushReprime) // R11: ~one buffer on (re)start
	case !next.RunReceiver && prev.RunReceiver:
		call(e.hooks.StopReceiver)
	}

	// ── Local render (gated on Caps.Render; T11 toggles ONLY this) ───────────
	switch {
	case next.RunRender && (first || !prev.RunRender):
		if e.hooks.StartRender != nil {
			_ = e.hooks.StartRender()
		}
	case !next.RunRender && prev.RunRender:
		call(e.hooks.StopRender)
	}
}

// stopFollowerParts tears down the clock follower + receiver + render if prev had
// them (A.14.4 "stop_follower_parts").
func (e *Engine) stopFollowerParts(prev Decision) {
	if prev.RunRender {
		call(e.hooks.StopRender)
	}
	if prev.RunReceiver {
		call(e.hooks.StopReceiver)
	}
	if prev.RunClockFol {
		call(e.hooks.StopClockFollower)
	}
}

// stopMasterParts tears down the clock server + origin if prev had them (A.14.4
// "stop_master_parts").
func (e *Engine) stopMasterParts(prev Decision) {
	if prev.RunOrigin {
		call(e.hooks.StopOrigin)
	}
	if prev.RunClockSrv {
		call(e.hooks.StopClockServer)
	}
}

func (e *Engine) resolveMaster(in Inputs) string {
	if e.masterAddr == nil {
		return ""
	}
	return e.masterAddr(in.GroupID, in.MasterID)
}

func call(fn func()) {
	if fn != nil {
		fn()
	}
}

func callErr(fn func(string) error, arg string) {
	if fn != nil {
		_ = fn(arg)
	}
}
