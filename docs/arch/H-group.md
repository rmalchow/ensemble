# H — group engine

Source of truth: [docs/README.md](../README.md) §5, §5.1, §5.2, §7, §8.2–§8.6,
§9.1. Shared contracts: [docs/arch/S-skeleton.md](S-skeleton.md) — `id`,
`contracts`, `stream`. This piece owns `internal/group/*` only.

H is the brain that turns the replicated cluster doc + liveness into **derived
groups** (§5), enforces follow/unfollow/takeover transitions, and runs the
**playback session** on the master: decode → real-time ticker release → fan-out
to every member (incl. self) → write playback status + group settings into the
replicated doc.

Design stance (per ground rules): smallest thing that satisfies the spec. No
interfaces invented in H beyond the ones S already pins. One concrete `Engine`
type; one concrete `session` type; one mutex on the engine. Everything is
testable with a fake cluster, a fake follow-client, a fake source, a fake
sender, and a fake clock — no audio hardware, no sockets.

---

## 1. Package / file layout

Files H creates and owns (`internal/group/`):

```
engine.go         Engine type: construction, deps, one mutex, lifecycle (Start/Close).
derive.go         Pure group derivation from a Snapshot (§5): groups, XOR id, self-heal detection.
follow.go         Follow / Unfollow validation + apply via cluster setters (§5.1).
takeover.go       Master takeover orchestration (§5.2) over the injected FollowClient.
heal.go           10 s self-heal grace timer: reset own `following` when target invalid (§5).
play.go           Play / Stop entry points: validate, build session, write playback status (§8.6).
session.go        Playback session: source -> ticker release -> sender fan-out; gen bump; EOF/stop.
settings.go       Group-settings get/set, validation + defaults, write to cluster (§8.3/§8.4/§9.1).
deps.go           Dependency interfaces H consumes (Cluster setter view, Source, Sender, factories).
doc.go            Package doc + slog component name.

engine_test.go    Lifecycle, Subscribe re-derive on cluster change, Close idempotency.
derive_test.go    Derivation truth table: solo, follow, dead/unknown/chained target, XOR id stability.
follow_test.go    Follow validation (alive master only), unfollow, re-point; setter calls asserted.
takeover_test.go  Takeover happy path + missed-member tolerance; forwards to current master.
heal_test.go      Grace timer: no reset before 10 s, reset after; cancel when target recovers.
play_test.go      Play rejects non-master / missing file / opus-without-cap; status written.
session_test.go   Ticker release order, gen bump, fan-out to all members incl self, EOF + Stop.
settings_test.go  Defaults, validation, master-only write, LWW via cluster setter.
```

No file here is larger than ~250 lines; `session.go` is the densest.

---

## 2. Concrete Go API

### 2.1 `deps.go` — what H consumes (and who provides it)

H depends on cluster (C), audio (D), stream sender (G), clock (F), and the API
follow-client (I). S pins `contracts.{Snapshot, GroupView, GroupSettings,
Playback, Sink, Clock, FollowClient}` and `id`. The remaining surfaces — the
cluster **write** side, the audio **source**, and the stream **sender** — are
not in S. H defines the minimal interfaces it needs locally (Go style: define
interfaces where consumed), so H stays compilable and testable in isolation and
the providers satisfy them structurally.

