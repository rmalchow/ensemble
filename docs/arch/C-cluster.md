# C — cluster state via gossip

Source of truth: [docs/README.md](../README.md) §3, §3.1, §4, §5.
Shared contracts: [S-skeleton.md](./S-skeleton.md) — `internal/id`,
`internal/contracts` (`StateStore`, `Snapshot`, `NodeView`, `GroupView`,
`Capabilities`, `Observed`, `Playback`, `GroupSettings`, defaults). Those are
**fixed**; this piece implements `StateStore` and produces those DTOs.

This piece owns `internal/cluster/*`: a thin wrapper around
`hashicorp/memberlist` plus an in-memory, eventually-consistent LWW document
(node records, group-name map, playback map, group-settings map). It exposes
read access (`Snapshot`, `Subscribe`, `DialCandidates`), liveness from
memberlist events, observed-IP tracking (§3.1), own-record setters that bump a
per-record version and broadcast, an hourly 30-day purge, and a `Join` driven by
the discovery `Peer` channel.

**Group derivation (§5) is NOT here.** The cluster stores the raw `following`
field per node and the raw maps; deriving group membership / master / group IDs
is piece H (group engine). The cluster's `Snapshot.Groups` is therefore produced
by a small **derivation function injected by H**, or — to keep C self-contained
and testable without H — C computes the trivial derivation itself from the raw
state it already holds. **Decision: C owns the derivation**, because (a) the
`StateStore.Snapshot()` contract returns `Groups []GroupView` fully resolved,
(b) the derivation is pure data already in C's document (`following` + liveness),
and (c) it is ~40 lines of XOR + map walk with no behavior, only projection.
H still owns the *active* group logic (follow validation, self-heal timer,
takeover, playback). C's derivation is read-only projection for the snapshot;
it never writes `following`. See §4 "Derivation" for the exact rules and the
split with H.

KEEP IT SIMPLE: one document struct, one mutex, JSON gossip messages, no
generics, no plugin abstraction. memberlist is the only external dependency.

---

## 1. Package / file layout

```
internal/cluster/cluster.go      Cluster: lifecycle (New, Start, Close), wires memberlist + delegate,
                                 consumes discovery Peer channel to Join, owns the document + mutex,
                                 Subscribe/notify, hourly purge goroutine.
internal/cluster/doc.go          Document & record types (NodeRecord, GroupName, PlaybackRecord,
                                 GroupSettingsRecord), the LWW merge rules, version/tie-break helpers.
internal/cluster/store.go        StateStore impl: Self, Snapshot (resolve doc+liveness→DTOs),
                                 group derivation, DialCandidates, observed-IP read side.
internal/cluster/setters.go      Own-record mutators: SetName, SetFollowing, SetPlayback,
                                 SetGroupName, SetGroupSettings, Observe — each bumps version +
                                 broadcasts + notifies.
internal/cluster/delegate.go     memberlist Delegate (NodeMeta, LocalState, MergeRemoteState,
                                 NotifyMsg) and EventDelegate (NotifyJoin/Leave/Update) +
                                 the TransmitLimitedQueue broadcast.
internal/cluster/wire.go         gossip message JSON encoding: msgBroadcast (single-record delta),
                                 pushPullState (whole doc), broadcast wrapper implementing
                                 memberlist.Broadcast.
internal/cluster/liveness.go     alive/dead + lastSeen map fed by EventDelegate, separate from the
                                 replicated doc (§4: liveness is memberlist's, not gossiped).

internal/cluster/cluster_test.go     lifecycle, two-node convergence over loopback, Join from Peer chan.
internal/cluster/doc_test.go         merge rules: version LWW, tie-break, monotonicity, purge.
internal/cluster/store_test.go       snapshot resolution, derivation (XOR id, master/members), DialCandidates.
internal/cluster/setters_test.go     each setter bumps version + broadcasts + notifies; Observe map.
internal/cluster/delegate_test.go    LocalState/MergeRemoteState round-trip, NotifyMsg merge, event liveness.
```

Single package `cluster`. `wire.go`/`delegate.go` are split out only for file
size; everything shares the one `Cluster` struct and its one mutex.

---

## 2. Concrete Go API

### 2.1 Public surface (`cluster.go`)

