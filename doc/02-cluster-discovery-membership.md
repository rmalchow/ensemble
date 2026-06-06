# 02 — Cluster discovery, membership & per-group master election

> Spine: [README.md](./README.md). This document elaborates decisions **D7**
> (discovery — LAN broadcast / mDNS, zero-config, reuse) and **D8**
> (membership/replication — memberlist gossip, versioned config doc, last-writer-
> wins, replicated to all full nodes). It uses the canonical contracts from
> README §6 verbatim — in particular `ConfigDoc`/`NodeRecord`/`GroupRecord`
> (§6.5) — and **does not redefine them**.
>
> Sibling cross-references:
> - **[01-architecture-and-packages.md](./01-architecture-and-packages.md)** —
>   package responsibilities, boot/adopt/play/failover sequence diagrams. This
>   doc supplies the discovery→join→elect→role steady state those diagrams enter.
> - **[03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md)**
>   — what happens *after* an uninitialized node is discovered here: PIN-gated
>   CSR signing, cert distribution, the source-IP **allowlist** derived from
>   `ConfigDoc.Nodes[].Addrs`. Discovery only *surfaces* an adoptable node; trust
>   is established there.
> - **[04-clock-and-groups.md](./04-clock-and-groups.md)** — consumes the
>   per-group master id and `Generation` counter elected here to (re)baseline the
>   per-group clock and run the group engine. The election is the *input* to the
>   clock; the clock is out of scope here.
> - **[07-config-and-replication.md](./07-config-and-replication.md)** — the full
>   `ConfigDoc` schema, persistence, and allowlist derivation. This doc specifies
>   only *how the doc rides gossip and converges* (the merge mechanics reused from
>   mpvsync `state`).

This section describes the parts of Ensemble that let a freshly-powered node
**find** the cluster, **join** it, **agree** with every other node on who is in
the cluster and which node leads each group, and **converge** the replicated
configuration — all with zero static configuration. The mechanics are lifted
directly from the proven mpvsync `internal/cluster` and `internal/state`
packages; this document cites the real mpvsync code being reused and calls out
exactly where Ensemble extends it (the one structural change: **election is
per-group, not per-cluster**).

---

## 1. Overview & the reuse map

```
   power on
      │
      ▼
 ┌───────────┐    mDNS announce + browse     ┌──────────────┐
 │ discovery │ ───────────────────────────► │ seed addrs    │
 │ (D7)      │ ◄─── TXT: id,name,group,wp ── │ (ip:port)     │
 └───────────┘                               └──────┬───────┘
                                                    │ memberlist.Join(seeds)
                                                    ▼
 ┌──────────────────────────────────────────────────────────────┐
 │ cluster  — memberlist SWIM gossip (D8)                         │
 │   • alive/suspect/dead failure detection                       │
 │   • per-node Meta (id, name, group, ports) gossiped            │
 │   • delegate push/pull anti-entropy carries the ConfigDoc      │
 └───────┬───────────────────────────────────────────┬───────────┘
         │ Members()                                  │ MergeRemoteState
         ▼                                            ▼
 ┌──────────────────┐                        ┌──────────────────┐
 │ per-group        │                        │ state — ConfigDoc │
 │ election         │                        │  LWW merge (§6.5) │
 │  lowest stable id│                        │  versioned        │
 │  + Generation    │                        └──────────────────┘
 └────────┬─────────┘
          ▼
   group engine / clock  → see 04
```

| Mechanic | Reused mpvsync source | Ensemble change |
|---|---|---|
| mDNS announce / browse | `internal/cluster/discovery.go` (`Register`, `Browse`, `BrowseAll`, `txtRecords`, `nodeFromTXT`) | service type `_ensemble._udp`; TXT gains `init` flag + `cf` (cluster fingerprint); browse keyed by **cluster fingerprint**, not group name |
| memberlist SWIM gossip | Appendix **A.14** (wrapper `New`, `Membership`, `Meta`, `Member`, `delegate`) | `Meta` carries cluster fingerprint; gossip key = `gossipKey` from `ClusterSecrets` (not group password) |
| previously-seen peer cache | Appendix **A.14** (`PeerStore`, `Upsert`, `JoinSeeds`, `Clear`) | full contract in A.14 (file `peers.json`, mode 0644) |
| master election | `internal/cluster/election.go` (`Election`, `Update`, `Generation`, `IsMaster`) | **one `Election` per group** instead of one per node/cluster |
| ConfigDoc replication / LWW | `internal/state/state.go` (`Store`, `Merge`, `MergeGossip`, `MarshalGossip`) | the merged doc is `ConfigDoc` (§6.5), not mpvsync's playlist `Doc`; same LWW-by-version, id-tiebreak algorithm |
| 4-state device classification | `internal/web/api_cluster.go` (`buildDevices`) | adds the `uninitialized` state (TXT `init=0`) so the UI can offer adoption |