```go
package group

import (
	"context"
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Cluster is the slice of the cluster piece (C) that H needs: the read side
// (Snapshot/Subscribe/Self) plus the owner-only setters that write THIS node's
// record and the per-group LWW records. Implemented by *cluster.State.
//
// All setters write only fields this node is allowed to own (§4): SetFollowing
// mutates this node's own record; SetPlayback / SetGroupSettings / SetGroupName
// write group-keyed records and are only ever CALLED by H when this node is the
// group master (H enforces that, not C).
type Cluster interface {
	contracts.StateStore // Self() id.ID; Snapshot() contracts.Snapshot; Subscribe() <-chan struct{}

	// SetFollowing sets this node's own `following` field (Zero == solo) and
	// bumps its record version + broadcasts (§5.1). target==Zero is unfollow.
	SetFollowing(target id.ID)

	// SetPlayback writes the playback-status record for groupID (§4, master-only).
	// Passing state "idle" with empty file clears it (§8.6).
	SetPlayback(groupID id.ID, p contracts.Playback)

	// SetGroupSettings writes the per-group settings record (§8.3/§8.4), LWW.
	SetGroupSettings(groupID id.ID, s contracts.GroupSettings)

	// GroupSettings returns the stored settings for groupID, or defaults if none.
	GroupSettings(groupID id.ID) contracts.GroupSettings

	// SetGroupName writes the group-name record (§4). Used by the API name route,
	// re-exported through H only for convenience; H itself never renames.
	SetGroupName(groupID id.ID, name string)
}

// Source is one decoded media file as a stream of canonical 20 ms PCM frames
// (§8.1/§8.2). Implemented by the audio piece (D). Open is done by a SourceOpener
// so H can be tested with a fake.
type Source interface {
	// ReadFrame returns the next canonical PCM frame (exactly stream.FrameBytes).
	// Returns io.EOF after the last frame (natural end, §8.6). The returned slice
	// is valid until the next ReadFrame; the session copies before handing off.
	ReadFrame() ([]byte, error)
	// Close releases the decoder/file.
	Close() error
}

// SourceOpener opens a local media file (relative to MEDIA_DIR; traversal is
// rejected by the opener, §6) into a Source. Injected so play_test uses a fake.
type SourceOpener interface {
	Open(relPath string) (Source, error)
}

// Sender streams one session's frames to a fixed set of member endpoints,
// including this node itself via localhost (§8.2 "one code path, no special
// cases"). Implemented by the stream piece (G). A Sender is created per session
// by SenderFactory with the session's gen, codec and transport already chosen.
type Sender interface {
	// Send transmits one audio frame (header gen/seq/pts + payload) to all members.
	// Non-blocking best-effort for UDP; blocking-with-timeout for TCP. Errors are
	// logged+counted by the sender, not fatal to the session.
	Send(seq uint64, pts int64, payload []byte)
	// Stop sends the stop/end control for this generation (§8.6) and tears down
	// connections. Idempotent.
	Stop()
}

// Endpoint is one stream destination resolved by H from the snapshot (§3.1
// address choice is the cluster's job; H passes member IDs and the cluster
// resolves the dial address inside SenderFactory).
type Endpoint struct {
	Node       id.ID
	StreamAddr string // "host:port" already resolved (host chosen per §3.1)
}

// SenderFactory builds a Sender bound to a generation + the member endpoint set
// + transport/codec. Injected by main (K); real impl lives in G, fake in tests.
type SenderFactory interface {
	New(gen uint32, members []Endpoint, s contracts.GroupSettings) (Sender, error)
}

// Resolver turns member IDs into stream Endpoints using the cluster's §3.1
// address selection. Provided by C/main; H never dials directly.
type Resolver interface {
	StreamEndpoints(members []id.ID) []Endpoint
}
```

`contracts.FollowClient` (S) and `contracts.Clock` (S) are consumed as-is.
The clock is **not** used by H directly for scheduling release — the master
releases by wall-clock ticker (§8.2) and only needs `MasterNow()` to stamp
`sessionStart`. Playout-side clock translation is the sink's job (E).

### 2.2 `engine.go` — the one stateful type