```go
package cluster

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"

	"ensemble/internal/contracts"
	"ensemble/internal/discovery"
	"ensemble/internal/id"
)

// Cluster is the gossip-backed replicated state for one node. It implements
// contracts.StateStore. One per process.
type Cluster struct {
	self id.ID
	log  *slog.Logger

	mu       sync.Mutex          // ONE mutex: guards doc, observed, subs, closed
	doc      *Document           // the replicated LWW document (own + peers)
	observed observedTable       // peerID -> {ip, lastSeen}; our own observations (§3.1)
	subs     []chan struct{}     // Subscribe() coalesced notify channels
	closed   bool

	live *liveness               // alive/lastSeen from memberlist events (own lock)

	ml    *memberlist.Memberlist // set in Start
	queue *memberlist.TransmitLimitedQueue
	deleg *delegate

	peers   <-chan discovery.Peer // from piece B; consumed by joinLoop
	clock   func() time.Time      // injectable for tests (default time.Now)
	purgeEvery time.Duration      // default time.Hour
	maxAge     time.Duration      // default 30*24h (§4)

	done chan struct{}
	wg   sync.WaitGroup
}

// Config wires the cluster. All ports/addrs are the ACTUALLY-bound values from
// netx (K passes them after bind-or-increment, §2).
type Config struct {
	Self       id.ID                 // immutable node id (from config piece A)
	Name       string                // initial display name (own record seed)
	Caps       contracts.Capabilities
	Addrs      []string              // self-reported interface CIDRs (netx.InterfaceCIDRs)
	HTTPPort   int
	StreamPort int
	GossipPort int                   // bound gossip port (memberlist BindPort)
	BindAddr   string                // gossip bind host ("0.0.0.0")
	Peers      <-chan discovery.Peer // discovery channel to Join (piece B)
	Logger     *slog.Logger

	// Optional test hooks (nil → production defaults).
	Now        func() time.Time      // default time.Now
	PurgeEvery time.Duration         // default time.Hour
	MaxAge     time.Duration         // default 30 * 24h
}

// New builds the Cluster and its memberlist config, seeding this node's own
// record (version 1) from cfg. It does NOT start networking; call Start.
func New(cfg Config) (*Cluster, error)

// Start creates the memberlist, begins gossiping, and launches the join loop
// (drains cfg.Peers) and the hourly purge loop. Non-blocking.
func (c *Cluster) Start() error

// Close leaves the cluster gracefully (memberlist.Leave with a timeout, then
// Shutdown), stops the goroutines, and closes subscriber channels. Idempotent.
func (c *Cluster) Close() error
```

### 2.2 `contracts.StateStore` implementation (`store.go`)

```go
// Self returns this node's immutable id.
func (c *Cluster) Self() id.ID

// Snapshot returns a deep-copied, resolved, JSON-ready view: every node record
// joined with liveness + staleness, plus derived groups (§5). Safe for
// concurrent callers; holds the mutex only for the copy, derives outside it.
func (c *Cluster) Snapshot() contracts.Snapshot

// Subscribe returns a coalesced change channel (buffer 1, non-blocking sends).
// Signaled on every applied document change (own setter or remote merge) and on
// liveness changes. Channel is closed on Close.
func (c *Cluster) Subscribe() <-chan struct{}
```

### 2.3 Address resolution (`store.go`, §3.1)

```go
// DialCandidates returns IP:port-free candidate IPs for reaching peer, ordered
// best-first, per §3.1: the peer's self-reported CIDR IPs INTERSECTED with the
// set of IPs that ANY node in the cluster has observed for that peer; an IP no
// one has observed is excluded. Within the intersection, most-recently-observed
// first. If the intersection is empty, falls back to the peer's self-reported
// IPs (so a brand-new peer nobody has talked to yet is still dialable) — this
// fallback is the pragmatic deviation noted in §6.
//
// Returns bare IPs (netip.Addr); the caller appends the relevant port
// (peer.HTTPPort / StreamPort / GossipPort) from the same NodeView.
func (c *Cluster) DialCandidates(peer id.ID) []netip.Addr
```

### 2.4 Own-record setters (`setters.go`)

Every setter: takes the mutex, mutates **only this node's own record** (or the
shared group maps it is allowed to write), bumps that record's `Version`, sets
`UpdatedAt = now`, releases the mutex, then queues a single-record broadcast and
notifies subscribers. No-op (no version bump, no broadcast) if the value is
unchanged, to avoid gossip churn.