Everything below is the steady state these pieces form. Boot ordering and the
adopt handshake are sequenced in [01](./01-architecture-and-packages.md) and
[03](./03-adoption-takeover-security-pki.md) respectively.

---

## 2. Discovery (D7) — LAN broadcast + mDNS

### 2.1 What & why

Discovery exists only to **bootstrap memberlist**: a node finds one or two peers,
hands their gossip addresses to `memberlist.Join`, and from then on SWIM gossip
(§3) maintains membership. This is the exact role discovery plays in mpvsync —
see the package doc on `internal/cluster/discovery.go:12`: *"Discovery is used
only to bootstrap memberlist."* mDNS is **never** the source of truth for
membership; it is a hint generator that is allowed to be slow, lossy, or briefly
wrong.

mpvsync uses DNS-SD / mDNS via `github.com/grandcat/zeroconf`. Ensemble keeps the
library and the announce/browse split, changing the service type and the TXT
payload.

```go
// internal/discovery (mirrors mpvsync internal/cluster/discovery.go)
const (
    mdnsService = "_ensemble._udp"
    mdnsDomain  = "local."
)
```

### 2.2 What is advertised

Each node announces via `zeroconf.Register` (cf. mpvsync
`discovery.go:31 Register`). The advertised **port is the memberlist gossip
port**; everything else rides TXT key=value records (mpvsync builds these in
`txtRecords`, `discovery.go:143`). Ensemble's TXT set:

| TXT key | Value | Purpose |
|---|---|---|
| `id` | node id (stable, e.g. UUID/host-derived) | identity; lets a browser skip itself and dedup |
| `name` | friendly name | UI label |
| `cf` | **cluster fingerprint** — short hash of the cluster CA cert (`ConfigDoc.Cluster` CA) | which cluster this node belongs to; `""`/absent ⇒ uninitialized |
| `gid` | the node's **group id** (from `ConfigDoc.Groups[].memberNodeIDs`) | UI grouping; a node is in exactly one group (§5) |
| `init` | `1` initialized, `0` uninitialized | adoption gate (see §2.4) |
| `ctrl` | control port (mTLS HTTP API) | reach a non-member's `/api/v1` for adoption without it being a gossip peer |
| `clk` | clock UDP port | per-group clock sync endpoint |
| `aud` | audio UDP port | audio wire endpoint |
| `wp` | web UI port | browser deep-link |