```go
package group

import (
	"context"
	"log/slog"
	"sync"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Deps bundles everything H needs, injected by main (K).
type Deps struct {
	Cluster   Cluster
	Follow    contracts.FollowClient // POST /api/follow|/unfollow on peers (§5.2)
	Opener    SourceOpener           // open local media (D)
	Senders   SenderFactory          // build per-session sender (G)
	Resolve   Resolver               // member IDs -> stream endpoints (§3.1, C)
	Clock     contracts.Clock        // master-now for sessionStart stamping (F)
	Caps      contracts.Capabilities // this node's own caps (codec gating, §8.3)
	Log       *slog.Logger
	// Knobs (defaults applied in New if zero):
	GraceWindow time.Duration // self-heal grace, default 10 s (§5)
	LeadMs      int            // source release lead, default contracts.DefaultLeadMs (§8.2)
}

// Engine is the group brain. One per node. One mutex guards all mutable fields.
type Engine struct {
	d   Deps
	log *slog.Logger

	mu       sync.Mutex
	self     id.ID
	sess     *session            // current playback session (nil = idle), master-only
	healAt   time.Time           // when a stale `following` becomes eligible for reset (zero = none)
	closed   bool
	done     chan struct{}
	wg       sync.WaitGroup
	subClose chan struct{}       // signals the cluster-watch goroutine to exit
}

// New builds an Engine. Does not start goroutines; call Start.
func New(d Deps) *Engine

// Start launches the cluster-watch goroutine: on every cluster change it
// re-derives groups, arms/cancels the self-heal timer, and tears down a session
// whose group membership/mastership no longer makes this node master (§5/§5.2).
func (e *Engine) Start()

// Close stops the watch goroutine, stops any running session (without bumping
// playback into "stop" semantics beyond clearing local state), and returns.
// Idempotent.
func (e *Engine) Close() error

// --- API-facing methods (called by the I piece's HTTP handlers) ---

// Follow makes THIS node follow target (§5.1). Validates target is alive and a
// master. Returns a typed error on rejection.
func (e *Engine) Follow(target id.ID) error

// Unfollow makes THIS node a solo master (§5.1).
func (e *Engine) Unfollow() error

// MakeMaster orchestrates takeover so that `node` becomes master of its current
// group (§5.2). May be called on any group member; forwards to the current
// master if this node isn't it. ctx bounds the HTTP fan-out.
func (e *Engine) MakeMaster(ctx context.Context, node id.ID) error

// Play starts playback of relPath to this node's group (§8.2). Master-only:
// returns ErrNotMaster (with a takeover hint) if this node is a follower.
// Bumps generation, opens the source, writes playback status, starts the
// session. If a session is already running it is stopped first (§8.6).
func (e *Engine) Play(relPath string) error

// Stop stops the running session and clears playback status (§8.6). Master-only.
// No-op (nil) if nothing is playing.
func (e *Engine) Stop() error

// Settings returns the effective group settings for this node's group.
func (e *Engine) Settings() contracts.GroupSettings

// SetSettings validates + writes group settings (master-only) (§9.1).
func (e *Engine) SetSettings(s contracts.GroupSettings) error

// Group returns this node's current derived group (for /api/status).
func (e *Engine) Group() contracts.GroupView
```

### 2.3 `derive.go` — pure derivation (§5)

No engine state; takes a snapshot, returns groups. Used by the engine and by C/I
(but H owns the function; I reads groups out of the `Snapshot` that C already
filled — so in practice **C calls Derive** to populate `Snapshot.Groups`).
H re-exports it for its own use and tests.

```go
package group

import (
	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Derive computes the derived groups (§5) from a resolved snapshot. Inputs are
// the node views (with Alive + Following). Output groups are sorted by master ID
// for stable rendering. Pure: no I/O, no clock, deterministic.
//
// Rules (§5):
//   - A node M is a MASTER iff M.Alive && M.Following.IsZero().
//   - A node F is a VALID FOLLOWER of M iff F.Alive && F.Following==M.ID &&
//     M is a master (alive, not following). Otherwise F is treated as solo.
//   - group(M) = {M} ∪ {valid followers of M}; master=M; id = XOR(member ids).
//   - A solo/orphaned node forms its own one-member group (id == its node id).
// Group name + playback + settings are looked up from the snapshot's per-group
// records by the derived group ID and attached to each GroupView.
func Derive(s contracts.Snapshot) []contracts.GroupView

// memberSetID is XOR of member ids (§5). Exposed for tests.
func memberSetID(members []id.ID) id.ID // == id.XOR(members...)

// staleFollowing reports, for nodeID in snapshot s, whether its `following`
// points at an invalid target (dead, unknown, or itself-following), i.e. the
// node should self-heal to solo (§5). false for solo nodes and valid followers.
func staleFollowing(s contracts.Snapshot, self id.ID) (stale bool)
```

`Derive` is what C invokes to fill `Snapshot.Groups`; therefore the
`GroupView.Name/Playback/Settings` attachment needs the per-group records. Since
`contracts.Snapshot` (S) only exposes `Nodes` and the already-filled `Groups`,
**C performs the name/playback/settings join when it builds the snapshot.** H's
`Derive` is the membership-and-id algorithm; the join is trivial map lookups C
already has. See Contract concerns.

### 2.4 `play.go` / `session.go` — the playback session