```go
// SetName renames this node (§1, PATCH /api/node). Persisted by piece A
// separately; here it only updates+broadcasts the replicated copy.
func (c *Cluster) SetName(name string)

// SetFollowing sets this node's following target (§5). id.Zero == solo master.
func (c *Cluster) SetFollowing(target id.ID)

// SetCapabilities replaces this node's reported capabilities (rare; boot-time).
func (c *Cluster) SetCapabilities(caps contracts.Capabilities)

// SetAddrs replaces self-reported interface CIDRs (§3.1) — e.g. on a network
// change. Boot value comes from Config.
func (c *Cluster) SetAddrs(cidrs []string)

// SetPlayback writes the playback-status record for group (§4). Caller (H, the
// group master) is responsible for only writing groups it masters; C does not
// police mastership (no group derivation authority for writes). Pass an empty
// Playback{State:"idle"} to clear.
func (c *Cluster) SetPlayback(group id.ID, pb contracts.Playback)

// SetGroupName writes/renames a group (§4; any node may write, LWW).
func (c *Cluster) SetGroupName(group id.ID, name string)

// SetGroupSettings writes per-group codec/transport/bufferMs (§8.3/§8.4, master
// only by policy in H; LWW here). Empty fields are filled with defaults.
func (c *Cluster) SetGroupSettings(group id.ID, s contracts.GroupSettings)

// Observe records that we saw `peer` sending from IP `ip` now (§3.1). Fed by
// the HTTP layer (client remote IP) and by gossip events. Updates our own
// observed map; if it changed materially (new IP, or lastSeen advanced past a
// throttle), bumps our node record version + broadcasts so the cluster learns
// our observation. Throttled: re-observing the same IP only re-broadcasts at
// most once per observeBroadcastInterval (default 30s) to avoid churn.
func (c *Cluster) Observe(peer id.ID, ip netip.Addr)
```

### 2.5 Document & records (`doc.go`)

The replicated document. **Stored types are distinct from the `contracts.*`
DTOs**: stored types carry version metadata and use `id.ID` maps; DTOs are the
resolved JSON view. `doc.go` converts stored → DTO during `Snapshot`.

```go
package cluster

import "ensemble/internal/contracts"
import "ensemble/internal/id"

// Document is the whole replicated state (§4). In-memory only; never persisted.
type Document struct {
	Nodes    map[id.ID]*NodeRecord          `json:"nodes"`
	Groups   map[id.ID]*GroupNameRecord     `json:"groups"`   // group names map
	Playback map[id.ID]*PlaybackRecord      `json:"playback"` // per-group playback
	Settings map[id.ID]*GroupSettingsRecord `json:"settings"` // per-group settings
}

// NodeRecord — owned and only ever written by the node it describes (§4).
type NodeRecord struct {
	ID         id.ID                  `json:"id"`
	Name       string                 `json:"name"`
	Addrs      []string               `json:"addrs"` // self-reported CIDRs (§3.1)
	HTTPPort   int                    `json:"httpPort"`
	StreamPort int                    `json:"streamPort"`
	GossipPort int                    `json:"gossipPort"`
	Caps       contracts.Capabilities `json:"caps"`
	Following  id.ID                  `json:"following"` // id.Zero == solo
	Observed   map[id.ID]obsEntry     `json:"observed"`  // peerID -> {ip,lastSeen}
	Version    uint64                 `json:"version"`
	UpdatedAt  int64                  `json:"updatedAt"` // unix seconds, LWW timestamp
}

// obsEntry is one observed-IP record inside a NodeRecord (§3.1).
type obsEntry struct {
	IP           string `json:"ip"`
	LastSeenUnix int64  `json:"lastSeen"`
}

// GroupNameRecord — group names map value (§4, LWW, any writer).
type GroupNameRecord struct {
	Name      string `json:"name"`
	Version   uint64 `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
	Writer    id.ID  `json:"writer"` // last node that wrote it (tie-break, §4)
}

// PlaybackRecord — per-group playback status (§4, written by group master).
type PlaybackRecord struct {
	State       string  `json:"state"` // "idle" | "playing"
	File        string  `json:"file"`
	StartedUnix int64   `json:"startedAt"`
	PositionSec float64 `json:"positionSec"`
	Codec       string  `json:"codec"`
	Transport   string  `json:"transport"`
	Version     uint64  `json:"version"`
	UpdatedAt   int64   `json:"updatedAt"`
	Writer      id.ID   `json:"writer"`
}

