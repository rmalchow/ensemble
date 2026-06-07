# H — group engine

Source of truth: [docs/README.md](../README.md) §5, §5.1, §5.2, §6, §6.1, §7,
§8.2, §8.5, §8.6, §8.7, §9.1. Integrator decisions:
[DECISIONS.md](DECISIONS.md) — binding where they amend any piece doc; the ones
H lives by are **D5** (C owns derivation), **D9** (`ReadFrame(dst)` EOF),
**D10** (`Clock.LocalToMaster`), **D14** (cluster write-side), **D16**
(`FollowClient` from I), **D17** (takeover runs on master; clock re-pointing is
H's), **D22** (subscribe model on SOURCE_PORT — *supersedes* the old push
model: no `Sender`/`SetEndpoints`/`Resolver`/`Endpoint`), **D23** (live settings
via RECONFIG), **D26** (scheme-keyed media-source factory), **D28**
(`Playback.Source` stats surfaced on the periodic position update). Shared
contracts: [S-skeleton.md](S-skeleton.md) — `id`, `contracts`, `stream`.

This piece owns **`internal/group/*` only.**

H is the per-node group brain. It:

1. reads the **already-derived** groups out of the cluster snapshot (§5, D5 — H
   does **not** re-derive; C's `cluster.DeriveGroups` fills `Snapshot.Groups`)
   and finds this node's own group;
2. enforces the `following`-field transitions — **follow / unfollow** (§5.1)
   with validation, **self-heal** to solo after a 10 s grace (§5), and
   **takeover** orchestration (§5.2, on the master only, D17);
3. runs the **master-side playback session**: open a media source for a URI
   (D's scheme factory, §6.1/D26) → release canonical frames on a 20 ms ticker,
   stamping `pts` via `Clock.LocalToMaster` (§8.2) → hand each frame to the
   **local audio source server** (G's `internal/source`) which every member —
   including this node's own sink — **subscribes** to (§8.2/§8.7/D22);
4. keeps every node's **local stream plumbing pointed at the current master**:
   it tells the local **subscriber client** (G) which master `SOURCE_PORT` and
   generation to track, tells the local **sink** to `Reset(gen)`, and tells the
   local **clock follower** to `SetMaster(addr, gen)` (D17). This is the whole
   of "members follow the current master automatically";
5. applies **live settings changes** (codec/transport/bufferMs) by bumping the
   generation and broadcasting **RECONFIG** through the source server (§8.7/D23);
6. writes the replicated **playback-status** record (master-only, §4), including
   `Playback.Source` stats refreshed on the **5 s position heartbeat** (D28).

Design stance (ground rules): smallest thing that satisfies the spec. One
concrete `Engine` type; one concrete `session` type; **one mutex** on the
engine. No interfaces invented beyond the few seams S does not already pin
(media factory, source server, subscriber control). Everything testable with a
fake cluster, fake follow-client, fake media factory, fake source-server, fake
subscriber, fake sink, and a fake clock — no audio hardware, no sockets, no
multicast, no root.

There is **no endpoint management and no `Resolver`** anywhere in H. Per D22 the
subscribe model removed per-member stream endpoints: subscribers dial the
master's `SOURCE_PORT` and the source streams back to the address each
subscription came from. The only address H ever resolves is the **master's**
SOURCE_PORT / STREAM_PORT, via `cluster.DialCandidates(master)`, to point the
local subscriber and clock follower (§3.1, D6/D18).

---

## 1. Package / file layout

Files H creates and owns (`internal/group/`):

```
engine.go      Engine: construction, deps, one mutex, lifecycle (New/Run/Close),
               and the API-facing methods (Follow/Unfollow/MakeMaster/Play/Stop/
               Settings/SetSettings/Group/Status).
watch.go       The reconcile loop: on every cluster change (and a 1 s tick) find
               this node's group, re-point the local subscriber+sink+clock to the
               current master, run self-heal, tear down a session this node may no
               longer host.
mygroup.go     myGroup(snap) helper: locate this node's GroupView in Snapshot.Groups
               + classify role (solo/master/follower) + detect stale `following`.
               Pure read of the pre-derived snapshot — NOT a re-derivation (D5).
follow.go      Follow / Unfollow validation + apply via cluster.SetFollowing (§5.1).
takeover.go    MakeMaster orchestration on the master (§5.2, D17) over FollowClient.
heal.go        10 s self-heal grace: reset own `following` when target invalid (§5).
play.go        Play(uri) / Stop entry points: validate, build session, RECONFIG/stop.
session.go     Playback session: media source → 20 ms ticker release (pts via
               Clock.LocalToMaster) → source-server publish; gen; EOF vs stop.
settings.go    Group-settings get/set, validation + defaults, live apply (D23).
status.go      Playback record assembly + the 5 s position/SourceStats heartbeat (D28).
deps.go        Dependency interfaces H consumes (cluster write view, media factory,
               source server, subscriber control, follow client, clock, caps).
doc.go         Package doc + slog component name "group".

engine_test.go    Lifecycle, reconcile on cluster change, Close idempotency.
mygroup_test.go   myGroup classification truth table over pre-derived snapshots.
follow_test.go    Follow validation (alive master only), unfollow, re-point.
takeover_test.go  Takeover happy path + missed-member tolerance; ErrNotMaster off-master.
heal_test.go      Grace timer: no reset before 10 s, reset after; cancel on recover.
play_test.go      Play rejects non-master / bad URI / opus-without-cap; status written.
session_test.go   Ticker release order, pts via LocalToMaster, publish-all, EOF vs Stop.
settings_test.go  Defaults, validation, master-only write, live RECONFIG + gen bump.
status_test.go    Heartbeat refreshes positionSec + Playback.Source from source stats.
watch_test.go     Re-point local subscriber/sink/clock to master; master-loss teardown.
```

No file exceeds ~250 lines; `session.go` and `watch.go` are the densest.

The old H-group files `derive.go` / `derive_test.go` are **dropped** (D5: C owns
derivation). H reads `Snapshot.Groups`; `mygroup.go` only *locates and
classifies*, it does not compute membership or XOR ids.

---

## 2. Concrete Go API

### 2.1 `deps.go` — what H consumes (and who provides it)

S already pins `contracts.{Snapshot, GroupView, GroupSettings, Playback,
SourceStats, Sink, Clock, FollowClient, Capabilities}` and `id`. The remaining
surfaces — the cluster **write** side (C), the media-source **factory** (D), the
audio **source server** (G `internal/source`), and the member-side **subscriber
control** (G `internal/stream`) — are not in S. Per Go style H declares the
**minimal consumer-side interfaces** it needs locally; the real types satisfy
them structurally, and tests supply fakes.

```go
package group

import (
	"context"
	"net/netip"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Cluster is the slice of the cluster piece (C) that H needs: the read side
// (contracts.StateStore — Self/Snapshot/Subscribe) plus the owner-only setters
// (D14) that write THIS node's record and the per-group LWW records. Implemented
// by *cluster.Cluster.
//
// SetFollowing mutates this node's own `following` (§4 owner-only). SetPlayback /
// SetGroupSettings write group-keyed records and are only ever CALLED by H when
// this node is the group master (H enforces master-only, not C).
// DialCandidates resolves the master's address per §3.1 (D6) so H can point the
// local subscriber + clock follower at it — the ONLY dialing H does (D22: no
// per-member endpoints).
type Cluster interface {
	contracts.StateStore // Self() id.ID; Snapshot() contracts.Snapshot; Subscribe() <-chan struct{}

	SetFollowing(target id.ID)                         // Zero == unfollow/solo (§5.1)
	SetPlayback(group id.ID, p contracts.Playback)     // master-only (§4/§8.6/D28)
	SetGroupSettings(group id.ID, s contracts.GroupSettings) // master-only LWW (§8.3/D23)
	GroupSettings(group id.ID) contracts.GroupSettings // stored or defaults
	DialCandidates(peer id.ID) []netip.Addr            // best-first (§3.1/D6)
}

// MediaSource is one media source as a stream of canonical 20 ms PCM frames
// (§6.1/§8.1, D26). Implemented by the audio piece (D). D9 EOF semantics:
// ReadFrame fills exactly stream.FrameBytes into caller-owned dst; the final
// partial frame is zero-padded and returned with nil; the NEXT call returns
// io.EOF (pull-paced). Live-paced sources (http/input) never return io.EOF —
// when they momentarily underflow they return a transient signal (see below)
// and the session emits a silence frame so the seq/pts cadence never stalls
// (§6.1).
type MediaSource interface {
	// ReadFrame fills dst[:stream.FrameBytes] with the next canonical frame.
	// Pull-paced: io.EOF after the last (padded) frame (§8.6). Live-paced:
	// returns ErrUnderflow (see MediaFactory doc) when no data is ready yet —
	// never io.EOF. Any other error aborts the session.
	ReadFrame(dst []byte) error
	// Paced reports the source's pacing class: false = pull (file; decode-ahead,
	// EOF ends the session), true = live (http/input; never EOF, underflow →
	// silence, ends only on Stop). Set at Open time from the scheme (§6.1).
	Live() bool
	// Close releases the decoder / connection / capture process.
	Close() error
}

// MediaFactory opens a URI into a MediaSource by scheme (§6.1/D26). Implemented
// by the audio piece (D): schemes file / http(s) / input. mediaDir scoping and
// path-traversal rejection for file: URIs live in D's factory, not in H. The
// node's available schemes are reported in capabilities.sources (§1); H does not
// re-check them — an unsupported scheme surfaces as the factory's error.
//
// Live-paced underflow is signalled by the sentinel error ErrUnderflow from
// MediaSource.ReadFrame; D exports it and H references it. (Contract concern: D
// must export ErrUnderflow, or change the underflow signal to a bool return —
// see §6.)
type MediaFactory interface {
	Open(uri string) (MediaSource, error)
}

// SourceServer is the master-side audio source server on SOURCE_PORT
// (§8.2/§8.7/D22–D24), implemented by G's internal/source. H drives ONE session
// at a time through it: StartSession arms a generation (subscriber registry,
// ring buffer, listeners already running for the node's lifetime); Publish hands
// each released frame to fan-out + ring; Reconfig broadcasts RECONFIG to all
// live subscribers (D23, settings change or stop); EndSession tears the session
// down. Stats() feeds the playback heartbeat (D28).
type SourceServer interface {
	// StartSession arms the server for a new generation with the session's codec/
	// transport (so the fan-out path and ring are configured). Resets ring + stats
	// counters that are per-session; subscriber registry persists (keepalive).
	StartSession(gen uint32, s contracts.GroupSettings)
	// Publish fans one released audio frame (gen/seq/pts + canonical-PCM payload)
	// out to every live subscriber and folds it into the ring for burst-priming
	// late joiners (§8.2/D24). Non-blocking best-effort per subscriber.
	Publish(gen uint32, seq uint64, pts int64, payload []byte)
	// Reconfig broadcasts a RECONFIG control (type 0x23) to all live subscribers:
	// stop=false → "settings/gen changed, re-read group settings, resubscribe
	// under newGen" (D23); stop=true → end-of-session notice (§8.6). Subscribers
	// then HELLO again (or unsubscribe on stop).
	Reconfig(newGen uint32, stop bool)
	// EndSession clears the active generation/ring after a Reconfig(stop) so a
	// stale late datagram can't be primed. Idempotent.
	EndSession(gen uint32)
	// Stats returns current source stats for the playback heartbeat (D28/§9.1).
	Stats() contracts.SourceStats
}

// Subscriber is this node's member-side stream client (G internal/stream): it
// subscribes to a master's SOURCE_PORT via stream control (HELLO/keepalive/BYE/
// RESTART, §8.7) and delivers received frames to the local sink. H points it at
// the current master and generation; G owns the keepalive/RESTART/recovery
// internals and the actual sink.Push wiring (set up by main, K).
type Subscriber interface {
	// SubscribeTo points the client at master `src` (the master's SOURCE_PORT,
	// already resolved by H via DialCandidates) for session `gen`, with the
	// session transport. The client (re)HELLOs with prime-me, keepalives every
	// 5 s, RESTARTs on a >2 s frame gap, and BYEs the previous master. Idempotent
	// for an unchanged (src,gen) (no churn on repeated reconciles). The master's
	// own subscriber points at its loopback SOURCE_PORT — no special case (§8.2).
	SubscribeTo(src netip.AddrPort, gen uint32, transport string)
	// Unsubscribe BYEs the current master and goes idle (no active session).
	Unsubscribe()
}

// contracts.Sink (S) is the local playout sink. H calls only Reset(gen) on it —
// to arm it for the generation it just pointed the subscriber at. Push is G's
// (the subscriber delivers frames straight to the sink, wired by main). Stats()
// is read by the API for /api/status, not by H.

// contracts.Clock (S) — H stamps pts with LocalToMaster (D10) on the master, and
// calls SetMaster on the follower via the ClockControl seam below.

// ClockControl re-points the local clock follower at the current master clock
// endpoint + generation (§7/D17). Implemented by clock.Follower (F): SetMaster
// (dst netip.AddrPort, gen uint32). The follower discards samples and resyncs on
// any change; a no-op same-target call is cheap (F handles dedup).
type ClockControl interface {
	SetMaster(dst netip.AddrPort, gen uint32)
}

// FollowClient (contracts.FollowClient, S/D16) drives takeover (§5.2): POST
// /api/follow|/unfollow on peers. Concrete impl injected by the API piece (I).
```

`contracts.Clock` and `contracts.FollowClient` are consumed as-is from S. The
clock is **not** used to *schedule* release — the master releases by a wall-clock
20 ms ticker (§8.2) and uses `Clock.LocalToMaster` only to convert each frame's
local release instant into a master-clock `pts`. Playout-side clock translation
(`MasterToLocal`) is the sink's job (E).

### 2.2 `engine.go` — the one stateful type

```go
package group

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Params bundles everything H needs, injected by main (K).
type Params struct {
	Cluster Cluster                // C: read + owner setters + DialCandidates
	Media   MediaFactory           // D: scheme-keyed media-source factory (§6.1)
	Source  SourceServer           // G internal/source: master-side fan-out
	Sub     Subscriber             // G internal/stream: member-side subscribe client
	Sink    contracts.Sink         // E: local playout (H calls Reset only)
	Clock   contracts.Clock        // F: LocalToMaster for pts stamping (master)
	ClockCtl ClockControl          // F: SetMaster re-point (member, §7/D17)
	Follow  contracts.FollowClient // I: takeover HTTP fan-out (§5.2)
	Caps    contracts.Capabilities // this node's own caps (codec gating, §8.3)
	Log     *slog.Logger

	// Knobs (defaults applied in New when zero):
	Grace     time.Duration // self-heal grace, default 10 s (§5)
	LeadMs    int           // source release lead, default contracts.DefaultLeadMs (§8.2)
	Heartbeat time.Duration // playback position/SourceStats refresh, default 5 s (§9.2/D28)
}

// Engine is the group brain. One per node. One mutex guards all mutable fields.
type Engine struct {
	p   Params
	log *slog.Logger

	mu     sync.Mutex
	self   id.ID

	// playback (master-only)
	sess *session // current session, nil = idle
	gen  uint32   // monotonic per-node session generation (§8.4); never reused

	// reconcile / member-side tracking — what the local plumbing is pointed at
	curMaster id.ID  // master this node currently tracks (Zero = none/solo-idle)
	curGen    uint32 // generation the subscriber+sink+clock are armed for
	healAt    time.Time // when stale `following` becomes eligible for reset (zero=none)

	closed bool
	done   chan struct{}
	wg     sync.WaitGroup
}

// New builds an Engine and applies knob defaults. Starts no goroutines.
func New(p Params) *Engine

// Run launches the reconcile goroutine (one): it ranges Cluster.Subscribe() and
// a 1 s ticker, and on each wake re-points the local subscriber/sink/clock at
// the current master, runs self-heal, and tears down a session this node should
// no longer host. Returns when ctx is cancelled or Close is called.
func (e *Engine) Run(ctx context.Context)

// Close stops the reconcile goroutine, halts any running session (clearing local
// state without rewriting the replicated doc, §3.4), BYEs the subscriber, and
// returns. Idempotent.
func (e *Engine) Close() error

// --- API-facing methods (called by the I piece's HTTP handlers) ---

// Follow makes THIS node follow target (§5.1): validates target is alive and a
// master, then SetFollowing(target). Typed error on rejection.
func (e *Engine) Follow(target id.ID) error

// Unfollow makes THIS node a solo master: SetFollowing(Zero) (§5.1).
func (e *Engine) Unfollow() error

// MakeMaster orchestrates takeover so `node` becomes master of its current group
// (§5.2). MUST run on the current master (D17): returns ErrNotMaster otherwise
// so the API (I) can proxy to the master first. ctx bounds the HTTP fan-out.
func (e *Engine) MakeMaster(ctx context.Context, node id.ID) error

// Play starts playback of uri to this node's group (§6/§8.2). Master-only:
// returns ErrNotMaster (the API turns it into a 409 + takeover hint, §9.1) if
// this node is a follower. Opens the media source via the factory (scheme select
// §6.1), bumps the generation, starts the source session, writes playing status,
// and spawns the release loop. Replaces any running session first (§8.6).
// uri accepts file:/http(s):/input: and a bare path (≡ file:, §9.1 back-compat).
func (e *Engine) Play(uri string) error

// Stop stops the running session, broadcasts RECONFIG/stop, and clears playback
// status (§8.6). Master-only. No-op (nil) if nothing is playing.
func (e *Engine) Stop() error

// Settings returns the effective group settings for this node's group.
func (e *Engine) Settings() contracts.GroupSettings

// SetSettings validates + writes group settings (master-only, §9.1) and applies
// them LIVE (D23): bump generation, write the settings record, broadcast RECONFIG
// so subscribers re-read and resubscribe under the new gen. If a session is
// running the release loop re-stamps under the new gen mid-stream.
func (e *Engine) SetSettings(s contracts.GroupSettings) error

// Group returns this node's current derived group view (for /api/status §9.1).
func (e *Engine) Group() contracts.GroupView

// SourceStats returns the live source stats when this node runs a session, plus
// ok=false when idle (for /api/status §9.1/D19 "source present only while active").
func (e *Engine) SourceStats() (contracts.SourceStats, bool)
```

### 2.3 `mygroup.go` — locate + classify (NOT derive; D5)

```go
package group

import (
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// role classifies this node within its derived group.
type role int

const (
	roleSolo     role = iota // master of a group of 1 (following == Zero, alone)
	roleMaster               // master of a multi-member group
	roleFollower             // a valid follower of someone else
)

// myView is this node's resolved position in the pre-derived snapshot. C already
// filled Snapshot.Groups (D5); H only LOCATES this node's group + reads its own
// NodeView. No XOR, no membership computation here.
type myView struct {
	group    contracts.GroupView // the group containing self (always exactly one)
	self     contracts.NodeView  // this node's own record
	role     role
	master   id.ID               // group.Master
	stale    bool                // self.Following points at an invalid target (§5)
}

// myGroup finds this node's group in snap.Groups (the unique group whose Members
// contain self), reads self's NodeView, and classifies role + staleness.
//
// stale == true iff self.Following != Zero AND the snapshot's derivation did NOT
// place self as a follower of that target — i.e. C derived self into its OWN
// solo group despite a non-empty Following (target dead/unknown/itself-following,
// §5). H detects this by: self is master of a group that is {self} only, yet
// self.Following != Zero. That is exactly the self-heal trigger (§5).
func myGroup(snap contracts.Snapshot, self id.ID) myView
```

`myGroup` is pure and total: every alive node appears in exactly one derived
group (its own at minimum), so `group` is always found; if not (a transient
snapshot before C derived self), H treats it as solo-idle and skips reconcile for
that tick.

### 2.4 `session.go` / `play.go` — the master-side playback session

```go
package group

import (
	"sync"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// session runs one playback of one URI for one generation on the master. Created
// by Play, owned by the Engine (e.sess). Self-contained: one goroutine + ticker.
type session struct {
	gen     uint32
	uri     string
	settings contracts.GroupSettings
	groupID id.ID
	live    bool // pacing class (§6.1): false = pull (EOF ends), true = live (Stop only)

	src    MediaSource
	srv    SourceServer

	startMaster int64 // sessionStart in master-clock ns (LocalToMaster(now)+leadMs)
	startedUnix int64 // wall-clock unix for positionSec
	seq        uint64 // next frame sequence (read by status heartbeat under e.mu? no — see §3)

	stop chan struct{} // closed by halt()
	done chan struct{} // closed when run() exits (EOF or stop)
	once sync.Once      // guards stop close

	onEnd func(reason endReason) // engine callback: clear status, drop sess (EOF only)
}

type endReason int

const (
	endEOF  endReason = iota // natural end of a pull-paced source (§8.6)
	endStop                   // explicit Stop / replace / takeover / master loss
)

// run is the release loop. A 20 ms ticker releases one frame per tick:
//   - read a frame: src.ReadFrame(buf). On a live source's ErrUnderflow, publish
//     a silence frame instead (cadence never stalls, §6.1); on pull io.EOF, enter
//     drain (no more reads, keep ticking until lead+bufferMs has elapsed) then
//     end EOF; any other error ends the session with that error.
//   - stamp pts = startMaster + seq*stream.FrameNanos (§8.2). startMaster was set
//     from Clock.LocalToMaster(releaseNow)+leadMs at session start, so pts is in
//     master time without per-frame clock calls.
//   - srv.Publish(gen, seq, pts, payload); seq++.
// Exits on stop (endStop) or pull EOF-drain complete (endEOF); on exit it does
// NOT touch the SourceServer's session lifecycle — Stop()/onEnd handle Reconfig/
// EndSession (so the order is deterministic and idempotent).
func (s *session) run(leadMs, bufferMs int)

// halt closes stop once and waits for done. Called under no lock by the engine
// (the engine copies the *session pointer under e.mu, releases, then halt()s) so
// onEnd (which re-takes e.mu) cannot deadlock.
func (s *session) halt()
```

`Play` flow (under `e.mu` for the bookkeeping, releasing it across the blocking
`Media.Open` and `halt`):

1. `myGroup(snapshot, self)`; if `role == roleFollower` → `ErrNotMaster`.
2. Open the source: `src, err := p.Media.Open(uri)` (scheme select, §6.1). On
   error return it (no gen consumed, no status). `live := src.Live()`.
3. Validate `codec` against `Caps` (opus gating, §8.3) using the **current** group
   settings (`Cluster.GroupSettings(groupID)`); reject `ErrNoOpus` if needed —
   *before* consuming a generation.
4. Clock readiness: need `Clock.LocalToMaster(now)` ok to stamp `startMaster`.
   The master runs a follower against localhost and syncs within ~1 s; if not yet
   ok, retry-wait up to ~2 s; still not ok → close src, return a transient error
   (don't stamp garbage pts). (§7, tested with a fake clock toggling ok.)
5. Replace any running session: copy `e.sess`, release lock, `halt()` it, re-lock.
6. `e.gen++`; `gen := e.gen`. `srv.StartSession(gen, settings)`.
7. Build `session{...}`, `startMaster = LocalToMaster(now)+leadMs`,
   `startedUnix = time.Now().Unix()`. Install `e.sess = sess`.
8. Re-point THIS node's own plumbing at itself as master for `gen` (so the
   master hears its own stream like any member): the next reconcile does it, and
   `Play` also calls it inline for immediacy — see §3.2.
9. Write playing status: `SetPlayback(groupID, {State:"playing", URI:uri,
   StartedUnix, Codec, Transport, Source: srv.Stats()})`.
10. `go sess.run(leadMs, settings.BufferMs)`; the position/SourceStats heartbeat
    (status.go) refreshes the record every 5 s (D28).

### 2.5 `follow.go`, `takeover.go`, `settings.go`, `heal.go`, `status.go`

```go
package group

// follow.go
func (e *Engine) follow(target id.ID) error          // validate + SetFollowing(target)
func validateFollowTarget(snap contracts.Snapshot, self, target id.ID) error
//   reject ErrSelfFollow (target==self), ErrTargetUnknown (not in snapshot),
//   ErrTargetDead (!Alive), ErrTargetFollower (target.Following != Zero, §5.1).
//   Re-point (already following someone) is allowed — just overwrites.

// takeover.go — orchestration (§5.2, D17). Runs ON the master; I proxies first.
func (e *Engine) makeMaster(ctx context.Context, newMaster id.ID) error
//   1. classify self via myGroup; if role==roleFollower → ErrNotMaster (I proxies
//      to the current master, §5.2 step 1 — an I concern, H stays single-node).
//   2. newMaster must be a current member → else ErrTargetUnknown.
//   3. stop any running session (halt + RECONFIG/stop + clear status).
//   4. for each member except newMaster: Follow(member -> newMaster) over HTTP.
//      for newMaster: Unfollow(newMaster). If newMaster==self: call e.Unfollow()
//      locally (skip the HTTP self-call + re-proxy guard edge).
//   5. per-member HTTP errors are logged, not fatal (§5.2 "members that miss the
//      command self-heal"). Returns nil unless the snapshot is inconsistent.

// settings.go
func validateSettings(s contracts.GroupSettings, caps contracts.Capabilities) (contracts.GroupSettings, error)
//   codec ∈ {pcm, opus}; opus requires caps.Codecs to list it (§8.3) else ErrNoOpus.
//   transport ∈ {udp, tcp}; bufferMs clamped to [20, 2000]; 0 → DefaultBufferMs.
//   Unknown codec/transport → ErrBadSettings.
func (e *Engine) setSettings(s contracts.GroupSettings) error
//   master-only (ErrNotMaster off-master); validate; SetGroupSettings(groupID,v).
//   LIVE apply (D23): e.gen++; if a session is running, swap its settings + gen
//   (the run loop re-stamps gen on subsequent frames) and srv.StartSession(newGen)
//   to re-arm the ring; srv.Reconfig(newGen, stop=false) so subscribers re-read
//   settings + resubscribe; re-point local plumbing to (self,newGen).
//   If idle, only the record is written (next Play uses it).

// heal.go — called under e.mu from the reconcile loop after each myGroup.
func (e *Engine) reconcileHeal(mv myView, now time.Time)
//   If mv.stale: if healAt.IsZero() set healAt = now + Grace; if now >= healAt,
//   SetFollowing(Zero) + clear healAt. If !stale (valid follower or already solo):
//   clear healAt (cancel a pending heal — the master flapped back). (§5, 10 s.)

// status.go — playback record assembly + the periodic heartbeat (D28/§9.2).
func (e *Engine) playbackRecord(sess *session, st contracts.SourceStats) contracts.Playback
//   {State:"playing", URI, StartedUnix, PositionSec: elapsed since startedUnix,
//    Codec, Transport, Source: st}.  Idle → {State:"idle"} with zero fields.
func (e *Engine) heartbeat()
//   every Heartbeat (5 s) while a session runs: SetPlayback(groupID,
//   playbackRecord(sess, srv.Stats())) so positionSec advances and Playback.Source
//   refreshes in the cluster snapshot (UI reads it from there, no extra round-trip).
//   Runs on the reconcile goroutine's ticker branch (no extra goroutine).
```

### 2.6 Errors (engine.go)

```go
var (
	ErrNotMaster      = errors.New("group: not the group master (use takeover)") // §9.1 hint
	ErrTargetUnknown  = errors.New("group: follow/takeover target unknown")      // §5.1/§5.2
	ErrTargetDead     = errors.New("group: follow target not alive")             // §5.1
	ErrTargetFollower = errors.New("group: follow target is not a master")       // §5.1
	ErrSelfFollow     = errors.New("group: cannot follow self")                  // §5.1
	ErrNoOpus         = errors.New("group: opus codec not supported on this node") // §8.3
	ErrBadSettings    = errors.New("group: invalid group settings")             // §9.1
	ErrNotSynced      = errors.New("group: clock not synced yet, retry")        // §7 transient
	ErrClosed         = errors.New("group: engine closed")
)
```

`ErrNotMaster` is the typed signal the API (I) turns into a 409 + the "use
takeover" hint (§9.1, "Non-masters reject with a hint to use takeover").

---

## 3. Control flow, goroutines, locking

### 3.1 Goroutines

1. **reconcile goroutine** (one, started by `Run`): `select` over
   `Cluster.Subscribe()` and a 1 s `time.Ticker`. On each wake (and once
   immediately at start):
   - take `e.mu`, read a fresh `Snapshot`, `mv := myGroup(snap, self)`;
   - **re-point local plumbing** to the current master (§3.2);
   - `reconcileHeal(mv, now)` (self-heal, §5);
   - if a session is running but `mv.role != roleMaster && mv.role != roleSolo`
     (this node is no longer master), copy `e.sess`, clear it, release lock,
     `halt()` it with `endStop` (no status rewrite — H is no longer the writer of
     that group's record), re-lock;
   - the 1 s ticker branch also runs the **heartbeat** (D28) when a session is
     active and ≥5 s since the last `SetPlayback`.
   Exits on `ctx.Done()` / `e.done`.
2. **session run goroutine** (one per active session): the 20 ms release ticker
   (§2.4). On exit (EOF-drain done or `stop` closed) it `srv.Reconfig(gen, stop)`
   + `srv.EndSession(gen)` then `s.onEnd(reason)` and closes `done`. The engine's
   `onEnd` (EOF only) re-takes `e.mu`, and if `e.sess` is still this session,
   clears it and writes **idle** playback status. For `endStop` the caller (Stop/
   replace/teardown) already owns the status write, so `onEnd` is nil-checked.

Total: **2 goroutines at rest** (reconcile + its ticker share one via `select`),
**+1 while playing**. No goroutine per member — fan-out lives entirely in the
SourceServer (G); subscriber/keepalive lives entirely in the Subscriber (G).

### 3.2 Re-pointing local plumbing (the heart of "members follow automatically")

Every reconcile, H computes the **current master endpoint + generation** and, if
they changed since last time, re-arms the three local consumers. This is the only
place H dials anything (D22: master only):

```
master  := mv.master
gen     := the generation this node should track:
             - if THIS node is master and running a session: e.gen (its session)
             - else: the master's currently-advertised playback gen, derived from
               the master's Playback record in the snapshot if present, else a
               sentinel "follow whatever the source sends" handled by RESTART/
               RECONFIG. (H tracks curGen; the subscriber's prime-me + the
               source's RECONFIG converge it — H does not need to guess the exact
               gen, only WHO the master is and WHEN it changed.)
srcAddr := AddrPort(DialCandidates(master)[0], master.SourcePort)   // §3.1/D6
clkAddr := AddrPort(DialCandidates(master)[0], master.StreamPort)   // clock on STREAM_PORT

if (master,gen) != (e.curMaster,e.curGen):
    p.Sink.Reset(gen)                       // arm the sink for the new session (§8.5)
    p.Sub.SubscribeTo(srcAddr, gen, transport)  // HELLO prime-me to the new master
    p.ClockCtl.SetMaster(clkAddr, gen)      // re-point clock follower (§7/D17)
    e.curMaster, e.curGen = master, gen
```

When this node is **master**, `master == self` and `DialCandidates(self)` yields
loopback (or the master substitutes loopback for an unspecified addr, mirroring
F's `SetMaster` unspecified-addr rule): the master's own sink subscribes over
loopback exactly like any member (§8.2, "no special handling"). `Play` also calls
this re-point inline right after installing the session so the master starts
hearing itself without waiting for the next reconcile tick.

When the group is **solo-idle** (no playback), `gen` is the last armed gen and the
subscriber simply HELLO-keepalives the (self) source which publishes nothing —
or, simplest and what H does: on idle, `Sub.Unsubscribe()` and leave the sink
reset; the subscriber re-subscribes the instant a session starts (next reconcile
after the master writes a playing record). Either way no audio flows when idle.

H does **not** guess the exact generation a remote master is on: it tracks the
master *identity* and re-subscribes (prime-me) on any change; the source answers
HELLO by burst-priming the current ring and stamps every frame with the live gen,
and the sink's gen-gate (`Reset(gen)` + `Header.Gen`) plus the receiver's
stale-gen drop (G) discard anything older. RECONFIG converges a mid-session
settings change. So "which gen to track" is self-correcting; H only needs to
re-HELLO on master change and on local RECONFIG.

### 3.3 Locking

- **One mutex** `e.mu` guards every mutable Engine field (`sess`, `gen`,
  `curMaster`, `curGen`, `healAt`, `closed`, `self`). Per S's convention.
- The **session has no mutex**: its cross-goroutine state is the `stop`/`done`
  channels and a `sync.Once`. The run goroutine exclusively owns `seq`, `src`,
  and the `srv.Publish` calls. The heartbeat reads `startedUnix` (immutable after
  Play) and `srv.Stats()` (SourceServer's own concern) — it never reads `seq`
  across the goroutine boundary, so there is no race (positionSec is wall-clock
  elapsed, not seq-derived).
- `halt()` must **not** be called while holding `e.mu` (it waits on `done`, and
  `onEnd` re-takes `e.mu` → deadlock). Pattern everywhere: copy the `*session`
  under the lock, release, `halt()`, re-lock to install/clear.
- Cluster setters (`SetFollowing`/`SetPlayback`/`SetGroupSettings`) may be called
  under `e.mu` (no cycle: C never calls synchronously back into H). `FollowClient`
  HTTP calls in `makeMaster` are done **without** `e.mu` held (they block on the
  network). `Sub.SubscribeTo`/`ClockCtl.SetMaster`/`Sink.Reset` are non-blocking
  (they just update target state / arm a buffer) and are called under `e.mu`.

### 3.4 Startup / shutdown

`New(params)` → `Run(ctx)`. The reconcile goroutine does an initial
`myGroup`+re-point+heal immediately, so a node booting already following a dead
master heals after its grace window and a member immediately points at its
master. No session at start.

`Close()`: set `closed`, signal `done`, `wg.Wait()`. If a session is running,
copy+`halt()` it (outside the lock); `Sub.Unsubscribe()`. Does **not** rewrite the
replicated doc on close (a dying master's playback record is left as-is;
subscribers' 2 s watchdog → RESTART → give-up + self-heal (§8.6) stop their
playout, and the 30-day purge / next master cleans the record). Idempotent via
`closed`.

---

## 4. Edge cases & failure handling

- **Play on a follower (§9.1)**: `Play`/`SetSettings`/`Stop`/`MakeMaster`-on-
  non-master return `ErrNotMaster`; I → 409 + takeover hint. The UI's "play from a
  follower" is takeover + play as two API calls (§5.2), not one H call.
- **Generation management (§8.4/§8.6/§8.7)**: a monotonic `uint32` `e.gen`,
  incremented on every `Play` and every live `SetSettings` (D23). `MakeMaster`
  and `Stop` end the session and broadcast RECONFIG/stop but do **not** need a new
  gen (the *next* Play bumps it). The gen is engine-lived (survives sessions) so
  it never reuses a value within the node's lifetime; receivers drop stale gens.
- **Natural EOF — pull sources (§6.1/§8.6)**: `src.Live()==false`; `ReadFrame` →
  `io.EOF`. The session does **not** cut instantly: it has released frames up to
  `leadMs+bufferMs` ahead of playout, so it keeps ticking with silence-free drain
  (no further reads/publishes) until that tail elapses, then `Reconfig(gen,stop)`
  + `EndSession` + `onEnd(endEOF)` → engine writes idle status. Prevents clipping
  the last ~200 ms.
- **Live sources never EOF (§6.1)**: `src.Live()==true` (http/input). `ReadFrame`
  returns `ErrUnderflow` on a momentary stall → the run loop publishes a **silence
  frame** for that tick (seq/pts cadence never stalls). The session ends **only**
  on `Stop`/takeover/master-loss, never on EOF. (Contract concern §6: D's underflow
  signal.)
- **Explicit Stop / replace (§8.6)**: `Stop` (or a new `Play`) `halt()`s the run
  loop, which stops publishing immediately, then `Reconfig(gen,stop)` +
  `EndSession`. `Stop` writes idle status; `Play` lets the new session's playing
  status overwrite it. Idempotent: double Stop / Stop-after-EOF → a single
  Reconfig/stop (guarded by `sync.Once` on the session and the `e.sess` identity
  check).
- **Live settings change mid-session (§8.7/D23)**: `SetSettings` bumps gen,
  writes the settings record, re-arms the source ring (`StartSession(newGen)`),
  broadcasts `Reconfig(newGen,stop=false)`, and re-points the local plumbing to
  `(self,newGen)`. The run loop reads `s.gen` (updated under `e.mu`) and stamps
  subsequent frames with the new gen; subscribers re-read the replicated settings
  and resubscribe. Settings therefore apply **live**, not at next play.
- **Follow validation (§5.1)**: reject self-follow, unknown, dead, and a target
  that is itself following someone (not a master). Re-point (already following A,
  follow B) is allowed and overwrites.
- **Self-heal (§5, 10 s)**: in `reconcileHeal`. Armed the first time `mv.stale`,
  fires after `Grace` even absent further cluster events (1 s ticker). Cancelled
  if the target recovers within the window. A node following an invalid target
  already behaves as solo for *derivation* (C, D5) immediately; only the
  write-back reset of its own `following` waits 10 s.
- **Takeover missed members (§5.2)**: per-member HTTP errors logged + ignored;
  the member self-heals to solo or follows late. `makeMaster` returns nil unless
  `newMaster` is not a current member (`ErrTargetUnknown`).
- **Takeover off-master (§5.2/D17)**: `MakeMaster` on a follower → `ErrNotMaster`;
  I proxies to the current master first (the proxy hop is an I concern). H stays
  single-node-reasoning. `newMaster==self` skips the HTTP self-call (local
  `Unfollow`).
- **Master change re-points cleanly (§7/D17)**: when the snapshot shows a new
  master (takeover, or a dead master self-healed away), the next reconcile
  computes the new master endpoint and calls `Sub.SubscribeTo(newSrc,…)`,
  `ClockCtl.SetMaster(newClk,…)`, `Sink.Reset(gen)`. The subscriber BYEs the old
  master and HELLO-primes the new one; the follower discards clock samples and
  resyncs (F). No audio-side coordination beyond these three local calls.
- **Opus without capability (§8.3)**: `validateSettings`/`Play` reject
  `codec:opus` when `Caps.Codecs` lacks `"opus"` → `ErrNoOpus`. Default build
  never lists opus.
- **Unsynced master clock (§7)**: `Play` needs `Clock.LocalToMaster(now).ok` to
  stamp `startMaster`. The master runs a follower against localhost (synced ~1 s).
  `Play` retry-waits up to ~2 s; still unsynced → `ErrNotSynced` (transient), no
  gen consumed, source closed. (Tested with a fake clock toggling ok.)
- **Media open failure (§6/§6.1)**: bad path/traversal (file:), unreachable host
  (http:), missing capture tool (input:), or unsupported scheme → the factory
  returns an error; `Play` returns it for I to surface; no session, no status, gen
  NOT consumed.
- **SourceServer is per-node, not per-session**: its listeners/registry run for
  the node's whole life (any node can be master, §8.7/D22). `StartSession`/
  `EndSession` only arm/disarm a generation. If `StartSession` somehow fails
  (it does not return an error in the seam — it's pure local state), Play proceeds;
  a genuinely broken source path surfaces via no subscribers, not via Play.
- **Membership change mid-session**: H does **not** re-fan-out (D22 removed
  endpoints). A member that *joins* late simply subscribes (HELLO prime-me) and is
  burst-primed by the source ring (§8.2/D24); a member that *leaves* BYEs or
  expires (15 s) in the source registry. The master's session is untouched by
  membership churn — no `SetEndpoints` exists.
- **Close while playing**: `halt()` the session + `Sub.Unsubscribe()`, no status
  rewrite (§3.4).
- **Concurrent Play/Stop/SetSettings/MakeMaster**: serialized by `e.mu`; each
  copies the current `*session` under the lock, halts outside it, then re-locks to
  install/clear. The reconcile goroutine takes the same lock, so a master-loss
  teardown can't race a `Play`.

---

## 5. Test plan

All tests use: a `fakeCluster` (in-memory `Snapshot` with **pre-derived**
`Groups` per D5, settable records, a deterministic `Subscribe` channel, recorded
`SetFollowing`/`SetPlayback`/`SetGroupSettings` calls, a `DialCandidates` stub);
a `fakeFollowClient` (records calls, optional per-peer error); a `fakeMedia`
factory + `fakeSource` (N canned frames then `io.EOF` for pull, or `ErrUnderflow`
on demand for live); a `fakeSource` **server** (records `StartSession`/`Publish`/
`Reconfig`/`EndSession`, returns settable `Stats()`); a `fakeSubscriber`
(records `SubscribeTo`/`Unsubscribe`); a `fakeSink` (records `Reset(gen)`); a
`fakeClock` (settable `LocalToMaster` + ok); a `fakeClockCtl` (records
`SetMaster`); and a controllable `now func() time.Time` + tick channel for heal
and heartbeat timing. **No sockets, no audio, no root.**

`mygroup_test.go`
- `TestMyGroupSolo` — node following Zero, alone → roleSolo, master==self, !stale.
- `TestMyGroupMaster` — node + valid followers in its pre-derived group → roleMaster.
- `TestMyGroupFollower` — node is a member of someone else's group → roleFollower.
- `TestMyGroupStaleFollowing` — Following != Zero but derived into own solo group →
  stale==true (self-heal trigger).
- `TestMyGroupNotYetDerived` — self absent from Groups (transient) → solo-idle,
  reconcile skipped, no panic.

`follow_test.go`
- `TestFollowAliveMaster` — SetFollowing(target) recorded.
- `TestFollowRejectsSelf / Unknown / Dead / Follower` — typed errors, no setter.
- `TestFollowRepoint` — following A, follow B → SetFollowing(B).
- `TestUnfollow` — SetFollowing(Zero).

`heal_test.go`
- `TestHealNoResetBeforeGrace` — stale, now<healAt → no SetFollowing.
- `TestHealResetsAfterGrace` — now>=healAt → SetFollowing(Zero), healAt cleared.
- `TestHealCancelsWhenTargetRecovers` — becomes valid → healAt cleared, no reset.
- `TestHealFiresWithoutClusterEvents` — 1 s ticker drives the reset on time.

`takeover_test.go`
- `TestMakeMasterFanout` — 3 members, newMaster=follower B → Follow(A→B),
  Follow(C→B), Unfollow(B); running session halted first.
- `TestMakeMasterSelfUsesLocalUnfollow` — newMaster==self → local Unfollow, no HTTP.
- `TestMakeMasterToleratesMemberError` — one Follow errors → still nil.
- `TestMakeMasterOnNonMasterRejected` — called on a follower → ErrNotMaster.
- `TestMakeMasterUnknownNode` — newMaster not a member → ErrTargetUnknown.

`play_test.go`
- `TestPlayRejectsFollower` — ErrNotMaster.
- `TestPlayRejectsBadURI` — factory error propagated; no status; gen NOT consumed.
- `TestPlayRejectsOpusWithoutCap` — ErrNoOpus before gen bump.
- `TestPlayWritesPlayingStatus` — SetPlayback(state=playing, uri, codec, transport,
  Source from srv.Stats()).
- `TestPlayBumpsGeneration` — second Play uses gen+1; srv.StartSession(gen+1).
- `TestPlayReplacesRunningSession` — old session halted before new one starts.
- `TestPlayWaitsForClockSync` — LocalToMaster ok=false→true → succeeds; stays
  false → ErrNotSynced, no gen, source closed.
- `TestPlayBarePathIsFileScheme` — "song.wav" opens factory with file: semantics
  (factory's concern; H passes the raw uri through).

`session_test.go`
- `TestSessionReleasesInOrder` — srv.Publish sees seq 0..N-1, pts monotonic step
  FrameNanos, first pts == startMaster.
- `TestSessionStartFromLocalToMaster` — startMaster == LocalToMaster(now)+LeadMs.
- `TestSessionPullEOFDrainsThenEnds` — pull source: after io.EOF, ticker runs
  ~lead+bufferMs more with no publishes, then Reconfig(gen,stop)+EndSession+
  onEnd(endEOF) once; idle status written.
- `TestSessionLiveUnderflowPublishesSilence` — live source returns ErrUnderflow on
  a tick → a silence frame is published (cadence unbroken), session continues.
- `TestSessionLiveNeverEOF` — live source never ends on its own; only Stop ends it.
- `TestSessionStopHaltsImmediately` — Stop → no further Publish, Reconfig(stop)
  once, onEnd(endStop), idle status.
- `TestSessionStopIdempotent` — double Stop / Stop-after-EOF → single Reconfig/stop.

`settings_test.go`
- `TestSettingsDefaults` — unset group → pcm/udp/150.
- `TestSetSettingsMasterWritesAndReconfigs` — master → SetGroupSettings + gen bump
  + srv.StartSession(newGen) + srv.Reconfig(newGen,false) + re-point to (self,newGen).
- `TestSetSettingsRejectsFollower` — ErrNotMaster.
- `TestSetSettingsValidates` — bad codec/transport → ErrBadSettings; opus no-cap →
  ErrNoOpus.
- `TestSetSettingsClampsBuffer` — out-of-range clamped; 0 → 150.
- `TestSetSettingsLiveMidSession` — running session: new gen applied to subsequent
  Publish frames; subscriber re-pointed.

`watch_test.go`
- `TestRepointSubscriberOnMasterChange` — snapshot master A→B → Sub.SubscribeTo(B
  src addr, gen), ClockCtl.SetMaster(B clk addr, gen), Sink.Reset(gen) once;
  unchanged master → no repeat calls (idempotent).
- `TestRepointUsesDialCandidates` — srcAddr/clkAddr built from
  DialCandidates(master)[0] + master's SourcePort/StreamPort.
- `TestMasterSubscribesToSelfLoopback` — this node master → re-points at its own
  (loopback) source; master hears its own stream.
- `TestWatchTearsDownSessionOnMasterLoss` — this node loses mastership → session
  halted (endStop), no status rewrite.
- `TestReconcileSkipsBeforeSelfDerived` — self not yet in Groups → no re-point, no
  panic.

`status_test.go`
- `TestHeartbeatRefreshesPositionAndSource` — after Heartbeat interval, SetPlayback
  called with advanced positionSec and Playback.Source == srv.Stats() (D28).
- `TestHeartbeatStopsWhenIdle` — no session → no heartbeat writes.

`engine_test.go`
- `TestRunReconcilesOnClusterChange` — Subscribe signal → Group() updates + repoint.
- `TestRunHealsOnBoot` — boot following a dead node → resets after grace.
- `TestCloseHaltsSessionAndUnsubscribes` — Close while playing halts the session,
  BYEs the subscriber, exits clean.
- `TestCloseIdempotent` — double Close → nil, no panic, no goroutine leak (-race).

All `*_test.go` run with `-race`, no network, no root, no hardware. Time-driven
tests inject a controllable `now` + tick channel so there are no real sleeps
beyond microsecond waits on `done`.

---

## 6. Contract concerns (for the integrator)

D, G, and K are all regenerated to the subscribe model (D22–D28); the seams H
declares (`MediaFactory`/`MediaSource`, `SourceServer`, `Subscriber`) match the
*shape* those pieces now ship. The residual items below are naming/signature
mismatches and one genuinely-unpinned signal, to reconcile before H compiles
against the real pieces:

1. **D's media-factory entry point & source method set.** D-audio.md exports
   `audio.Open(ctx, uri, mediaDir) (audio.Source, error)` (+ `audio.Schemes()`),
   with `Source.ReadFrame(dst) error` / `Live() bool` / `Close() error` (D9 EOF,
   D26 schemes). H's seam is `MediaFactory.Open(uri) (MediaSource, error)` with
   `MediaSource.{ReadFrame,Live,Close}`. The shape matches; the only gap is the
   `Open` signature (D takes `ctx` + `mediaDir`; H's seam takes just `uri`).
   **Ask:** wrap D's `Open` in a small `mediaDir`/`ctx`-bound factory (K or H
   adapter) so H's `MediaFactory.Open(uri)` lines up — a one-line closure, no
   redesign.

2. **Live-paced underflow signal is unspecified — REAL MISMATCH.** §6.1 says live
   sources emit silence on underflow; H's `MediaSource` doc assumes a sentinel
   `audio.ErrUnderflow` returned from `ReadFrame`. **D-audio.md does NOT export
   `ErrUnderflow`**: its http/input sources emit silence *internally* and return
   `nil` on underflow (D §3.2/§3.3, §4 "Live underflow"), so H never sees a
   transient signal. **Ask:** either H drops its `ErrUnderflow` assumption and
   treats live `ReadFrame` as "always nil, silence already substituted" (matches
   D as written — preferred, simplest), OR D adds the sentinel. As things stand H
   must not key off a non-existent `audio.ErrUnderflow`. (Resolution affects only
   the `session.run` underflow branch, §2.4/§4.)

3. **Generation a remote member should track.** H points its subscriber at the
   master with a generation, but a follower does not author the master's gen.
   H's design (§3.2) tracks master *identity* and lets HELLO-prime + RECONFIG +
   the sink/receiver gen-gate converge the actual gen, so H never has to read the
   master's exact gen out of the snapshot. **Ask:** confirm G's subscriber accepts
   "subscribe to this master, prime me, follow whatever gen it stamps" (i.e. the
   `gen` arg to `SubscribeTo` is the *minimum* gen to accept / the local sink-arm
   gen, not a hard filter that drops a master already on gen+1). If G needs the
   exact gen, H must read it from `Playback` in the snapshot — a small addition.

6. **Clock endpoint port (§7).** H points the clock follower at the master's
   **STREAM_PORT** (clock rides STREAM_PORT UDP, §7), while the subscriber points
   at SOURCE_PORT (§8.7). Both come from `DialCandidates(master)[0]` with the
   respective port from the master's `NodeView`. This is consistent with F's
   `SetMaster(dst, gen)`. No change requested — flagged so K wires the right port
   into each seam.