```go
package group

import (
	"io"
	"sync"
	"time"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
	"ensemble/internal/stream"
)

// session runs one playback of one file for one generation. Created by Play,
// owned by the Engine (engine.sess). Self-contained goroutine + ticker.
type session struct {
	gen      uint32
	file     string
	codec    string
	transport string
	groupID  id.ID

	src    Source
	sender Sender

	startMaster int64 // sessionStart in master-clock ns (= MasterNow()+leadMs)
	startedUnix int64 // wall-clock unix for playback status positionSec

	stop chan struct{} // closed by Stop()
	done chan struct{} // closed when the run goroutine exits (EOF or stop)
	once sync.Once      // guards stop close

	onEnd func(reason endReason) // engine callback: clear status, drop sess
}

type endReason int

const (
	endEOF  endReason = iota // natural end of file (§8.6)
	endStop                   // explicit Stop / new Play / takeover
)

// run is the release loop: a 20 ms ticker releases frames in real time,
// stamping pts = startMaster + seq*FrameNanos (§8.2). Sends each to the sender.
// On io.EOF it lets the buffered audio drain (lead+buffer worth) then ends EOF.
func (s *session) run(leadMs, bufferMs int)

// halt closes stop once and waits for done (used by Stop / Play-replace / Close).
func (s *session) halt()
```

### 2.5 `follow.go`, `takeover.go`, `settings.go`, `heal.go`

```go
package group

// follow.go
func (e *Engine) follow(target id.ID) error    // validate + SetFollowing(target)
func (e *Engine) validateFollowTarget(s contracts.Snapshot, target id.ID) error

// takeover.go — orchestration (§5.2)
//  1. if this node isn't current master, forward to master via FollowClient?
//     NO: forwarding is an HTTP proxy concern (I). H's MakeMaster assumes it
//     runs ON the current master (I proxies the request there first, §5.2.1).
//     If called on a non-master, H returns ErrNotMaster so I can proxy/retry.
//  2. stop any running session (Stop()).
//  3. for each member except the new master: FollowClient.Follow(member, newMaster).
//     for the new master: FollowClient.Unfollow(newMaster).
//     (If newMaster == self, call own Unfollow() locally instead of HTTP.)
//  4. best-effort: errors per member are logged, not fatal (§5.2 "members that
//     miss the command self-heal"). Returns nil unless the snapshot is invalid.
func (e *Engine) makeMaster(ctx context.Context, newMaster id.ID) error

// settings.go
func validateSettings(s contracts.GroupSettings, caps contracts.Capabilities) (contracts.GroupSettings, error)
//   codec ∈ {pcm, opus}; opus requires caps to list it (§8.3) else ErrNoOpus.
//   transport ∈ {udp, tcp}; bufferMs clamped to [20, 2000], default 150 if 0.

// heal.go — called under e.mu from the watch loop after each re-derive
func (e *Engine) reconcileHeal(s contracts.Snapshot, now time.Time)
//   If self has stale `following`: if healAt==zero set healAt=now+grace; if
//   now>=healAt call SetFollowing(Zero) and clear healAt. If following is valid
//   or already solo: clear healAt (cancel pending heal). (§5 10 s grace.)
```

### 2.6 Errors (engine.go)

```go
var (
	ErrNotMaster     = errors.New("group: not the group master (use takeover)") // §9.1 play hint
	ErrTargetUnknown = errors.New("group: follow target unknown")               // §5.1
	ErrTargetDead    = errors.New("group: follow target not alive")             // §5.1
	ErrTargetFollower= errors.New("group: follow target is not a master")       // §5.1
	ErrSelfFollow    = errors.New("group: cannot follow self")                  // §5.1
	ErrNoOpus        = errors.New("group: opus codec not supported on this node") // §8.3
	ErrBadSettings   = errors.New("group: invalid group settings")             // §9.1
	ErrClosed        = errors.New("group: engine closed")
)
```

`ErrNotMaster` is the typed signal the API (I) turns into a 409 + the
"use takeover" hint (§9.1, "Non-masters reject with a hint to use takeover").

---

## 3. Control flow, goroutines, locking