// GroupSettingsRecord — per-group codec/transport/bufferMs (§8.3/§8.4, LWW).
type GroupSettingsRecord struct {
	Codec     string `json:"codec"`
	Transport string `json:"transport"`
	BufferMs  int    `json:"bufferMs"`
	Version   uint64 `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
	Writer    id.ID  `json:"writer"`
}
```

**Why a `Writer` field on the shared maps but not on `NodeRecord`:** a node
record is only ever written by its own node, so its `id.ID` key *is* the writer
— no tie-break ever needed. Group names/playback/settings can be written by
multiple nodes (any node renames a group; mastership moves), so they need an
explicit writer id for the LWW tie-break (§4).

### 2.6 Merge rules (`doc.go`)

This is the load-bearing part. Per-record monotonic version; tie-broken by
writer node id (§4).

```go
// versionedLater reports whether the (version, writer) of `b` wins over `a`
// under the LWW rule: higher Version wins; on equal Version the larger writer
// id (lexicographic over the 16 bytes) wins. Equal version AND equal writer →
// not later (idempotent; keeps `a`).
func versionedLater(aVer uint64, aWriter id.ID, bVer uint64, bWriter id.ID) bool

// mergeNode merges remote node record r into the document. The node's own id is
// the writer (no Writer field). Returns true if the local doc changed.
// Special case: we NEVER let a remote merge overwrite OUR OWN record (we are the
// sole writer of our record) — a remote copy of our record can only be staler or
// equal; if a peer somehow has a higher version for us, we ignore it and, on the
// next own-setter call, our version already advances past it because we keep a
// local monotonic counter (see §3 own-version monotonicity).
func (d *Document) mergeNode(self id.ID, r *NodeRecord) bool

// mergeGroupName / mergePlayback / mergeSettings: LWW by (Version, Writer).
func (d *Document) mergeGroupName(g id.ID, r *GroupNameRecord) bool
func (d *Document) mergePlayback(g id.ID, r *PlaybackRecord) bool
func (d *Document) mergeSettings(g id.ID, r *GroupSettingsRecord) bool

// mergeAll merges an entire remote Document (push/pull path). Returns true if
// anything changed locally (→ notify subscribers).
func (d *Document) mergeAll(self id.ID, remote *Document) bool

// clone deep-copies the document (for Snapshot and LocalState encoding).
func (d *Document) clone() *Document
```

### 2.7 Gossip wire (`wire.go`)

Two message kinds. Both JSON (§4 explicitly allows JSON "at this scale").

```go
// kind tags a NotifyMsg broadcast payload (first byte before the JSON).
const (
	kindNodeDelta     byte = 'n' // one NodeRecord
	kindGroupName     byte = 'g' // one group-name entry  {group, rec}
	kindPlayback      byte = 'p' // one playback entry     {group, rec}
	kindSettings      byte = 's' // one settings entry     {group, rec}
)

// delta is the JSON body of a single-record broadcast: exactly one of the
// pointers is non-nil, selected by the leading kind byte. Group is the map key
// for the group-scoped kinds (ignored for nodes).
type delta struct {
	Group    id.ID                `json:"group,omitempty"`
	Node     *NodeRecord          `json:"node,omitempty"`
	Name     *GroupNameRecord     `json:"name,omitempty"`
	Playback *PlaybackRecord      `json:"playback,omitempty"`
	Settings *GroupSettingsRecord `json:"settings,omitempty"`
}

// encodeDelta produces []byte{kind} ++ json(delta) for a NotifyMsg broadcast.
func encodeDelta(kind byte, d delta) []byte

// decodeDelta parses a NotifyMsg payload back to (kind, delta).
func decodeDelta(msg []byte) (byte, delta, error)

// broadcast implements memberlist.Broadcast for a single delta.
type broadcast struct {
	key string // dedup key: kind + record id, so a newer delta supersedes older
	msg []byte
}
func (b *broadcast) Invalidates(other memberlist.Broadcast) bool // same key → true
func (b *broadcast) Message() []byte                             { return b.msg }
func (b *broadcast) Finished()                                   {}
```

Push/pull uses `Document` directly: `LocalState` returns `json(doc.clone())`;
`MergeRemoteState` parses it into a `Document` and calls `mergeAll`.

### 2.8 Delegate (`delegate.go`)

```go
type delegate struct{ c *Cluster }

// memberlist.Delegate
func (d *delegate) NodeMeta(limit int) []byte                  // our node id (16 bytes)
func (d *delegate) NotifyMsg(msg []byte)                       // a broadcast delta → merge
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte // from c.queue
func (d *delegate) LocalState(join bool) []byte                // json(doc.clone())
func (d *delegate) MergeRemoteState(buf []byte, join bool)     // mergeAll → notify

// memberlist.EventDelegate
func (d *delegate) NotifyJoin(n *memberlist.Node)   // mark alive, observe IP
func (d *delegate) NotifyLeave(n *memberlist.Node)  // mark dead
func (d *delegate) NotifyUpdate(n *memberlist.Node) // refresh lastSeen, observe IP
```

`NodeMeta` carries our 16-byte node id so memberlist node name ↔ ensemble id
mapping is available in events (we also set `memberlist.Config.Name = self.String()`,
making the mapping trivial; `NodeMeta` is the robust backup and the source for
`NotifyJoin` to extract the peer id without parsing the name).

### 2.9 Liveness (`liveness.go`)

```go
// liveness tracks alive/dead + lastSeen, fed by EventDelegate. Separate from the
// replicated Document (§4: liveness is memberlist's, not gossiped). Its own tiny
// mutex (an exception to one-mutex-per-component, justified: it is written from
// the memberlist event goroutine and read in Snapshot; keeping it separate keeps
// the main doc mutex off the event hot path and avoids lock-ordering with it).
type liveness struct {
	mu    sync.Mutex
	alive map[id.ID]bool
	seen  map[id.ID]int64 // unix seconds of last event
}

func (l *liveness) join(peer id.ID, now int64)
func (l *liveness) leave(peer id.ID)
func (l *liveness) update(peer id.ID, now int64)
func (l *liveness) snapshot() (alive map[id.ID]bool, seen map[id.ID]int64)
```

---

## 3. Control flow, goroutines, locking

### Startup (`New` → `Start`)

1. `New(cfg)`:
   - validate cfg (self non-zero, ports > 0).
   - build `Document` with our own `NodeRecord` at `Version: 1`,
     `UpdatedAt: now`, seeded from cfg (name, caps, addrs, ports, Following=Zero,
     empty Observed).
   - build `memberlist.Config` (`DefaultLANConfig()` tuned): `Name =
     self.String()`, `BindAddr = cfg.BindAddr`, `BindPort = cfg.GossipPort`,
     `AdvertisePort = cfg.GossipPort`, `Delegate = deleg`, `Events = deleg`,
     `Logger` routed to slog at warn. **`memberlist` is created in `Start`, not
     `New`**, so `New` does no networking (unit-testable doc/merge without a
     socket).
   - init `liveness` (self marked alive), empty `observed`, `queue`.
2. `Start()`:
   - `memberlist.Create(mlConfig)` (binds the already-reserved gossip port; K
     guarantees it is free because netx bound a placeholder then handed the
     number — **see contractConcern about gossip port ownership**).
   - set `c.queue.NumNodes = func() int { return c.ml.NumMembers() }`.
   - launch `joinLoop` goroutine (drains `cfg.Peers`).
   - launch `purgeLoop` goroutine (ticker `purgeEvery`).

### Steady state — goroutines

- **memberlist internal goroutines** (gossip, push/pull, probe): owned by
  memberlist; call into our `delegate` methods. Treated as external callers of
  our mutex.
- **joinLoop** (one goroutine): `for peer := range c.peers { ... c.ml.Join([]string{addr}) }`.
  Dedup: skip if `peer.ID == self` or already a live member
  (`memberlist.Members()` contains the id). `Join` is best-effort; log at debug
  on error (peer may be transiently unreachable; mDNS will re-emit). The Join
  address is `netip.AddrPortFrom(peer.Addr, peer.GossipPort)` from the discovery
  Peer (we trust discovery's directly-observed address for the *join*; §3.1
  intersection governs later app-level dials, not the bootstrap join — noted in
  §6).
- **purgeLoop** (one goroutine): every `purgeEvery`, take mutex, delete any
  `NodeRecord`/`GroupNameRecord`/`PlaybackRecord`/`GroupSettingsRecord` whose
  `UpdatedAt` is older than `maxAge` (§4: 30 days), **except our own node
  record** (never purge self). If anything was deleted, notify subscribers.

### delegate callbacks (run on memberlist goroutines)

- `NotifyMsg(msg)`: `decodeDelta` → take mutex → `merge*` the single record →
  release → if changed, queue **re-broadcast is automatic** (the delta is still
  in our queue if it originated here; for received deltas we rely on
  memberlist's own gossip fan-out, we do NOT re-enqueue to avoid storms) and
  `notify()`.
- `MergeRemoteState(buf, join)`: parse `Document` → `mergeAll` under mutex → if
  changed, `notify()`. This is the push/pull anti-entropy path that guarantees
  convergence even if individual broadcasts were lost.
- `LocalState(join)`: under mutex, `json(doc.clone())`.
- `NotifyJoin/Update`: extract peer id (from `n.Name`, fallback `n.Meta`); mark
  liveness; `Observe(peer, n.Addr)` (memberlist gives the real remote IP — a
  first-class §3.1 observation). `notify()`.
- `NotifyLeave`: mark dead; `notify()`. The record stays in the doc (dead ≠
  purged); derivation treats it as not-alive.

### Setters (called from API/group goroutines)

`Set*` → mutex → mutate own record/shared map, bump version (see monotonicity)
→ release → `enqueueBroadcast(delta)` (push to `c.queue`) → `notify()`.

**Own-version monotonicity:** the node keeps the authoritative version of its
own record in `doc.Nodes[self].Version`. Each own setter does
`rec.Version++`. We never accept a remote write to our own record
(`mergeNode` ignores `r.ID == self` unless `r.Version > ours`, and even then we
*don't copy it* — we bump ours to `r.Version+1` on the next setter). Practically:
because only we ever increment our version and we persist nothing, a process
restart resets our version to 1; a peer may still hold our old record at a higher
version. **Resolution:** on `Start`, after `memberlist.Create` + first push/pull,
if a remote copy of our own record has `Version >= ours`, set
`ours.Version = remote.Version + 1` and re-broadcast (a one-time reconciliation
in `Start`'s post-join step). This guarantees our subsequent writes win. Tested
in `doc_test.go` / `cluster_test.go`.

### notify() and Subscribe

`notify()` does, for each subscriber chan, a non-blocking send
(`select { case ch <- struct{}{}: default: }`) — coalesced. Subscribers (API WS,
group engine) debounce on their side (§9.2 ≥250 ms). `notify` is called WITHOUT
holding the doc mutex where possible; if called under it, sends are non-blocking
so no deadlock. `Subscribe` appends a `chan struct{}` (buffer 1) under mutex.

### Locking strategy

- **One primary mutex** `c.mu` guards `doc`, `observed`, `subs`, `closed`.
- **One small auxiliary mutex** inside `liveness` (justified in §2.9): it is on
  the memberlist event hot path and read in `Snapshot`; keeping it separate
  avoids holding the big doc mutex during every probe and removes any
  lock-ordering hazard (liveness never calls back into `c.mu`). Lock ordering
  rule: **never hold `c.mu` and `liveness.mu` simultaneously.** `Snapshot`
  reads them sequentially (copy doc under `c.mu`, release, then read liveness).
- No other locks. memberlist has its own internal locking; we never call a
  memberlist method while holding `c.mu` except `c.queue.QueueBroadcast` (which
  is internally synchronized and safe).

### Shutdown (`Close`)

1. set `closed = true` under mutex (setters become no-ops; idempotent).
2. `close(c.done)` → joinLoop and purgeLoop exit.
3. `c.ml.Leave(5*time.Second)` then `c.ml.Shutdown()` (best-effort, log errors).
4. `c.wg.Wait()`.
5. close all subscriber channels (under mutex), nil the slice.

---

## 4. Derivation (Snapshot.Groups) and the split with H

C's `Snapshot()` must return `Groups []GroupView` (the `StateStore` contract).
The derivation is the **read-only projection** of §5; it does NOT mutate
`following` or run the 10s self-heal timer (those are H's, the active engine).

Derivation algorithm (pure, over the doc snapshot + liveness):

1. Let `alive(n)` = liveness says n is alive (self always alive).
2. A node `M` is a **master** iff `alive(M)` and `M.Following == Zero`.
   Also treat as solo-master (for derivation) any alive node whose `Following`
   points to a dead/unknown node, or to a node that is itself following someone
   (§5 "behaves as solo"). C does **not** reset their `following` — that is H's
   self-heal write; C only projects them as their own master so the snapshot is
   coherent in the gap before H heals.
3. For each master `M`: `members = {M} ∪ {n : alive(n), n.Following == M}`.
   `groupID = id.XOR(members...)` (commutative; solo → groupID == nodeID, §5).
4. `Name` = `doc.Groups[groupID].Name` (or `""`). `Playback` =
   `doc.Playback[groupID]` resolved (or `{State:"idle"}`). `Settings` =
   `doc.Settings[groupID]` resolved (or contracts defaults).
5. Emit one `GroupView` per master; sort members and groups by id for stable
   JSON (avoids WS churn from map iteration order).

This duplicates a *small* slice of §5 logic with H. The contract boundary:
- **C**: projection for read (`Snapshot.Groups`), no writes, no timers.
- **H**: the authoritative engine — validates follow (target alive + is master),
  performs takeover, runs the 10s self-heal that *writes* `following` back to
  Zero via `c.SetFollowing(Zero)`, and orchestrates playback. H derives the same
  groups for its own logic; it MAY call a shared helper. To avoid two
  divergent derivations, **C exports the pure function** so H can reuse it:

```go
// DeriveGroups projects derived groups (§5) from a document snapshot and a
// liveness view. Pure; no writes. Exported so the group engine (H) reuses the
// exact same rule C uses for Snapshot. self is included as always-alive.
func DeriveGroups(
	nodes map[id.ID]*NodeRecord,
	names map[id.ID]*GroupNameRecord,
	playback map[id.ID]*PlaybackRecord,
	settings map[id.ID]*GroupSettingsRecord,
	alive map[id.ID]bool,
	self id.ID,
) []contracts.GroupView
```

If the integrator prefers H to own derivation entirely, C can instead accept a
`DeriveFunc` in `Config` and call it in `Snapshot`. **Default: C owns
`DeriveGroups` and exports it** (simplest, no injection, testable standalone).
Recorded as a contractConcern since it touches the C/H boundary.

---

## 5. Edge cases & failure handling

- **Own record never overwritten by remote (§4 "owned and only ever written by
  the node itself").** `mergeNode` skips `r.ID == self` except to trigger the
  one-time version reconciliation at `Start` (§3). A malicious/buggy peer cannot
  hijack our record.
- **Version tie (§4 tie-break by node id).** Equal `Version` on a shared map
  record → larger writer id wins (`versionedLater`). Deterministic across the
  cluster, so all nodes converge to the same value regardless of delivery order.
- **Process restart resets own version to 1 (no persistence, §4 in-memory).**
  Stale higher-versioned copies of our record linger in peers. The `Start`
  reconciliation bumps our version above the max remote copy so our writes win.
  Without it, our `SetName` post-restart could be silently dropped by peers.
- **Stale `following` → dead/unknown master.** Derivation treats such a node as
  its own master (solo) for the snapshot (§5). C does not write; H self-heals.
  Prevents phantom groups pointing at dead masters.
- **Group ID is derived, so a renamed group keeps its name when it reforms
  (§5).** Names are keyed by group id (XOR of members); C stores them keyed by
  id, never by membership, so reforming the same node set reuses the name. No
  special handling needed — falls out of the keying.
- **Purge excludes self and live members.** Never purge our own node record;
  also skip purging a node record whose node is currently alive (a long-lived
  but quiet node shouldn't be purged just because its *record* wasn't rewritten
  in 30 days — though in practice liveness traffic keeps it present). Group
  name/playback/settings purge purely on `UpdatedAt` age (§4) regardless of
  liveness.
- **Observed-IP churn (§3.1).** `Observe` throttles re-broadcast of the same
  (peer, ip) to once / 30s; only a *new* ip or a peer-id we hadn't observed
  forces an immediate version bump + broadcast. Prevents every HTTP request from
  re-gossiping our whole node record.
- **`DialCandidates` empty intersection (§3.1).** If no node has observed the
  peer yet (cold start), the strict rule yields nothing → unreachable. We fall
  back to the peer's self-reported CIDR IPs so first contact is possible; once
  any traffic flows, the observation tightens the set. This is a deliberate
  softening of §3.1's "an IP that no node has ever observed is ignored" for the
  bootstrap case — flagged as a contractConcern.
- **CIDR parse on dial.** `Addrs` are CIDRs; `DialCandidates` strips the prefix
  to the bare `netip.Addr` (parse with `netip.ParsePrefix`, take `.Addr()`).
  Unparseable entries are skipped, not fatal.
- **memberlist Name vs ensemble id.** We set `Config.Name = self.String()`; if a
  collision ever occurred (two nodes same id — impossible by §1 randomness) the
  later join is rejected by memberlist; acceptable.
- **Broadcast size vs UDP MTU.** A single `NodeRecord` with many observed entries
  could exceed the gossip UDP limit; memberlist falls back to TCP for oversized
  user messages via push/pull, and `GetBroadcasts` respects the `limit`. We keep
  deltas to one record each (not whole-doc) precisely to stay small.
- **Join of self / already-member.** joinLoop skips `peer.ID == self` and peers
  already in `ml.Members()`.
- **Notify after Close.** `notify` checks `closed`; sends nothing to closed
  channels (channels already closed in Close; `closed` guard prevents send-on-
  closed panics from a late event).
- **Concurrent setter + remote merge of a shared map.** Both take `c.mu`; LWW is
  associative+commutative so order doesn't matter for the converged value, and
  the version bump under the lock keeps our local writes monotonic.

---

## 6. Deviations / pragmatic choices (vs spec, flagged)

1. **DialCandidates fallback** to self-reported IPs when the observation
   intersection is empty — needed for first contact; strict §3.1 would make a
   never-observed peer permanently unreachable. (contractConcern)
2. **C owns group derivation** (`DeriveGroups`) and exports it for H to reuse,
   rather than H injecting a derive func. Touches the C/H boundary which the
   skeleton leaves implicit. (contractConcern)
3. **Bootstrap Join** uses discovery's directly-observed address, not the §3.1
   intersection (which would be empty at join time). §3.1's intersection governs
   *app-level* dials (proxy/clock/stream), not the gossip bootstrap.
4. **Two mutexes** (main doc + tiny liveness) rather than strictly one, to keep
   the doc lock off the memberlist event hot path. Lock-order rule documented.

---

## 7. Test plan

`internal/cluster/doc_test.go`
- `TestMergeNodeHigherVersionWins` — remote higher version replaces local.
- `TestMergeNodeLowerVersionIgnored` — remote lower/equal version is a no-op.
- `TestMergeNodeNeverOverwritesSelf` — `r.ID == self` is ignored by merge.
- `TestMergeGroupNameTieBreakByWriter` — equal version, larger writer id wins.
- `TestMergePlaybackLWW` — version-ordered; writer tie-break.
- `TestMergeSettingsDefaults` — empty settings fields filled with contracts defaults.
- `TestMergeAllConvergence` — merging A∪B == B∪A (order independence).
- `TestVersionedLater` — truth table for (ver, writer) comparison incl. equality.
- `TestCloneIsDeep` — mutating a clone doesn't touch the original maps/slices.
- `TestPurgeOldRecords` — records older than maxAge dropped; self + fresh kept.

`internal/cluster/setters_test.go`
- `TestSetNameBumpsVersionAndBroadcasts` — version++, delta enqueued, notify fires.
- `TestSetNameNoOpWhenUnchanged` — same name → no version bump, no broadcast.
- `TestSetFollowingZeroIsSolo` — Following set to Zero; reflected in snapshot.
- `TestSetPlaybackWritesGroupKey` — playback stored under group id with writer=self.
- `TestSetGroupSettingsFillsDefaults` — partial settings get defaults.
- `TestObserveNewIPBroadcasts` — first observation bumps version + broadcasts.
- `TestObserveSameIPThrottled` — repeat within interval: no extra broadcast.
- `TestObserveUpdatesOwnRecordMap` — own NodeRecord.Observed reflects the peer.

`internal/cluster/store_test.go`
- `TestSnapshotResolvesLiveness` — alive/dead/stale flags from liveness view.
- `TestSnapshotDeepCopy` — mutating snapshot doesn't affect the live doc.
- `TestDeriveGroupsSolo` — single node → one group, id == node id, master==self.
- `TestDeriveGroupsFollowerJoins` — B follows A → one group {A,B}, master A,
  id == XOR(A,B).
- `TestDeriveGroupsDeadMasterSolo` — follower of a dead node projected as solo.
- `TestDeriveGroupsFollowingAFollowerSolo` — following a node that itself follows
  → projected solo (§5).
- `TestDeriveGroupsStableOrder` — members/groups sorted by id; deterministic JSON.
- `TestDialCandidatesIntersection` — only observed∩self-reported IPs, recent first.
- `TestDialCandidatesFallbackEmptyObservations` — cold peer → self-reported IPs.
- `TestDialCandidatesSkipsUnparseableCIDR` — bad CIDR entry ignored, others kept.

`internal/cluster/delegate_test.go`
- `TestLocalStateMergeRemoteRoundTrip` — encode LocalState, MergeRemoteState into
  a fresh doc → equal.
- `TestNotifyMsgAppliesDelta` — a node delta via NotifyMsg merges + notifies.
- `TestNotifyMsgBadPayloadIgnored` — malformed msg dropped, no panic.
- `TestEventDelegateLiveness` — Join→alive, Leave→dead, Update→lastSeen advance.
- `TestEventJoinObservesIP` — NotifyJoin records the peer's remote IP (§3.1).
- `TestEncodeDecodeDeltaAllKinds` — node/name/playback/settings deltas round-trip.
- `TestBroadcastInvalidates` — same key supersedes; different key does not.

`internal/cluster/cluster_test.go`
- `TestNewSeedsOwnRecord` — own NodeRecord present, version 1, fields from Config.
- `TestStartCloseNoLeak` — Start then Close; goroutines exit (`-race`, leak check).
- `TestCloseIdempotent` — second Close is a no-op, no panic.
- `TestTwoNodesConvergeOverLoopback` — two Clusters on 127.0.0.1, Join via Peer
  channel, both Snapshots list both nodes (push/pull convergence). [uses real
  memberlist on loopback ports; tagged but not network-root-requiring]
- `TestFollowPropagates` — node B SetFollowing(A); A's snapshot eventually shows
  the derived {A,B} group.
- `TestJoinLoopSkipsSelfAndMembers` — Peer with self id or existing member: no Join.
- `TestStartReconcilesOwnVersionAfterRestart` — peer holds higher-versioned self
  record; after Start our version is bumped above it and a SetName wins.
- `TestSubscribeCoalesces` — multiple rapid changes → at least one signal, never
  blocks; channel closed on Close.

All tests run on loopback, with `Config.Now` injected for deterministic
purge/throttle timing and a small `MaxAge`/`PurgeEvery` in tests. Two-node
convergence uses ephemeral gossip ports via `BindPort: 0` semantics (or a
free-port helper); no root, no external network, no multicast (mDNS is piece B
and is faked here by feeding the `Peers` channel directly).
```
