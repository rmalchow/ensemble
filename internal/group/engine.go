package group

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Typed errors surfaced to the API (I), which maps them to HTTP responses.
var (
	ErrNotMaster      = errors.New("group: not the group master (use takeover)") // §9.1 hint
	ErrTargetUnknown  = errors.New("group: follow/takeover target unknown")      // §5.1/§5.2
	ErrTargetDead     = errors.New("group: follow target not alive")             // §5.1
	ErrTargetFollower = errors.New("group: follow target is not a master")       // §5.1
	ErrSelfFollow     = errors.New("group: cannot follow self")                  // §5.1
	ErrNoOpus         = errors.New("group: opus codec not supported")            // §8.3/D33
	ErrBadSettings    = errors.New("group: invalid group settings")              // §9.1
	ErrNotSynced      = errors.New("group: clock not synced yet, retry")         // §7 transient
	ErrNotPlaying     = errors.New("group: nothing is playing")                  // D39 pause
	ErrNotPaused      = errors.New("group: not paused")                          // D39 resume
	ErrClosed         = errors.New("group: engine closed")
)

// Params bundles everything H needs, injected by main (K).
type Params struct {
	Cluster  Cluster                // C: read + owner setters + DialCandidates
	Media    MediaFactory           // D: scheme-keyed media-source factory (§6.1)
	Opus     OpusFactory            // D: opus encoder factory (D33); may be nil (no opus)
	Source   SourceServer           // G internal/source: master-side fan-out
	Sub      Subscriber             // G internal/stream: member-side subscribe client
	Sink     contracts.Sink         // E: local playout (H calls Reset only)
	Clock    contracts.Clock        // F: LocalToMaster for pts stamping (master)
	ClockCtl ClockControl           // F: SetMaster re-point (member, §7/D17)
	Follow   contracts.FollowClient // I: takeover HTTP fan-out (§5.2)
	Caps     contracts.Capabilities // this node's own caps (codec gating, §8.3)
	Log      *slog.Logger

	// Knobs (defaults applied in New when zero):
	Grace     time.Duration // self-heal grace, default 10 s (§5)
	LeadMs    int           // source release lead, default contracts.DefaultLeadMs (§8.2)
	Heartbeat time.Duration // playback position/SourceStats refresh, default 5 s (D28)

	// now is the time source (test seam). Defaults to time.Now.
	now func() time.Time
}

// Engine is the group brain. One per node. One mutex guards all mutable fields.
type Engine struct {
	p    Params
	log  *slog.Logger
	self id.ID
	now  func() time.Time

	mu sync.Mutex

	// playback (master-only)
	sess *session // current session, nil = idle
	gen  uint32   // monotonic per-node session generation (§8.4); never reused

	// reconcile / member-side tracking — what the local plumbing is pointed at
	curMaster  id.ID     // master this node currently tracks (Zero = none)
	curGen     uint32    // generation the subscriber+sink+clock are armed for
	curPlaying bool      // stream subscription + sink are armed (session active)
	haveCur    bool      // curMaster/curGen have been set at least once
	healAt     time.Time // when stale `following` becomes eligible for reset (zero=none)

	lastBeat time.Time // last heartbeat SetPlayback (master, while playing)

	// observed group composition (for logging membership/role/master changes)
	prevRole    role
	prevMaster  id.ID
	prevMembers map[id.ID]bool
	havePrev    bool

	// 1 Hz playing-stats throttle (one INFO line per second per side)
	lastStats time.Time

	closed bool
	done   chan struct{}
	wg     sync.WaitGroup
}

// New builds an Engine and applies knob defaults. Starts no goroutines.
func New(p Params) *Engine {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if p.Grace == 0 {
		p.Grace = 10 * time.Second
	}
	if p.LeadMs == 0 {
		p.LeadMs = contracts.DefaultLeadMs
	}
	if p.Heartbeat == 0 {
		p.Heartbeat = 5 * time.Second
	}
	if p.now == nil {
		p.now = time.Now
	}
	return &Engine{
		p:    p,
		log:  p.Log.With("comp", "group"),
		self: p.Cluster.Self(),
		now:  p.now,
		done: make(chan struct{}),
	}
}

// Close stops the reconcile goroutine, halts any running session (without
// rewriting the replicated doc, §3.4), BYEs the subscriber, and returns.
// Idempotent.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	close(e.done)
	e.stopLocked() // halts current session if any (releases+re-takes e.mu)
	e.mu.Unlock()

	e.p.Sub.Unsubscribe()
	e.wg.Wait()
	return nil
}

// Follow makes THIS node follow target (§5.1): validates target is alive and a
// master, then SetFollowing(target). Typed error on rejection.
func (e *Engine) Follow(target id.ID) error { return e.follow(target) }

// Unfollow makes THIS node a solo master: SetFollowing(Zero) (§5.1).
func (e *Engine) Unfollow() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	e.p.Cluster.SetFollowing(id.Zero)
	e.log.Info("unfollowing (now solo)", "reason", "user")
	return nil
}

// Settings returns the effective group settings for this node's group.
func (e *Engine) Settings() contracts.GroupSettings {
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if !mv.found {
		return defaultSettings()
	}
	return fillDefaults(mv.group.Settings)
}

// Group returns this node's current derived group view (for /api/status §9.1).
func (e *Engine) Group() contracts.GroupView {
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	return mv.group // zero value when not yet derived
}

// SourceStats returns the live source stats when this node runs a session, plus
// ok=false when idle (for /api/status §9.1/D19).
func (e *Engine) SourceStats() (contracts.SourceStats, bool) {
	e.mu.Lock()
	active := e.sess != nil
	e.mu.Unlock()
	if !active {
		return contracts.SourceStats{}, false
	}
	return e.p.Source.Stats(), true
}