### Goroutines
1. **watch goroutine** (one, started by `Start`): ranges over
   `Cluster.Subscribe()`; on each signal (and once immediately at start) it
   takes `e.mu`, derives this node's group from a fresh `Snapshot`, runs
   `reconcileHeal`, and—if a session is running but this node is no longer the
   master of a group that should be playing—calls `session.halt()` and clears
   `e.sess`. Also re-resolves: if membership changed while playing, the current
   session keeps its **original** member endpoint set (a session is fixed at
   Play time; members that join late simply self-heal/resync — spec §5.2/§8 does
   not require live re-fanout, keep it simple). It also runs a 1 s ticker so the
   grace heal fires even with no cluster events (a dead-target heal must trigger
   on time even if the doc is quiet). Exits on `e.done`.
2. **session run goroutine** (one per active session): the 20 ms release ticker
   loop. Reads frames from `Source`, stamps pts, calls `Sender.Send`. On EOF or
   `stop`, calls `sender.Stop()`, then `s.onEnd(reason)`, closes `done`. The
   engine's `onEnd` callback re-acquires `e.mu`, and if `e.sess` is still this
   session, clears it and (for EOF) writes idle playback status + bumps gen.

Total: 2 goroutines at rest (watch + its ticker share one goroutine via
`select`), +1 while playing. No goroutine per member; the Sender owns its own
fan-out internals (G).

### Locking
- **One mutex** `e.mu` guards every mutable Engine field (`sess`, `healAt`,
  `closed`, `self`). Per S's convention: one mutex per component.
- The session has **no mutex**: its only cross-goroutine state is `stop`/`done`
  channels and a `sync.Once`. The run goroutine owns `seq`, `src`, `sender`
  exclusively. `halt()` is called under `e.mu` but only closes `stop` (once) and
  waits on `done` — it must **not** hold `e.mu` while waiting, or `onEnd`
  (which takes `e.mu`) deadlocks. Pattern: copy the `*session` pointer under the
  lock, release the lock, then `halt()`. `onEnd` then re-locks and compares
  identity before clearing `e.sess`.
- `Cluster` setters are called while NOT holding `e.mu` where possible; they are
  C's concern and take C's lock. Calling a setter under `e.mu` is allowed (no
  cycle: C never calls back into H synchronously), but `FollowClient` HTTP calls
  in `makeMaster` are done **without** `e.mu` held (they block on the network).

### Startup (within node lifecycle, driven by K)
`New(deps)` → `Start()`. Watch goroutine does an initial derive + heal so a node
that boots already following a dead master heals after its grace window. No
session at start.

### Steady state
- Cluster changes → watch re-derives → heal reconcile + stale-session teardown.
- `Play` (master) → stop old session → `gen++` → open source → resolve member
  endpoints → `Senders.New(gen, members, settings)` → write playback status
  (state=playing) → spawn session.run.
- `MakeMaster` → stop session → fan out follow/unfollow over HTTP.

### Shutdown
`Close()` → set `closed`, signal `done`, `wg.Wait()`. If a session is running,
copy+`halt()` it first (outside lock). Does **not** rewrite the replicated doc
on close (a dying master's playback status is left as-is; followers' 2 s
watchdog, §8.6, stops their playout, and the 30-day purge / next master cleans
the record). Idempotent via `closed` flag.

---

## 4. Edge cases & failure handling

- **Play on a follower (§9.1)**: `Play` checks `Derive`→ this node is master of
  its group; if not, returns `ErrNotMaster` (I → 409 + takeover hint). The UI's
  "play from a follower" does takeover+play as two API calls (§5.2), not one H
  call.
- **Generation bump (§8.4/§8.6)**: a monotonic `uint32` counter on the Engine,
  incremented on every `Play` and every `MakeMaster` that stops a session, and
  on `Stop`. The session carries its gen; the Sender stamps it; receivers drop
  stale gens. Counter is engine-lived (survives sessions) so it never reuses a
  gen within a node's lifetime.
- **Natural EOF (§8.6)**: `Source.ReadFrame` → `io.EOF`. The session does **not**
  end instantly: it has already released frames up to `leadMs+bufferMs` ahead of
  playout, so it keeps the ticker running with no new sends until that tail has
  elapsed (drain), then `sender.Stop()` (sends the stop control), `onEnd(endEOF)`
  → engine writes `state:idle` playback status. This prevents cutting the last
  ~200 ms.