The **memberlist gossip port** is the SRV/A record port (the `Register`
positional arg in mpvsync), *not* a TXT field — matching mpvsync, where the
advertised port is the gossip port and `wp` rides TXT (`discovery.go:31` doc:
*"The advertised port is the memberlist gossip port, and the TXT records carry
the group and node id."*).

> **Spine note (D7 says "LAN broadcast / mDNS").** The reused mpvsync code
> implements *mDNS only* (zeroconf); there is no separate raw UDP broadcast
> beacon for discovery. Ensemble inherits this: "LAN broadcast" is realized
> *through* mDNS multicast (224.0.0.251:5353), plus memberlist's own UDP gossip
> probing once joined. A dedicated broadcast fallback is a possible future
> addition for environments that block mDNS, but it is **not** in the reused
> spine and is out of scope for this section. Flagged as a minor spine wording
> inconsistency in the final note.

### 2.3 Browse: two modes

mpvsync exposes two browse functions, both reused:

```go
// Seed-only browse: filter to OUR cluster, return memberlist join addrs,
// exclude self. (mpvsync discovery.go:55 Browse — keyed by group; Ensemble
// keys by cluster fingerprint cf.)
func Browse(ctx context.Context, clusterFP, selfID string) ([]string, error)

// Survey browse: surface EVERY advertised node (all clusters, uninitialized
// included) for the UI's device classification. (mpvsync discovery.go:106
// BrowseAll, returning []DiscoveredNode.)
func BrowseAll(ctx context.Context) ([]DiscoveredNode, error)

type DiscoveredNode struct {
    NodeID      string
    Name        string
    ClusterFP   string // TXT "cf" — "" means uninitialized
    GroupID     string // TXT "gid"
    Initialized bool   // TXT "init" == "1"
    Addr        string // first IPv4
    Port        int    // memberlist gossip port
    ControlPort int    // TXT "ctrl" — mTLS API
    ClockPort   int    // TXT "clk" — clock UDP
    AudioPort   int    // TXT "aud" — audio UDP
    WebPort     int    // TXT "wp"
}
```

- **`Browse`** drives the join path. In mpvsync it filters
  `txt["group"] != group` (`discovery.go:76`); Ensemble filters
  `txt["cf"] != clusterFP` so a node only seeds joins with peers of *its own
  cluster*. Its result feeds `mem.Join(found)` exactly as mpvsync's run loop does
  (`internal/node/node.go:804-805`).
- **`BrowseAll`** drives the UI. It surfaces foreign clusters and not-yet-joined
  peers and is **meant to be cached** — mpvsync's loop browses on a 5 s
  `rebrowse` ticker and stores the result (`node.go:705,793-800
  n.setDiscovered(all)`); the web layer reads the cache and **never browses per
  WS tick** (mpvsync `node.go:1918` comment). Ensemble keeps this discipline.

### 2.4 Distinguishing an **uninitialized** node (the adoption hook)

An uninitialized node has no cluster CA, no signed cert, no group. It still
announces over mDNS so the UI can find and adopt it. It is distinguished by the
TXT pair **`init=0`** and **empty `cf`** (mpvsync's analog is *"An empty group
marks an unconfigured node (discovered state)"* — `discovery.go:31` doc, where
`group=""`).

The UI classifies every discovered/known node into one of **five** states,
extending mpvsync's 4-state `buildDevices` (`internal/web/api_cluster.go:34`):

| State | Source input | Condition | UI affordance |
|---|---|---|---|
| `present` | `Members()` (alive gossip peers) | same cluster, alive | live controls |
| `offline` | adopted set / peer cache | known member, not currently alive | shown greyed, last-seen |
| `uninitialized` | `BrowseAll` | `init=0` / `cf==""` | **"Adopt"** → setup, [03](./03-adoption-takeover-security-pki.md) |
| `foreign` | `BrowseAll` | `cf != ""` and `cf != ourCF` | **"Takeover"** (forced re-issue), [03](./03-adoption-takeover-security-pki.md) |
| `discovered` | `BrowseAll` | same `cf`, advertised but not yet a gossip member | transient; should converge to `present` |

Precedence mirrors mpvsync (`present > offline > discovered/foreign`,
`api_cluster.go:60`), with `uninitialized` slotting beside `discovered`/`foreign`
as a survey-only state. The adoption action itself (PIN exchange, CSR signing,
pushing the CA + cert + gossip key) is entirely [03](./03-adoption-takeover-security-pki.md);
discovery's job ends at surfacing `init=0`.

---

## 3. Membership — memberlist SWIM gossip (D8)

### 3.1 Configuration

Ensemble wraps `hashicorp/memberlist`; the wrapper (`Config` fields we set, the
constructor, and the `delegate`) is fully specified in Appendix **A.14**. The wrapper type
and its constructor are reused, renamed only where the spine renames packages:

```go
// internal/cluster (reuses mpvsync internal/cluster/membership.go)
type Meta struct {                 // gossiped via memberlist Node.Meta
    NodeID      string `json:"id"`
    Name        string `json:"name"`
    GroupID     string `json:"gid"`  // mpvsync: Group
    ClusterFP   string `json:"cf"`   // NEW: which cluster (CA fingerprint)
    ControlPort int    `json:"ctrl"` // mTLS API (mpvsync: WebPort)
    ClockPort   int    `json:"clk"`  // clock UDP
    AudioPort   int    `json:"aud"`  // audio UDP
    WebPort     int    `json:"wp"`
}

type Config struct {
    NodeID      string
    Name        string
    GroupID     string
    ClusterFP   string
    BindAddr    string   // default "0.0.0.0"
    BindPort    int      // memberlist gossip port (required, must be reachable)
    ControlPort int
    ClockPort   int
    AudioPort   int
    WebPort     int
    SecretKey   []byte        // symmetric gossip key — gossipKey (32 random bytes) from ClusterSecrets (see §3.2)
    Seeds       []string      // explicit join addresses (host or host:port)
    LogOutput   io.Writer     // nil => io.Discard
    State       *state.Store  // shared ConfigDoc store; bridged into anti-entropy
}

func New(cfg Config) (*Membership, error)
func (m *Membership) Members() []Member            // alive peers incl. self, decoded Meta
func (m *Membership) Join(addrs []string) (int, error)
func (m *Membership) Changed() <-chan struct{}     // coalesced membership-change signal
func (m *Membership) NumMembers() int
func (m *Membership) Leave() error                 // graceful Leave + Shutdown
func (m *Membership) UpdateName(name string) error // re-broadcasts renamed Meta
```

Key configuration choices (the full `Config`-fields-we-set surface is in A.14):

- **Base profile `memberlist.DefaultLANConfig()`** — LAN timings (sub-second
  probe, fast convergence), appropriate for a single L2 segment.
- **`mlc.Name = cfg.NodeID`** — memberlist node name must be unique; the stable
  node id guarantees that (`membership.go:124`). This is the *same* id the
  election orders on, so memberlist identity and election identity never diverge.
- **`BindPort == AdvertisePort == cfg.BindPort`** — the gossip port is also the
  port announced over mDNS, so a peer's `DiscoveredNode.Port` is directly
  `Join`-able.
- **Delegate** (`mlc.Delegate = del`) supplies `Meta` bytes and bridges the
  ConfigDoc into anti-entropy (§4); its method set is specified in A.14.
- **Events** coalesced into one `changed` signal (`membership.go:150-157`): a
  receiver re-reads `Members()` on each tick rather than processing raw events.

### 3.2 Gossip encryption key

mpvsync derives the symmetric gossip `SecretKey` from the **group password**
(`AdoptedSet.Secret`, `adopted.go:50`) via HKDF. Ensemble changes the *source* of
the key to fit the PKI model (D9/D10): the gossip key is **`gossipKey` = 32
random bytes**, generated once for the cluster and held in `ClusterSecrets`
(plaintext-replicated per D18), distributed during adoption alongside the CA — it
is **not** HKDF-derived from a cluster secret. Cluster
membership in the gossip layer is thus gated by the same trust root as mTLS. The
mechanics of `Rekey` are reused unchanged: a node can rotate the primary key at
runtime (`membership.go:180 Rekey`) **only if it started encrypted**; an
open→encrypted transition needs a coordinated restart. Key distribution and
rotation policy are [03](./03-adoption-takeover-security-pki.md); here we only
note that the gossip plane is encrypted and keyed per-cluster.

### 3.3 The peer cache

Fully specified in Appendix **A.14** (`PeerStore`/`peers.json` contract). `PeerStore` is a
best-effort, always-on cache of previously-seen peers **of our own cluster**,
persisted to `<data>/peers.json` (mode 0644, non-secret — A.14). It
serves two jobs:

1. **Faster rejoin.** On startup, `JoinSeeds()` (A.14) returns the
   last-known gossip addresses to seed `memberlist.Join` *before* mDNS has a
   chance to answer, so a configured cluster re-forms even if mDNS is slow or
   blocked. mpvsync dedups these against explicit seeds (`node.go:479
   dedupSeeds`); Ensemble does the same.
2. **Immediate "offline" presence.** After a reboot a known-but-absent peer shows
   as `offline` immediately (seeded via `SeedFromPeers`, A.14),
   rather than vanishing until rediscovered.

`Upsert(members)` is called on every membership change to refresh
name/addr/last-seen (`node.go:787-789`). `Clear()` (A.14) wipes the
cache when the node **leaves** its cluster (forget / re-group), so stale peers of
the abandoned cluster never seed a rejoin — important for the takeover/forget
flows in [03](./03-adoption-takeover-security-pki.md).

### 3.4 Join / leave / suspect timing

memberlist's SWIM gives three liveness phases. The defaults below come from
`DefaultLANConfig()` (the profile mpvsync and Ensemble use); they are stated for
reference, not redefined by Ensemble.

| Phase | Trigger | Effect | Approx. LAN timing |
|---|---|---|---|
| **Join** | `memberlist.Join(seeds)` (startup seeds, mDNS browse, peer cache) | full state sync with a seed; node becomes `alive`; `Changed()` fires | one round-trip; election runs on next `Members()` |
| **Probe** | periodic SWIM probe | direct ping; on no-ack, indirect ping via _k_ peers | `ProbeInterval` ≈ 1 s |
| **Suspect** | probe + indirect probes all fail | node marked **suspect**, gossiped; refutable | `SuspicionMult × log(N) × ProbeInterval` (≈ 4–5 s small N) |
| **Dead** | suspicion not refuted before timeout | node marked **dead**, removed from `Members()`; `Changed()` fires → re-election | end of suspicion window |
| **Leave** | graceful `Leave()` → `ml.Leave(0)` + `Shutdown()` (`membership.go:247`) | broadcasts intent to leave; peers remove immediately (no suspicion wait) | one gossip round |

The crucial property for the election (§4): **memberlist's suspicion mechanism
debounces membership**, so a single momentary packet loss does *not* eject a
node and does *not* cause master flap. mpvsync's election doc relies on exactly
this — *"memberlist's own failure detection debounces membership, so a single
elected master does not flap under normal churn"* (`election.go:18`).

### 3.5 Message & timer table (membership + discovery)

| Mechanism | Transport | Direction | Period / trigger | Carries |
|---|---|---|---|---|
| mDNS announce | UDP multicast 224.0.0.251:5353 | self → LAN | on (re)register; refresh per zeroconf | `id, name, cf, gid, init, ctrl, clk, aud, wp`; gossip port in SRV |
| mDNS browse (`Browse`) | UDP multicast | self → LAN, replies in | `rebrowse` ticker 5 s (mpvsync `node.go:705`), 1 s ctx | same-cluster seeds (`ip:port`) |
| mDNS survey (`BrowseAll`) | UDP multicast | self → LAN, replies in | 5 s ticker, cached | all `DiscoveredNode`s for UI |
| SWIM probe | memberlist UDP | self → 1 peer (+ k indirect) | `ProbeInterval` ≈ 1 s | liveness ack; piggybacked gossip |
| Gossip (state sync) | memberlist UDP | self → random peers | `GossipInterval` (sub-second) | member alive/suspect/dead deltas, `Meta` |
| Anti-entropy push/pull | memberlist TCP | self ↔ 1 peer | `PushPullInterval` (~30 s LAN) | **full ConfigDoc** via delegate (§4) |
| Membership-change handler | in-proc | `Changed()` → loop | on every join/leave/suspect→dead | re-run `election.Update`, `peers.Upsert` |
| `membershipTick` | in-proc | timer | 2 s (mpvsync `node.go:701`) | re-run `applyRole` (safety re-eval) |

---

## 4. ConfigDoc replication over gossip (LWW)

### 4.1 The bridge

The replicated `ConfigDoc` (§6.5) rides memberlist's **push/pull anti-entropy**
via the same delegate seam mpvsync uses for its playlist doc. The delegate
(specified in A.14) implements:

```go
func (d *delegate) LocalState(bool) []byte        // ship our doc + our id
func (d *delegate) MergeRemoteState(buf []byte, _ bool) // fold a peer's doc in (LWW)
```

These call straight into the `state.Store` (delegate wiring in A.14):
`LocalState → state.MarshalGossip()`, `MergeRemoteState → state.MergeGossip(buf)`.
The delegate deliberately leaves `NotifyMsg`/`GetBroadcasts` unused — the periodic
state push/pull is sufficient for a doc that changes rarely and only needs
eventual convergence (A.14). Ensemble keeps this: the
`ConfigDoc` is human-edited and infrequent, exactly the workload LWW anti-entropy
suits.

### 4.2 The merge algorithm (reused, applied to ConfigDoc)

The convergence rule is mpvsync's `state.Store.Merge` (`state.go:145`), reused
**unchanged in logic**, operating on `ConfigDoc.Version` / `ConfigDoc.UpdatedBy`
(§6.5) instead of mpvsync's `Doc.Version` / sender id:

```go
// Last-writer-wins by Version; node id breaks a same-version tie deterministically.
func (s *Store) Merge(remote ConfigDoc, remoteID string) {
    switch {
    case remote.Version > s.doc.Version:                                   take = true
    case remote.Version == s.doc.Version && remote.Version != 0 &&
         remoteID > s.selfID:                                              take = true
    }
    // ... if take: adopt remote, persist, signal Changed
}
```

Properties, all inherited:

- **Deterministic convergence.** Every replica that sees the same pair of docs
  makes the same choice (higher `Version` wins; equal `Version` → higher
  `UpdatedBy`/node id wins). No Raft, no vector clocks — *"acceptable for
  Phase 5 where edits are rare and human-driven"* (`state.go:9-11`).
- **Optimistic concurrency on writes.** `Apply` (`state.go:123`) requires the
  caller's `Version` to equal the current one, else returns `ErrConflict`. This
  is the server side of the spine's `If-Match: <version>` → `409` convention
  (README §6.6); the UI refetches and retries.
- **Envelope carries sender id.** `MarshalGossip` wraps the doc with the sender's
  node id (`state.go:174 gossipState`) because the raw anti-entropy stream has no
  identity of its own; `MergeGossip` unwraps it to apply the tiebreak.
- **Persistence is best-effort.** `Load` seeds from `<data>/config.json` on
  startup; every accepted `Apply`/`Merge` re-persists (atomic temp+rename).
  A write error is logged and **never** fails the merge (`state.go:205 save`).
  Gossip reconciles the cluster's highest version on rejoin regardless.
- **Replicated to all full nodes** (D8). Every full node runs a `state.Store`
  bridged into its delegate, so the doc converges across the whole cluster — not
  a subset. Dumb nodes (D15) would receive a reduced view via the control plane,
  out of scope here.

### 4.3 Why membership and ConfigDoc are *separate* planes

memberlist `Meta` (id/name/group/ports) is **ephemeral liveness state** — it
exists only while a node is alive and is re-derived on every join. The
`ConfigDoc` is **durable cluster intent** (who *should* be a member, group
assignments, certs, audio config). Group **membership-of-a-group** is read from
`ConfigDoc.Groups[].memberNodeIDs` (§5), while group **liveness** (which members
are *currently up*) comes from `Members()`. The election (§4-below, "per-group")
intersects the two: it ranks the *alive* `Members()` that the *ConfigDoc* assigns
to that group.

---

## 5. Per-group master election

### 5.1 The rule

Ensemble elects, **per group**, the master = **the lowest stable node id among
that group's currently-alive members**. This is mpvsync's `Election.Update`
(`election.go:35`) verbatim — a pure, stateless function of the member set —
applied once per group instead of once per cluster.

```go
// internal/cluster (reuses mpvsync internal/cluster/election.go)
type Election struct { /* selfID, masterID, generation */ }

func NewElection(selfID string) *Election
func (e *Election) Update(members []Member) (masterID string, changed bool) // lowest id
func (e *Election) Master() string
func (e *Election) Generation() uint64  // ++ on every master change
func (e *Election) IsMaster() bool
```

The election is **intentionally stateless** (`election.go:11-15`): the master is
a pure function of the current member set, so every node that sees the same
membership computes the same master. mpvsync explicitly rejected incumbency
"stickiness" because *"local stickiness is inconsistent across nodes … a split
brain. Consistency wins; the cost is a master change … whenever a lower-id node
appears, which only happens on a topology change"* (`election.go:11-19`). Ensemble
keeps the stateless rule for the same reason.

### 5.2 How "per-group" differs from mpvsync's single election

mpvsync runs **one** `Election` for the whole node (it has a single sync group).
Ensemble has any number of independent groups (D-glossary: *"A node is in exactly
one group"*), so:

```go
// internal/group / internal/cluster glue: one Election per group id.
type GroupElections struct {
    mu sync.Mutex
    e  map[string]*cluster.Election // keyed by GroupRecord.ID
}

// Recompute runs each group's election over the subset of alive members that the
// ConfigDoc assigns to that group, returning the masters that CHANGED.
func (g *GroupElections) Recompute(doc ConfigDoc, alive []cluster.Member) (changed map[string]Outcome)

type Outcome struct {
    MasterID   string
    Generation uint64
    IsSelf     bool
}
```

`Recompute` does, for each `GroupRecord`:

1. Build the candidate set = `alive` members whose id ∈ `GroupRecord.memberNodeIDs`
   **and** (for self-correctness) whose gossip `Meta.GroupID` matches.
2. Call that group's `Election.Update(candidates)` — identical lowest-id logic.
3. If `changed`, read `Generation()` and emit an `Outcome` for the group.

Because the rule is the same stateless function, **all nodes converge on the same
master for every group** once they share the same `(ConfigDoc, alive-set)` view —
which gossip and anti-entropy guarantee eventually.

This replaces mpvsync's single-master `applyRole` (`node.go:673`). In Ensemble a
node may simultaneously be **master of its own group** and not involved in any
other group (it is only ever in one group), so per-node there is at most one
"am I master?" answer — but the *cluster* runs N concurrent elections, one per
group, each with its own `Generation` counter.

### 5.3 The generation counter & rebaseline

`Update` bumps `generation` on every master change (`election.go:51`). Downstream
consumers key off it:

- **Clock re-baseline.** A master change means a new clock reference for that
  group, so [04](./04-clock-and-groups.md)'s per-group clock must re-baseline.
  mpvsync threads the generation into the role goroutine (`node.go:692-694
  gen := election.Generation(); go n.runMaster(rctx, gen)`); Ensemble threads each
  group's generation into that group's clock + stream origin.
- **Stream generation.** The audio wire header carries `streamGen` (§6.4), reset
  on media/seek change. A master change is also a stream-origin change, so the
  group engine bumps `streamGen` on a new election generation — receivers detect
  the discontinuity and reset their jitter buffer (see
  [05](./05-audio-streaming-protocol.md)).

### 5.4 Election state diagram (per group)

```
                         ┌──────────────────────────────────────────────┐
                         │  membership change (Changed) or 2 s tick      │
                         ▼                                                │
   ┌───────────┐   no alive members in group    ┌──────────────┐         │
   │  NO-MASTER│◄──────────────────────────────►│  group empty  │        │
   │ master="" │                                 └──────────────┘        │
   └─────┬─────┘                                                          │
         │ ≥1 alive member assigned to group                              │
         ▼                                                                │
   ┌──────────────────────────────┐   lower-id member joins group        │
   │      MASTER ELECTED           │  ───────────────────────────────┐    │
   │  master = min(alive ∩ group)  │                                  │    │
   │  Generation = G               │  current master dies/leaves ─────┤    │
   └───────┬───────────────────────┘                                  │    │
           │ self == master                                           │    │
           ▼                                                          ▼    │
   ┌──────────────┐                                       ┌────────────────┐
   │  I AM MASTER │   self no longer lowest id            │ RE-ELECT       │
   │ run clock+   │ ────────────────────────────────────►│ master=new min │
   │ stream origin│                                       │ Generation++   │
   └──────┬───────┘ ◄──── new lowest id == self ──────────└───────┬────────┘
          │                                                       │
          │ self != master                                        │
          ▼                                                       ▼
   ┌──────────────┐                                       (back to MASTER
   │ I AM FOLLOWER│                                        ELECTED with the
   │ clock follow │                                        new master + gen)
   │ + stream recv│
   └──────────────┘
```

Every transition that changes `master` increments `Generation`; `master==""`
(NO-MASTER) is the bootstrap/empty state where the node reports role `starting`
(mpvsync `node.go:686-688`).

---

## 6. Failover & split-brain

### 6.1 Master dies

1. SWIM detects the master unreachable → `suspect` → `dead` after the suspicion
   window (§3.4), emitting `Changed()`.
2. Every surviving member re-runs `Recompute`; the group's `Election.Update`
   recomputes `min(alive ∩ group)`. The next-lowest live id becomes master and
   `Generation` increments on each node (deterministically the *same* new master,
   because the rule is a pure function of the agreed member set).
3. The new master starts its clock server + stream origin with the new
   `streamGen`; followers re-point their clock follower / stream receiver and
   reset jitter buffers (handoff detail in [04](./04-clock-and-groups.md) /
   [05](./05-audio-streaming-protocol.md)).

Worst-case audible gap ≈ suspicion window + clock re-baseline + buffer refill.
This is the per-group analog of mpvsync's master-death failover; here it affects
only the dead master's group, leaving other groups undisturbed.

### 6.2 Cluster partitions (split-brain by topology)

If the LAN partitions, each side sees only its own subset of alive members.
Within a group split across the partition:

- Each side independently elects `min(alive ∩ group)` *on its side*. The two
  sides may therefore run **two masters for the same group** — one per partition.
  This is acceptable and unavoidable without a quorum protocol (which the spine
  deliberately does not adopt — LWW, no Raft). Each side keeps playing internally
  in sync; the two sides may drift apart relative to each other.
- The `ConfigDoc` keeps converging *within* each partition via anti-entropy. If
  edits happen on both sides, both bump `Version`; on heal, LWW reconciles to the
  higher `Version` (id-tiebreak on equality) — see §4.2.

**On heal:** memberlist re-merges the member sets (one gossip round once
connectivity returns; peer-cache seeds and mDNS accelerate rediscovery). Each
group's stateless election now sees the *union* of alive members and converges to
the single global `min` id. The losing-side master observes it is no longer the
lowest id, demotes to follower, and `Generation` increments — convergence to one
master per group is automatic, no human action.

### 6.3 Two nodes both think they're master

This can only arise transiently (e.g. mid-partition, or a brief window where two
nodes have not yet agreed on membership). Convergence is structural:

- The election is a deterministic function of the alive set; the moment both
  nodes share the same `Members()` view (guaranteed by gossip convergence), they
  compute the *same* `min` id, and at most one of them is it.
- The higher-id node demotes itself on the next `Recompute`. Because demotion is
  driven by `IsMaster()` flipping to false (mpvsync `election.go:72`), the role
  goroutine is cancelled and the node becomes a follower of the survivor.
- The realtime planes self-heal too: followers only accept clock/audio from the
  *currently elected* master's address (and the **allowlist**,
  [03](./03-adoption-takeover-security-pki.md), drops packets from non-members),
  so a deposed master's lingering packets are ignored.

mpvsync's design note captures the principle exactly: a sticky/incumbent design
*causes* split brain; the stateless lowest-id rule *resolves* it as soon as
membership agrees (`election.go:11-19`).

### 6.4 Failover timer table

| Event | Detector | Time to detect | Recovery action | Generation bump |
|---|---|---|---|---|
| Master crash (no graceful leave) | SWIM suspect→dead | ≈ suspicion window (4–5 s, small N) | re-elect next-lowest; new clock+origin | yes |
| Master graceful leave | `Leave()` broadcast | ~1 gossip round (sub-second) | re-elect immediately (no suspicion wait) | yes |
| Partition forms | each side's SWIM | suspicion window | each side elects its own master | yes (each side) |
| Partition heals | memberlist re-merge | ~1 gossip round + rediscovery | converge to global min; losers demote | yes (losing side) |
| Lower-id node joins group | `Changed()` | join round-trip | incumbent demotes, new node masters | yes |
| Membership safety re-eval | `membershipTick` | ≤ 2 s | idempotent `Recompute` | only if min changed |

---

## 7. How groups map onto membership

A node is in **exactly one group** (glossary, §2 of the spine). Two facts must
agree, and the system is designed so they converge:

1. **Intent** — `ConfigDoc.Groups[].memberNodeIDs` lists which node ids belong to
   each group. This is the durable, replicated assignment (edited via the UI,
   merged via §4 LWW).
2. **Advertised reality** — each node gossips its own `Meta.GroupID` (§3.1) and
   announces `gid` over mDNS (§2.2). On startup / after a group change a node sets
   its own `GroupID` from the `ConfigDoc` and re-registers mDNS (mpvsync
   re-registers discovery on group change — `node.go:639 cluster.Register`).

`GroupElections.Recompute` (§5.2) intersects the two: a node is a candidate for a
group's master iff its id is in that group's `memberNodeIDs` **and** it is
currently alive in `Members()`. The intersection makes the system robust to lag:

| ConfigDoc says | Node advertises | Alive? | Treated as |
|---|---|---|---|
| in group G | `gid=G` | yes | group-G member & master candidate |
| in group G | `gid=G` | no | group-G member, `offline` (no candidate) |
| in group G | `gid=H` (stale, mid-move) | yes | excluded from G until it re-registers as G |
| not in any group | `init=0` | — | `uninitialized`, adoption candidate (§2.4) |

Moving a node between groups is therefore: edit `ConfigDoc.Groups`
(`Apply`/`If-Match`) → gossip converges → the node reads its new `GroupID`,
re-registers mDNS, clears its peer cache for the old cluster scope if leaving
(`PeerStore.Clear`, §3.3) → both the old and new group's elections `Recompute` and
re-baseline. A "solo" group is simply a `GroupRecord` with one
`memberNodeIDs` entry; its election trivially elects that node as its own master.

**Group-move transient (orphaned render).** The instant a node observes its
**own** `GroupID` change, it **orphans — it plays silence** and stops rendering
the old group's audio immediately; it does **not** keep playing the previous
group's stream while the move converges. It begins rendering again **only once it
has the NEW group's master, clock baseline, and stream** (master elected for the
new group, clock follower re-pointed and locked, stream receiver primed). Until
all three are in hand the node stays silent rather than emitting stale or
ambiguous audio. (The clock/stream re-acquisition for the new group is detailed
in [04](./04-clock-and-groups.md).)