- **Explicit Stop / replace (§8.6)**: `Stop` or a new `Play` calls `halt()`
  which closes `stop`; the run loop stops sending immediately and calls
  `sender.Stop()` (the stop control bumps nothing further; gen already advanced
  on the *next* Play). Playback status cleared to idle by Stop; left to the new
  session by Play.
- **Follow validation (§5.1)**: reject self-follow, unknown target, dead target,
  and a target that is itself following someone (not a master). Re-point
  (already following someone else) is allowed and just overwrites.
- **Self-heal (§5, 10 s)**: handled in `reconcileHeal`. The grace is armed the
  first time the target is seen invalid and fires after `GraceWindow` even
  absent further cluster events (1 s ticker). If the target becomes valid again
  within the window (e.g. master flaps back), the timer is cancelled. A node
  following an unknown/dead node still behaves as solo immediately for
  derivation; only the *write-back reset* waits 10 s.
- **Takeover missed members (§5.2)**: per-member HTTP errors are logged and
  ignored; the member self-heals to solo or follows late. `MakeMaster` returns
  nil unless the snapshot itself is inconsistent (e.g. `newMaster` not a current
  member → `ErrTargetUnknown`).
- **Takeover called on non-master**: returns `ErrNotMaster`; I is responsible
  for proxying to the current master first (§5.2 step 1 is a proxy hop, an I
  concern). H stays single-node-reasoning.
- **newMaster == self in takeover**: skip the HTTP self-call; invoke
  `e.Unfollow()` directly to avoid a localhost round-trip and a re-proxy guard
  edge.
- **Opus without capability (§8.3)**: `validateSettings` / `Play` reject
  `codec:opus` when `Caps.Codecs` lacks `"opus"` → `ErrNoOpus` (clear API
  error). Default build never lists opus.
- **Unsynced master clock (§7)**: the master needs `Clock.MasterNow().ok` to
  stamp `sessionStart`. The master runs a follower against localhost and is
  synced within ~1 s. If `Play` is called before the first sample, retry-wait up
  to ~2 s for `ok`; if still unsynced, fail `Play` with a transient error rather
  than stamping garbage PTS. (Tested with a fake clock toggling ok.)
- **Source open failure (§6/§8.2)**: bad path, traversal, or decode error → the
  opener returns an error; `Play` returns it un-wrapped enough for I to surface;
  no session, no status write, gen NOT consumed.
- **Sender build failure (§8.4)**: if `Senders.New` errors (e.g. cannot bind TCP
  conns), `Play` closes the source and returns the error; status not written.
- **Membership change mid-session**: the session's endpoint set is frozen at
  Play (kept simple per §11/§8). A member that leaves just stops receiving; one
  that joins isn't added until the next Play. Watch only tears the session down
  if *this node* stops being master.
- **Close while playing**: `halt()` the session, no status rewrite (above).
- **Concurrent Play/Stop/MakeMaster**: serialized by `e.mu`; each copies the
  current `*session` under the lock and halts outside it, then takes the lock
  again to install/clear.

---

## 5. Test plan

All tests use a `fakeCluster` (in-memory snapshot + records, deterministic
Subscribe channel), a `fakeFollowClient` (records calls, optional per-peer
error), a `fakeSource` (N canned frames then io.EOF), a `fakeSender` (records
`Send`/`Stop` calls with seq/pts), a `fakeClock` (settable MasterNow + ok), and
a manual time source for the heal timer. No sockets, no audio.

`derive_test.go`
- `TestDeriveSolo` — single alive node, following Zero → one group, id==nodeID.
- `TestDeriveMasterPlusFollowers` — M + two followers → one group, master=M,
  members sorted, id==XOR(all three).
- `TestDeriveFollowerOfDead` — follower of dead node → its own solo group.
- `TestDeriveFollowerOfUnknown` — following a never-seen id → solo group.
- `TestDeriveFollowerOfFollower` — F follows G, G follows M → F is solo (chain
  not allowed), M's group has only M+G.
- `TestDeriveGroupIDStableAcrossOrder` — member order permuted → same XOR id.
- `TestDeriveAttachesNameAndPlayback` — name/playback/settings join onto group.
- `TestStaleFollowingDetection` — truth table for staleFollowing.

`follow_test.go`
- `TestFollowAliveMaster` — sets following, asserts SetFollowing(target).
- `TestFollowRejectsSelf` — ErrSelfFollow, no setter call.
- `TestFollowRejectsUnknown` — ErrTargetUnknown.
- `TestFollowRejectsDead` — ErrTargetDead.
- `TestFollowRejectsFollower` — target itself follows someone → ErrTargetFollower.
- `TestFollowRepoint` — already following A, follow B → SetFollowing(B).
- `TestUnfollow` — SetFollowing(Zero).

`heal_test.go`
- `TestHealNoResetBeforeGrace` — stale target, now<healAt → no SetFollowing.
- `TestHealResetsAfterGrace` — now>=healAt → SetFollowing(Zero), healAt cleared.
- `TestHealCancelsWhenTargetRecovers` — target becomes valid → healAt cleared,
  no reset.
- `TestHealFiresWithoutClusterEvents` — 1 s ticker drives the reset on time.

`takeover_test.go`
- `TestMakeMasterFanout` — three members, newMaster=follower B → Follow(A→B),
  Follow(C→B) [or C already], Unfollow(B); session stopped first.
- `TestMakeMasterSelfUsesLocalUnfollow` — newMaster==self → local Unfollow, no
  HTTP self-call.
- `TestMakeMasterToleratesMemberError` — one Follow errors → MakeMaster still nil.
- `TestMakeMasterOnNonMasterRejected` — called on a follower → ErrNotMaster.
- `TestMakeMasterUnknownNode` — newMaster not a member → ErrTargetUnknown.
- `TestMakeMasterStopsRunningSession` — running session halted, gen bumped.

`play_test.go`
- `TestPlayRejectsFollower` — follower → ErrNotMaster.
- `TestPlayRejectsMissingFile` — opener error → propagated, no status, gen kept.
- `TestPlayRejectsOpusWithoutCap` — codec opus, no cap → ErrNoOpus.
- `TestPlayWritesPlayingStatus` — SetPlayback(state=playing,file,codec,transport).
- `TestPlayBumpsGeneration` — second Play uses gen+1; sender built with it.
- `TestPlayReplacesRunningSession` — old session halted before new one starts.
- `TestPlayWaitsForClockSync` — MasterNow ok=false then true → succeeds; stays
  false → transient error.

`session_test.go`
- `TestSessionReleasesInOrder` — fakeSender sees seq 0..N-1, pts monotonic step
  FrameNanos, first pts == startMaster.
- `TestSessionStartIsLeadAhead` — startMaster == MasterNow()+LeadMs.
- `TestSessionFanoutAllMembers` — sender constructed with full member endpoint
  set incl. self (Resolver returned self endpoint).
- `TestSessionEOFDrainsThenStops` — after EOF, ticker runs ~bufferMs more, then
  sender.Stop called once, onEnd(endEOF), idle status written.
- `TestSessionStopHaltsImmediately` — Stop → no further Send, sender.Stop once,
  onEnd(endStop), idle status.
- `TestSessionStopIdempotent` — double Stop / Stop-after-EOF → single sender.Stop.

`settings_test.go`
- `TestSettingsDefaults` — unset group → pcm/udp/150.
- `TestSetSettingsMasterWrites` — master → SetGroupSettings called, LWW.
- `TestSetSettingsRejectsFollower` — follower → ErrNotMaster.
- `TestSetSettingsValidatesCodec` — bad codec → ErrBadSettings; opus no-cap →
  ErrNoOpus.
- `TestSetSettingsClampsBuffer` — bufferMs out of range clamped; 0 → 150.

`engine_test.go`
- `TestStartReDerivesOnClusterChange` — Subscribe signal → Group() updates.
- `TestStartArmsHealOnBoot` — boot following a dead node → resets after grace.
- `TestCloseStopsSession` — Close while playing halts the session, exits clean.
- `TestCloseIdempotent` — double Close → nil, no panic, no goroutine leak (race).
- `TestWatchTearsDownSessionOnMasterLoss` — this node loses mastership (e.g.
  membership re-derives it as follower after a forced SetFollowing) → session
  torn down.

All `*_test.go` run with `-race`, no network, no root, no hardware. Time-driven
tests inject a controllable clock/ticker via small interfaces (a `now func()
time.Time` and a tick channel) so no real sleeps beyond microsecond waits on
`done`.