The derived **allowlist** for realtime traffic (§6.5) is computed from
`Nodes[].Addrs` ∪ current group membership; its derivation and enforcement are
[03](./03-adoption-takeover-security-pki.md) and
[07](./07-config-and-replication.md). Discovery/membership only *supplies* the
inputs (alive member addresses, ConfigDoc node records).

---

## 8. Summary of reused symbols (citation index)

| Symbol | File:line (mpvsync) | Reuse in Ensemble |
|---|---|---|
| `Register`, `Browse`, `BrowseAll`, `txtRecords`, `nodeFromTXT`, `DiscoveredNode` | `internal/cluster/discovery.go:31,55,106,143,149,94` | discovery package; +`init`/`cf` TXT, +`uninitialized` state |
| `Membership`, `New`, `Meta`, `Member`, `Members`, `Changed`, `Join`, `Leave`, `UpdateName`, `Rekey`, `delegate` | Appendix **A.14** (wrapper + delegate spec) | membership package; +`ClusterFP` in Meta, `gossipKey` from `ClusterSecrets` |
| `Election`, `Update`, `Master`, `Generation`, `IsMaster` | `internal/cluster/election.go:20,35,58,65,72` | one instance **per group** (`GroupElections`) |
| `PeerStore`, `Upsert`, `JoinSeeds`, `Clear`, `Snapshot` | Appendix **A.14** (`PeerStore`/`peers.json` contract) | full contract in A.14 |
| `state.Store`, `Merge`, `Apply`, `MarshalGossip`, `MergeGossip`, `Load`, `ErrConflict` | `internal/state/state.go:70,145,123,181,194,97,59` | operates on `ConfigDoc` (§6.5); same LWW logic |
| `buildDevices` 4-state classifier | `internal/web/api_cluster.go:34` | extended to 5 states (+`uninitialized`) |
| run-loop wiring (`applyRole`, `rebrowse` 5 s, `membershipTick` 2 s) | `internal/node/node.go:673,705,701,804` | per-group `Recompute`; same tickers |
