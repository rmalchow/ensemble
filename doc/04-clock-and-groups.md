# 04 — Clock & Groups

> **Scope.** This document specifies, for the **Ensemble** system, (1) **per-group clock
> synchronization** (the NTP-style four-timestamp exchange reused from `mpvsync`'s
> `internal/clock`), (2) the **group engine** — the per-node role state machine (incl. the
> sink-less / `Render=false` "master, no local render" variant, D17), (3) **profile
> negotiation** (choosing a group's codec/FEC/rate from the `Render=true` listeners' effective
> `Capabilities`, D16/§6.5), (4) the
> **timeline math** that turns the shared clock into a synchronized *stream sample index*,
> and (5) the **group lifecycle** (create / add / remove / move / dissolve / solo).
>
> This document **elaborates** the spine ([README.md](./README.md)); it does not redefine
> anything in §3 (locked decisions) or §6 (shared contracts). It uses the exact names
> `ClockSource`, `Timeline`, `ConfigDoc`, `NodeRecord`, `GroupRecord`, `streamGen`,
> `sampleIndex`, `masterMono`, and the `Capabilities` struct (`Caps`) with its fields
> `Render`, `Sinks`, `EncodeCodecs`, `DecodeCodecs`, `FEC`, `MaxRate` (§6.5), etc., from §6.
>
> **Cross-references**
> - **Election & membership** — who becomes a group's master, failover signalling, and the
>   per-group election driving this engine: [02-cluster-discovery-membership.md](./02-cluster-discovery-membership.md).
> - **Streaming protocol** — the actual decode→encode→FEC→unicast path the `master` state
>   starts and the receive/recover path the `follower` state starts:
>   [05-audio-streaming-protocol.md](./05-audio-streaming-protocol.md).
> - **Output scheduling & drift** — how `Timeline.NowSample()` is consumed by the render
>   loop and the content-domain PI drift loop: [06-audio-output-scheduling.md](./06-audio-output-scheduling.md).
> - **Config & replication** — `GroupRecord.profile` publication, LWW merge, allowlist
>   derivation: [07-config-and-replication.md](./07-config-and-replication.md).

---

## 4.0 Roles, planes, and what this document owns

Per spine §4 each full node runs the whole stack; its **role within a group is dynamic**.
The `internal/group` package is the *engine* that, for the local node, decides which role
to occupy and which sub-systems to run in that role. It is driven by exactly two inputs:

1. The **per-group election result** from `internal/cluster` ([02](./02-cluster-discovery-membership.md)) —
   "for group `G`, the master is node `M`".
2. The replicated **`ConfigDoc.Groups`** membership and `profile` from `internal/state`
   ([07](./07-config-and-replication.md)) — "node `self` is a member of group `G`".

It owns three runtime artifacts per group it participates in:

| Artifact | Reused/new | Where | Used by |
|---|---|---|---|
| `clock.Server` (master only) | reuse `internal/clock` | clock plane, UDP | followers' `clock.Follower` |
| `clock.Follower` (follower only) | reuse `internal/clock` | clock plane, UDP | this node's `Timeline` projection |
| `Timeline` (every member) | new `internal/group`, pattern from `internal/sync` | in-proc | render loop ([06](./06-audio-output-scheduling.md)) |

The three traffic tiers (spine §4) map as: **control plane** carries election + ConfigDoc
(mTLS, [02]/[03]/[07]); **clock plane** carries the four-timestamp exchange of §4.1
(UDP, source-allowlisted, unauthenticated); **audio plane** carries the §6.4 chunks
([05]). This document spans the clock plane and the in-process timeline; the audio plane
is [05]'s.

---

## 4.1 Per-group clock synchronization

### 4.1.1 Reused mechanism (mpvsync `internal/clock`)

Ensemble reuses the `mpvsync` clock package **unchanged** (spine D6, §6.2). The exchange
is the classic NTP four-timestamp handshake. A follower stamps **t1** at send; the master
stamps **t2** at receive and **t3** at send (echoing t1); the follower stamps **t4** at
reply receipt. From `clock.computeSample`:

```
offset = ((t2 - t1) + (t3 - t4)) / 2          // master_mono - follower_mono
delay  = (t4 - t1) - (t3 - t2)                // round-trip, minus master service time
```

```
 follower mono                                  master mono
   t1 ───────────────────req(seq,t1)──────────────► t2     (stamp receive ASAP)
     \                                              /│
      \                                            / │  master service time = t3 - t2
       \                                          /  │
   t4 ◄──────────────reply(seq,t1,t2,t3)──────── t3  ▼     (stamp send as late as possible)
```

The wire packet is the **40-byte mpvsync format**, magic `"MPVC"`, reused verbatim
(spine §6.4: "Clock packets reuse mpvsync's 40-byte format unchanged"). It is **not** the
`"ESND"` audio header of §6.4 — the two planes are distinct sockets and distinct framings.

**Offset semantics (the contract every consumer relies on).** Per `clock.go` and §6.2:

> **`master_mono ≈ follower_mono + Offset`**

`Offset` is `master_mono − follower_mono`. `clock.NowMono()` is *per-process* nanoseconds
since a monotonic epoch (`procEpoch`), so the quantity is **immune to wall-clock steps**;
nodes never need synchronized wall time, only a stable relative offset. To convert a local
instant to the master's frame: `master_mono = clock.NowMono() + Offset` (this is exactly
`Follower.MasterNow()`); to convert a master instant to local: `follower_mono =
master_mono − Offset`.

**Filtering.** `clock.Estimator` keeps a sliding **window** of `Sample{Offset, Delay}`,
applies the **NTP minimum-delay filter** (the lowest-`Delay` sample in the window is the
most trustworthy — high delay means one-directional queuing that biases the offset
estimate), then **EWMAs** that best offset so the applied value *slews, never steps*:

```
offset ← offset + alpha · (best.Offset − offset)
```

The canonical clock parameters live in **Appendix A.12** (single source of truth): wired
`window = 8, alpha = 0.15`; WiFi `window = 16, alpha = 0.10`; ping `interval = 1 s`
(`500 ms` during initial lock); per-ping RPC `timeout = 200 ms` (`NewEstimator`,
`NewFollower`). Use A.12's values rather than restating them here.

**Quality metric.** `MinDelay()` (the smallest round-trip in the window) is the published
sync-quality proxy, surfaced through the `ClockSource` contract. The group engine and UI
treat it as the health signal (see §4.2 `orphan` state and §4.6).

### 4.1.2 One server per master; each follower pings *its own* master

Where `mpvsync` has a single cluster-wide clock, **Ensemble runs one clock server per
group, on that group's master**, and each follower pings *its* master only:

```
   group A (master N1)                 group B (master N3)
   ┌──────────────────┐                ┌──────────────────┐
   │ N1: clock.Server  │◄── N2 ping     │ N3: clock.Server  │◄── N4 ping
   │   :CLOCKPORT      │◄── (only A's)  │   :CLOCKPORT      │◄── (only B's)
   └──────────────────┘                └──────────────────┘
      N2.Follower──► N1                    N4.Follower──► N3
```

- The **master** runs exactly one `clock.Listen(addr)` (`clock.Server`); it answers any
  allowlisted requester. A `solo` group's master runs a server too, but no follower pings
  it (harmless; see §4.2).
- Each **follower** runs one `clock.Follower.Run(ctx, masterAddr)` where `masterAddr`
  comes from the master node's `NodeRecord.Addrs` ([07](./07-config-and-replication.md))
  for the group's elected master ([02](./02-cluster-discovery-membership.md)).
- A node that is master of one group and (impossibly, by the §6.2 "exactly one group"
  rule) a member of another does **not** occur: a node is in exactly one group, so it runs
  *either* a server *or* a follower, never both, plus always its own `Timeline`.

The clock socket is **source-allowlisted** (spine §6.5, [03]/[07]): the master's server
accepts requests only from current cluster member IPs; cross-group pings are dropped by IP
even though the wire format is identical. The follower `DialUDP`s a single master, so it
only ever accepts that master's replies (`exchange` rejects foreign/stale `seq`).

### 4.1.3 The `ClockSource` contract

The engine adapts a `clock.Follower` to the spine §6.2 interface; the master adapts a
trivial zero-offset source (it *is* the reference):

```go
// §6.2 — do not redefine.
type ClockSource interface {
    Offset() (d time.Duration, ok bool)
    MinDelay() (time.Duration, bool)
}
```

| Role | `ClockSource` impl | `Offset()` | `MinDelay()` |
|---|---|---|---|
| `master` | self-reference | `0, true` | `0, true` (reference is exact) |
| `follower` | wraps `clock.Follower` | `f.Offset()` (EWMA) | `f.MinDelay()` |
| `orphan` | no estimate yet | `0, false` | `0, false` |

`clock.Follower` already satisfies the shape (`Offset`, `MinDelay`); the master adapter and
orphan zero-value are the only new code.

### 4.1.4 WiFi tuning vs. the wired video case

`mpvsync` targets a **wired** video wall; Ensemble must hold sub-millisecond audio sync
over **WiFi** (spine §1 goal). WiFi changes the *statistics of `Delay`*, not the algorithm:
power-save wakeups, retries, and aggregation produce **fat-tailed, bursty** RTTs with
occasional 10–50 ms spikes. The minimum-delay filter is precisely the right defense, but it
must see enough samples to *contain a clean minimum* and must not over-smooth across a
genuine offset drift (crystal aging/temperature).

The canonical per-environment values are fixed in **Appendix A.12** (the single source of
truth); the table below gives the rationale for each `NewEstimator(window, alpha)` /
`NewFollower(WithInterval, WithTimeout)` knob — see A.12 for the authoritative numbers:

| Knob | A.12 wired | A.12 WiFi | Rationale |
|---|---|---|---|
| `window` | 8 | 16 | Wider window → higher chance a jitter-free minimum sample is present. |
| `alpha` | 0.15 | 0.10 | Slower slew rides over RTT bursts; true offset drifts slowly so low alpha is fine. |
| `interval` | 1 s | 1 s (`500 ms` initial lock) | Enough samples/sec to keep the window spanning only a short wall-time span. |
| `timeout` | 200 ms | 200 ms | Tolerate WiFi retry latency without prematurely discarding a reply. |

These are *parameters to the unchanged code* — no algorithm change. The PI drift loop in
[06](./06-audio-output-scheduling.md) is the *second* line of defense: even a momentarily
noisy offset is absorbed by the jitter buffer + resampler, so the clock layer only has to be
*stable in the mean*, which low-`alpha`/wide-`window` guarantees. The quality gate (§4.2
`orphan`) keys off `MinDelay()` rather than instantaneous `Delay`, so a single WiFi spike
never demotes a synced follower.

---

## 4.2 The group engine — per-node role state machine

`internal/group` runs one **state machine per node** (the node participates in exactly one
group, §6.2). The states:

| State | Meaning | Runs |
|---|---|---|
| `solo` | Group of one; this node is its own master. | local `Timeline` + `clock.Server` (idle) + **stream origin → loopback render** *(only if `Caps.Render`; a sink-less solo node idles — see below)*; no network listeners served data. |
| `master` | Elected master of a multi-member group. | `clock.Server`; **stream origin** (decode→encode→FEC→unicast to each follower, [05]); publishes `GroupRecord.profile` (§4.3); local render **only if `Caps.Render`** — a sink-less master ("master, no local render", below) origins + clocks but does not render. |
| `follower` | Member, not master. | `clock.Follower` (pings master); **stream receiver** (recv→FEC recover→decode→ring, [05]); local render driven by projected `Timeline`. A `Caps.Render=false` member is **not** a listener and so never occupies `follower` — see below. |
| `orphan` | Member of a group but **no usable sync** (no master / `Offset` not ok / `MinDelay` over threshold). | clock follower *attempting* sync; render **muted/held** (no audio rather than wrong audio). Transient, recovery-seeking. |

`solo` vs `master` differ only in whether anyone is listening: `solo` is a `master` with an
empty follower set, so a `solo` node that gains a member is promoted to `master` without
restarting its origin or clock (just begins unicasting to the new follower) — see §4.6.

**Sink-less variant ("master, no local render").** Per spine D17 / §6.5
`Capabilities.Render`, a full node may have `Caps.Render=false` (no usable+enabled audio
sink — e.g. a NAS/docker node). The engine treats render as an **independent sub-system,
gated on `Caps.Render`**, not a separate state:

- In `solo`/`master`, a `Caps.Render=false` node runs the `clock.Server` + **stream origin**
  exactly as a rendering master would (it must be able to *encode* the chosen codec — see
  §4.3), but starts **no local render / no loopback sink**. It still serves followers; it is
  simply not itself a listener. This is the "master, no local render" variant.
- Election is **unaffected**: sink-less nodes are fully **master-eligible** (origin + clock +
  media store + UI need no sink). [02] does not weight render capability. A group whose
  elected master is sink-less is normal — it just produces no local sound.
- A `Caps.Render=false` node is **never** placed in `follower` or `orphan`: those states
  exist to *render* synced audio, which it cannot do. It is master-or-solo within its group.
  (If it is a non-master member of a group, see §4.2.4: it occupies a control-only `member`
  posture, running its clock follower for liveness but no receiver/render.)
- A `solo` sink-less node simply **idles** the origin (it sources nothing useful to itself
  and has no followers); see also the zero-render-member group note in §4.3.4.

### 4.2.1 State diagram

```
                         ┌──────────── added to a group, not elected master ───────────┐
                         │                                                              ▼
        create / solo    │                                            sync acquired  ┌──────────┐
    ●──────────────► ┌────────┐   added to multi-member group   ┌──────────┐◄────────│  orphan  │
                     │  solo  │──────────(I am master)──────────►│  master  │         └──────────┘
                     └────────┘                                  └──────────┘            ▲   │
                         ▲  ▲                                       │   ▲                 │   │ sync lost
                  group  │  │ last other member removed             │   │ won election    │   │ (MinDelay↑ /
                  →1     │  └───────────────────────────────────────┘   │ (master lost,   │   │  no master /
                         │                                              │  promoted)      │   │  Offset !ok)
                         │                  dissolve / removed → new solo group           │   ▼
                         │                                              ┌──────────┐──────┘ ┌──────────┐
                         └──────── removed from group (→own solo) ──────│ follower │◄───────│  orphan  │
                                                                        └──────────┘ sync   └──────────┘
                                                                            │   ▲   acquired
                                                                            └───┘
                                                                       master changed
                                                                       (re-point follower)
```

The diagram shows the **render-capable** path. Two render-orthogonal modifiers (not extra
states) overlay it (D17, §6.5 `Caps.Render`): a `solo`/`master` node with `Caps.Render=false`
runs as "master, no local render" (origin + clock, no sink), and a non-master member with
`Caps.Render=false` sits in the control-only `member` posture (§4.2.4) instead of
`follower`/`orphan`. Toggling `Caps.Render` (T11) starts/stops only the render sub-system.

### 4.2.2 Transitions (triggers and effects)

| # | From → To | Trigger (source) | Effect |
|---|---|---|---|
| T1 | `∅` → `solo` | Node initialized / created as its own group ([02] join, [07] default group). | Start `Timeline` (paused at sample 0), start `clock.Server` (idle), start origin → local render. |
| T2 | `solo` → `master` | A second node added to this group (`ConfigDoc.Groups[g].memberNodeIDs` grows), election names *me* ([02]). | Negotiate profile (§4.3), publish `GroupRecord.profile`, begin unicasting to the new follower; clock server already up — no restart. |
| T3 | `solo`/`master`/`follower` → `follower` | I am a member, **`Caps.Render=true`**, and election names *another* node master ([02]). | Stop origin if I had one; start `clock.Follower.Run(masterAddr)`; start stream receiver; render from projected `Timeline`. (A `Caps.Render=false` member instead takes the control-only `member` posture, §4.2.4 — no receiver/render.) |
| T4 | `follower` → `master` | **Master lost**: election promotes me ([02] failover). | Read the replicated **`GroupRecord.Playing`** (R4); `Timeline.Seed(lastSample, Playing)` for continuity (§4.4.4) — **resume** at the current `sampleIndex` iff `Playing`; stop follower; start origin + (existing) clock server; re-publish/confirm `profile`; bump `streamGen` (§4.3.3). |
| T5 | `follower` → `follower` (re-point) | Master changed to a *different* node (election), I remain follower. | Cancel current `Follower.Run`, start a new one against the new `masterAddr`; receiver reconnects; await new `streamGen`. |
| T6 | `follower` → `orphan` | Sync degraded: `Offset()` `ok=false`, or no elected master, or `MinDelay()` over the quality threshold for N consecutive windows. | Mute/hold render (§4.2.3); keep follower running to re-acquire. |
| T7 | `orphan` → `follower` | Sync re-acquired: `Offset` ok **and** `MinDelay` under threshold. | Resume render from current projection; resync ring against `Timeline`. |
| T8 | `orphan` → `master` | While orphaned, election promotes me (e.g. old master gone). | As T4, but no follower to stop. |
| T9 | `master`/`follower` → `solo` | I am **removed** from the group, or group **dissolved** ([07] membership shrink) → I become my own new group of one. | Tear down follower/receiver or the per-listener origin fan-out; create a fresh solo `Timeline`; see §4.6. |
| T10 | `master` → `solo` | Every *other* member removed (membership →1). | Stop unicasting (no listeners); keep origin + clock server idle; identical engine, empty follower set. |
| T11 | any → same role, **render toggled** | My effective `Caps.Render` flips via ConfigDoc edit ([07] / §6.5) — sink disabled or (re)enabled. | Role/state unchanged; **start or stop the local render sub-system only**. `true→false`: tear down render/receiver, become "master, no local render" if master/solo, or drop to control-only `member` (§4.2.4) if a non-master member. `false→true`: start render; a control-only `member` becomes a `follower` (start receiver). May trigger renegotiation on the master (§4.3.3). |

**Determinism.** The engine is a pure function of `(electionResult, myGroupMembership,
profile, myCaps, clockHealth)` recomputed on every gossip update or election event. It never
*infers* role from packets; the control plane is authoritative ([02]/[07]). This keeps two
nodes from both believing they are master: a split is resolved by election, then the engine
follows. The local **`Caps.Render` flag gates the render sub-system orthogonally to the role**
(T11), and a member's `Caps` (incl. `DecodeCodecs`/`FEC`/`MaxRate`) is a negotiation input on
the master (§4.3).

### 4.2.3 `orphan` policy

`orphan` exists because **silence is better than skew**: an unsynced follower that rendered
the master's chunks at the wrong instant would produce audible echo/comb-filtering against
its synced peers. So `orphan` **holds output** (ramped-down to avoid clicks; [06] owns the
fade) while the `clock.Follower` keeps pinging. Entry/exit is hysteretic on `MinDelay()` (a
high enter threshold, a lower exit threshold) so WiFi spikes (§4.1.4) don't flap the state.
A `solo`/`master` is *never* `orphan` — it is its own reference. A `Caps.Render=false`
member is also *never* `orphan` (it does not render synced audio at all; §4.2.4).

### 4.2.4 Control-only member posture (`Caps.Render=false`, non-master)

A sink-less node (D17, §6.5 `Caps.Render=false`) that is a **non-master member** of a
multi-member group has nothing to render and is not a listener, yet it is still a full
cluster member (media store, UI, control, replication, and a master-election candidate). It
occupies a **control-only `member` posture** rather than `follower`/`orphan`:

- It runs a `clock.Follower` against the group master for **liveness/quality reporting only**
  (so the UI can still show its sync health), but starts **no stream receiver, no decode, no
  ring, no render**.
- It does **not** constrain the group's playback profile (§4.3.2): negotiation looks only at
  `Caps.Render=true` listeners.
- It remains master-eligible; if election promotes it, it enters `master` as the "master, no
  local render" variant (§4.2). If its `Caps.Render` is later enabled it becomes a normal
  `follower` (T11).

---

## 4.3 Profile negotiation

### 4.3.1 What a profile is and who decides

A **profile** (spine glossary; §6.5 `GroupRecord.profile`) is the group's negotiated audio
parameters: **codec** (string enum `"pcm"|"opus"`, §6.3 — `FLAC` is **not** a wire codec; it
is a source-decode-only format, R2), **FEC** (string enum `"none"|"xorParity"|"duplicate"`,
§6.3), and **rate** (canonical sample rate + `FramesPerChunk`, §6.4 default 480 = 10 ms @
48 k). The wire codec set is `{PCM, OPUS}`: **PCM is the mandatory baseline** (no encode,
always works) and Opus is **optional, capability-gated** (low-bandwidth). FEC defaults to
XOR-parity (D4); both codec and FEC are **negotiated per group to the least-capable member** —
where "member", for the purposes of *playback* negotiation, means a **`Caps.Render=true`
listener** (see below). JSON (ConfigDoc + API) carries the string names; the integer wire
ids exist only at the §6.4 wire layer (R4).

**Negotiate against the Render=true listeners only.** The profile governs what listeners must
*decode and play*, so it is negotiated against the **effective `Capabilities` of the group's
`Caps.Render=true` listeners only** (spine D16/D17, §6.5). Control-only / sink-less members
(`Caps.Render=false`, §4.2.4) — including a sink-less *master* — do **not** constrain the
playback profile: they render nothing, so their `DecodeCodecs`/`FEC`/`MaxRate` are irrelevant
to what the listeners can play. Negotiation reads each listener's
`Caps.DecodeCodecs` / `Caps.FEC` / `Caps.MaxRate` (§6.5) and picks the
**least-common-capable across listeners** (the richest choice no listener lacks).

**The master must be able to encode it.** Whoever is master (rendering or sink-less) is the
**stream origin**, so it must be able to *originate* the chosen codec: the negotiated
`Profile.Codec` must be in the master's `Caps.EncodeCodecs` (§6.5). A sink-less NAS/docker
master encodes for its listeners without decoding/rendering locally. (`EncodeCodecs` is the
origin-side capability; `DecodeCodecs` is the listener-side one — §6.5.)

**The master decides.** Negotiation is a pure function over the listeners' `Capabilities`
(plus the master's `EncodeCodecs` constraint); running it on the master avoids races and
gives a single writer for `GroupRecord.profile`. Followers *consume* the published profile;
they never compute their own.

### 4.3.2 Least-common-capable selection

Input: each member's `NodeRecord.Caps Capabilities` (§6.5 — the structured, effective
capability set `{Render, Sinks, EncodeCodecs, DecodeCodecs, FEC, MaxRate}`). The master
first **filters to the `Render=true` listeners**, then intersects their
`DecodeCodecs`/`FEC` and takes the min of their `MaxRate`, picking the **most-preferred
option every listener supports** ("least-common-capable" = the richest choice no listener
lacks). The result is finally constrained to the master's own `EncodeCodecs` (the origin must
produce it, §4.3.1):

```go
// internal/group — master side. Caps come from ConfigDoc.NodeRecord.Caps (§6.5 Capabilities).
type Profile struct {
    Codec          string // §6.3 string enum: "pcm"|"opus" (FLAC is source-only, not a wire codec)
    FEC            string // §6.3 string enum: "none"|"xorParity"|"duplicate"
    Rate           int    // canonical Hz, e.g. 48000
    FramesPerChunk int    // §6.4, default 480
}
// Negotiation works entirely in string codec/fec names; the name↔id registry maps to the
// integer wire ids only at the §6.4 wire boundary (R4).

// NegotiateProfile picks the richest codec/FEC/rate every Render=true LISTENER can decode,
// constrained to what the MASTER can encode. Sink-less members (Render=false) are ignored
// for codec/FEC/rate; the master arg supplies the EncodeCodecs constraint (it may itself be
// sink-less — "master, no local render").
func NegotiateProfile(members []state.NodeRecord, master state.NodeRecord) Profile
```

Selection (richest → floor):

```
listeners = members where Caps.Render == true
codec:  "opus"  >  "pcm"              // ∩ over listeners' Caps.DecodeCodecs, then ∩ master.Caps.EncodeCodecs
                                       // PCM is the mandatory universal floor (dumb-node safe, D15); FLAC is NOT a wire codec (R2)
FEC:    "xorParity" > "duplicate" > "none"  // ∩ over listeners' Caps.FEC; XOR is the default ceiling (D4)
rate:   min over listeners' Caps.MaxRate (default 48000); FramesPerChunk fixed (480)
```

Worked example (group of N1 sink-less master + three members):

| Node | `Caps` (effective) | |
|---|---|---|
| N1 (master, NAS) | `Render=false, EncodeCodecs={pcm,opus}, …` | sink-less origin — **not** a listener |
| N2 | `Render=true, DecodeCodecs={pcm,opus}, FEC={xorParity}, MaxRate=48000` | |
| N3 | `Render=true, DecodeCodecs={pcm,opus}, FEC={xorParity,duplicate}, MaxRate=48000` | |
| N4 (dumb) | `Render=true, DecodeCodecs={pcm}, FEC={duplicate}, MaxRate=48000` | |
| **∩ listeners** | codec: `pcm` only; FEC: `duplicate` only; rate: 48000 | ∩ master `EncodeCodecs` keeps `pcm` → **Profile{"pcm", "duplicate", 48000, 480}** |

Drop dumb N4 and re-run: listener `∩` = `{opus}` codec, `{xorParity}` FEC → **`{"opus",
"xorParity", 48000, 480}`** (master can encode Opus). This is exactly the renegotiation in
§4.3.3. N1 being sink-less never affects the codec/FEC/rate floor — only its `EncodeCodecs`
acts as a ceiling. PCM is always in every full node's `DecodeCodecs`/`EncodeCodecs`, so the
listener intersection is never empty for full-node listeners; the PCM floor is what admits
future dumb nodes (D15).

### 4.3.3 Publication, renegotiation, and `streamGen`

**Publication.** The master writes the chosen `Profile` into its group's
`GroupRecord.profile` and bumps `ConfigDoc.Version`; gossip replicates it LWW
([07](./07-config-and-replication.md)). Followers read `profile` from the replicated doc and
configure their `Codec`/`FEC` decoders accordingly *before* admitting the next `streamGen`.

**Renegotiation triggers** (membership *or capability* change to *this* group, observed via
gossip):

- **Membership** — a `Render=true` listener **added** whose `Caps` narrow the intersection →
  profile may *downgrade*; a listener **removed** that was the constraint → profile may
  *upgrade*. (Adding/removing a `Render=false` member never changes the playback profile,
  §4.3.1 — though it can change the *master's* `EncodeCodecs` constraint if it is or becomes
  the master.)
- **Capability** — a member's effective `Caps` change via a ConfigDoc edit (§6.5 — effective
  caps = detected ∩ config-enabled, D16), even with membership unchanged. This includes:
  - a listener's `DecodeCodecs` / `FEC` / `MaxRate` changing (narrows/widens the intersection);
  - a member's **`Render` flipping** (T11): `true→false` drops it out of the listener set
    (profile may *upgrade*); `false→true` adds it as a listener (profile may *downgrade*);
  - the **master's `EncodeCodecs`** changing (relaxes/tightens the encode ceiling).

On any trigger the master re-runs `NegotiateProfile` over the current `Render=true` listeners
(and its own `EncodeCodecs`). If the result is unchanged, nothing happens. If it changes, the
master publishes the new `profile` **and bumps `streamGen`**.

**`streamGen` and in-flight playback.** `streamGen` is the §6.4 wire-header field
("group stream generation; reset on media/seek change") — and, here, on **profile change**.
Semantics:

- The master begins emitting chunks stamped with the **new** `streamGen` using the new
  codec/FEC; it sets the **keyframe** flag (§6.4 flags bit1) on the first chunk of the new
  generation so receivers can cleanly start.
- A follower that sees a `streamGen` greater than the one it is playing **drains** its ring
  ([06]) up to the generation boundary, reconfigures codec/FEC from the freshly published
  `profile`, and resumes decoding at the new generation's first `sampleIndex`. Chunks of the
  *old* `streamGen` arriving late are discarded.
- The **`sampleIndex` timeline is continuous across the bump** (§4.4): renegotiation does
  *not* reset playout position — only the *encoding* changes. So the user hears at most one
  small re-buffer (one ring-fill) at the switch, not a restart, unless the trigger was also a
  media/seek change (which legitimately resets `sampleIndex`).

```
streamGen:        7 7 7 7 │ 8(KEY) 8 8 8 ...
codec/FEC:        Opus/XOR │ PCM/Dup           (e.g. dumb node joined)
sampleIndex:  ...96000 ... │ 100800 ...        (continuous — no playout jump)
follower:     play old   │ drain→reconfig→play new   (≤ one ring refill of silence/hold)
```

This keeps profile changes **graceful**: the timeline is the invariant, the wire encoding is
the variable.

### 4.3.4 Zero-listener groups (no `Render=true` members)

A group with **zero `Render=true` members** (e.g. a lone sink-less NAS solo group, or a group
all of whose members have their sinks disabled) is **valid but silent**. There is no listener
set to negotiate a *playback* profile against, and nothing renders anywhere:

- The master (sink-less, "master, no local render") keeps its `clock.Server` and engine
  running normally, but the **origin idles** — it sources nothing useful, since there is no
  one to play it (no remote listeners, no local render). It does not fan out audio.
- `NegotiateProfile` over an empty listener set yields no meaningful playback constraint; the
  master may leave `GroupRecord.profile` at its default / last value (still bounded by its own
  `EncodeCodecs`). The profile becomes live again the instant a `Render=true` member joins or
  a member's `Render` is enabled (T11 → renegotiate, §4.3.3).
- This is not an error or `orphan` condition — it is a quiescent, fully-formed group. The UI
  surfaces it as "group has no audio output (no render-capable members)".

---

## 4.4 Timeline math — the synchronized sample index

### 4.4.1 What is synchronized

`mpvsync` synchronizes a *timeline position T in seconds* (`internal/sync/timeline.go`);
each video lane maps T onto its own clips. **Ensemble synchronizes a *stream sample
index*** instead: a monotonic count of **canonical-rate frames** on the group timeline
(§6.2 "Sample index is in canonical-rate frames"; §6.4 `sampleIndex` = "first sample's
canonical-rate frame index on the group timeline"). Sample index, not seconds, is the
synchronized quantity because audio sync is *sample*-accurate (spine §1) and the renderer
([06]) ultimately needs "which sample plays now", not a float second.

This is the same *pattern* as `internal/sync.Timeline` — a `(base, baseMono, rate, playing)`
clock read under a mutex with a mono stamp — re-expressed in samples:

```go
// §6.2 — the synchronized quantity per group. Do not redefine.
type Timeline interface {
    // NowSample returns the group's playout sample index expected at "now",
    // whether playing, and ok=false when no sync yet.
    NowSample() (sample int64, playing bool, ok bool)
}
```

### 4.4.2 Master-side `Timeline` (the authority)

On the master the timeline is authoritative, exactly like `sync.Timeline` but counting
samples. Let `Rate` be the canonical rate (Hz, from the profile, §4.3):

```
baseSample : int64   // sample index at baseMono
baseMono   : int64   // master monotonic ns when baseSample was set (clock.NowMono)
playing    : bool

atLocked(mono) = playing ? baseSample + round((mono − baseMono) · Rate / 1e9)
                         : baseSample
```

`Play(fromSample)`, `Pause()`, `Seek(posSample)`, `Seed(baseSample, playing)` mirror
`sync.Timeline`'s methods one-for-one (the `Seed` hook is the failover-continuity entry
used by transition **T4**). The master's `NowSample()` returns `(atLocked(NowMono()),
playing, true)` — `ok` is always true on the master (it is the reference, §4.1.3). Transport
commands ([05]/[08]) drive `Play/Pause/Seek`; a media or seek change additionally bumps
`streamGen` (§4.3.3, §6.4).

The master stamps **every chunk** it emits with the pair **(`sampleIndex`, `masterMono`)**
(§6.4 header): `sampleIndex` = the chunk's first canonical-rate frame on the timeline,
`masterMono` = `clock.NowMono()` on the master when the chunk was sourced. This pair is the
ground truth followers project against.

### 4.4.3 Follower-side projection (the core formula)

A follower does **not** run an authoritative timeline; it **projects** the master's timeline
to its own "now" using (a) the clock offset and (b) the most recent chunk's
`(sampleIndex, masterMono)` stamps. Two equivalent derivations — both are implemented as one
function:

**Derivation A — via the stamped chunk (preferred; ties directly to §6.4 header).**
A received chunk asserts: master-clock instant `chunk.masterMono` corresponds to playout
sample `chunk.sampleIndex`, advancing at `Rate`. To find the sample that should be *at the
speaker now*, convert the follower's "now" into the master's clock and extrapolate:

```
master_now_ns = clock.NowMono() + Offset          // §4.1.1, Follower.MasterNow()

NowSample = chunk.sampleIndex
          + round( (master_now_ns − chunk.masterMono) · Rate / 1e9 )
```

**Derivation B — via the master's published timeline base** (equivalent; used when the
master also gossips/sends its `(baseSample, baseMono)` rather than per-chunk stamps):

```
NowSample = baseSample + round( (clock.NowMono() + Offset − baseMono) · Rate / 1e9 )
```

A and B are identical because `(chunk.sampleIndex, chunk.masterMono)` *is* a point on the
master's `baseSample + (mono−baseMono)·Rate/1e9` line; any stamped chunk re-anchors the same
line. The follower uses **A** (it always has a recent chunk; no extra gossip needed) and
treats the newest chunk's stamps as the anchor, which also self-corrects for any master-side
re-base (Seek/Seed) the instant the first new-generation chunk arrives.

**`ok` and `playing`.** `ok = Offset().ok && (have a chunk for the current streamGen)`. When
`ok=false` the engine is in `orphan` (§4.2.3) and render holds. `playing` is carried from the
master (a paused master sends no advancing chunks / sets a paused flag via transport state in
[05]); a follower freezes `NowSample` at the last sample while paused.

**Putting offset semantics together.** Because `master_mono ≈ follower_mono + Offset`
(§4.1.1, §6.2):

```
   follower asks: "what sample is due at my instant follower_mono = clock.NowMono()?"
   step 1  translate to master clock:   master_mono = follower_mono + Offset
   step 2  walk the master timeline:     NowSample  = sampleIndex
                                                     + (master_mono − masterMono)·Rate/1e9
```

The render loop ([06](./06-audio-output-scheduling.md)) calls `NowSample()` to decide which
ring sample to feed the `AudioSink` next, applies the per-node `HWDelayUs` ([06], §6.5
`NodeRecord.HWDelayUs`) as a sample bias, and lets the PI drift loop trim residual ppm error
— so the *clock* layer only needs the projection to be correct in the mean, with jitter
absorbed downstream.

### 4.4.4 Failover continuity (`Seed`)

On promotion (T4/T8) the just-promoted follower's last good projection gives the sample that
*should* be playing now. The play/stop state is **not** inferred locally: the new master
reads the replicated **`GroupRecord.Playing bool`** (R4 — flipped by the play/stop endpoints,
[08], separate from `Media{File,Loop}`) and calls `Timeline.Seed(lastNowSample, Playing)` so
the new master's authoritative timeline continues from exactly there — no jump for the other
followers. If `Playing` is true it **resumes** at the current `sampleIndex` and begins
emitting chunks (new `streamGen`, keyframe-flagged, §4.3.3) anchored on the seeded
`(baseSample, baseMono=NowMono())`; if `Playing` is false it seeds paused at that sample. This
is the sample-domain analogue of `sync.Timeline.Seed`'s "continue from the last beacon T seen
as a follower", with the replicated `Playing` flag as the authoritative play/stop state.

---

## 4.5 Worked end-to-end example

Group `A`, profile `{"opus", "xorParity", 48000, 480}`, master `N1`, follower `N2` on WiFi.

```
N1 (master)                                   N2 (follower)
 Timeline: baseSample=0, baseMono=1_000_000ns  clock.Follower pings N1 @1s (A.12 WiFi)
 emits chunk: sampleIndex=96000,               Estimator (window 16, alpha 0.10 — A.12):
              masterMono=3_000_000ns             Offset = +812_000 ns  (N1 ahead of N2)
                                                 MinDelay = 1.4 ms  (healthy → follower)
 N2 render @ follower instant clock.NowMono()=2_400_000ns:
   master_now = 2_400_000 + 812_000           = 3_212_000 ns
   NowSample  = 96000 + (3_212_000 − 3_000_000)·48000/1e9
              = 96000 + round(212_000·48000/1e9)
              = 96000 + round(10.176)         = 96010   ← sample due at N2's speaker now
```

`N2` feeds ring sample `96010` (then `+HWDelayUs`, then PI-trimmed) to its `AudioSink`. If a
WiFi burst pushes `Delay` to 30 ms, the minimum-delay filter ignores that sample; `Offset`
barely moves; `MinDelay` stays low → `N2` remains `follower`, never `orphan`.

---

## 4.6 Group lifecycle

All lifecycle operations are **ConfigDoc edits** ([07](./07-config-and-replication.md)) on
`Groups[]`/`Nodes[]`, executed over the mTLS control plane ([03]/[08]) with optimistic
concurrency (§6.6 `If-Match`). The engine (§4.2) reacts to the replicated result; election
([02]) reacts in parallel. Each operation's effect on **election** and **streams** is called
out.

| Operation | ConfigDoc effect | Election ([02]) | Streams ([05]) | Engine transitions (§4.2.2) |
|---|---|---|---|---|
| **Create group** | Append `GroupRecord{members:[n], profile:negotiated}`. | New election domain; sole member is master. | New origin starts; `streamGen=0`. | T1 / T2 (→`solo` or `master`). |
| **Add member** | Append node id to `Groups[g].members`; remove from its old group. | Re-run for both groups; master usually stable. | If the new member is `Render=true`: master begins unicasting to it; **renegotiate** over listeners (§4.3.1/§4.3.3) → maybe `streamGen` bump. A `Render=false` member adds no listener/fan-out and does not move the playback profile. | New member: T3 (→`follower`) if `Render=true`, else control-only `member` (§4.2.4). Old `solo` master: T2. |
| **Remove member** | Drop id from `Groups[g].members`; node forms its **own** new solo group. | Re-run group `g`; node leaves its election domain. | If it was a `Render=true` listener, master stops that fan-out and **renegotiates** over remaining listeners (may upgrade). Removed node starts its own origin (or idles if sink-less, §4.3.4). | Removed node: T9 (→`solo`). |
| **Toggle a node's render** | Edit the node's `Caps.Render` in `Nodes[]` (§6.5; effective caps = detected ∩ config). | No change (sink-less stays master-eligible). | Master **renegotiates** over the new listener set (§4.3.3): enabling adds a listener (maybe downgrade + fan-out start); disabling drops one (maybe upgrade + fan-out stop). | Toggled node: T11 (render sub-system start/stop; `follower`↔control-only `member`). |
| **Move node A→B** | Atomic: drop from `A.members`, add to `B.members` (one versioned write) — the node's `GroupID` changes. | Re-run both `A` and `B`. | Leave A's fan-out; if `Render=true`, B adds it as a listener and renegotiates (may `streamGen`-bump); if `Render=false`, no fan-out and the playback profile is unmoved. | T9-then-T3 collapsed: on seeing its own `GroupID` change the node **orphans (silence)** during the transient and ends in B as `follower`/`master` (`Render=true`) or control-only `member` (`Render=false`, §4.2.4). It renders **only once it has B's master + clock + stream**, and never plays A's audio in the meantime (R13 m1). |
| **Dissolve group** | Remove `GroupRecord`; each member becomes its own solo group (or per UI, merged elsewhere). | All members leave the domain; each elects self. | All fan-out torn down; each ex-member starts solo origin. | Every member: T9 (→`solo`). |
| **Solo a node** | Shorthand for *remove member* → new group of one. | As remove. | As remove. | T9 (→`solo`). |
| **Master lost (failover)** | No edit; gossip/health detects departure ([02]). | Promote next candidate. | New master `Seed`s timeline, bumps `streamGen`, restarts origin; followers re-point. | Promoted: T4/T8. Others: T5 (re-point). |
| **Master changed (manual takeover)** | Set the group's **`MasterHint`** in ConfigDoc; election authoritative (consumes the hint, below). | Elect the hinted node iff alive & a member, else lowest stable id. | As failover but graceful (old master can hand off last sample). | Promoted: T4. Others: T5. |

**Invariants across all operations.**

1. **Exactly one group per node** (§6.2): every operation that removes a node from a group
   simultaneously places it in a group (its own solo group at minimum) — there is no
   "groupless" state. `orphan` is a *sync* state, not a membership state. **Group-move
   transient (R13 m1):** when a node observes its own `GroupID` change, it **orphans
   (silence)** and renders only once it has acquired the **new** group's master + clock +
   stream; it never keeps playing the old group's audio across the move.
2. **Single writer for `profile`**: only the group's elected master computes and publishes
   `GroupRecord.profile`, negotiated over the group's **`Caps.Render=true` listeners** and
   bounded by the master's own `Caps.EncodeCodecs` (§4.3.1). Renegotiation always rides a
   membership **or `Caps`** change (incl. a `Render` toggle, §4.3.3) and, when the result
   differs, bumps `streamGen`.
3. **Timeline continuity is preserved across master changes** via `Seed` (§4.4.4); it is
   *reset* only by an explicit media/seek change (which also bumps `streamGen`).
4. **Allowlist follows membership** (§6.5, [07]): clock and audio sockets accept only current
   member IPs; add/remove/move update the allowlist as a side effect of the ConfigDoc write,
   so a node dropped from a group immediately stops being able to ping the master's clock
   server or send/receive its audio.

**`MasterHint` consumption (soft preference).** A group's `GroupRecord.MasterHint` (A.5, §6.5)
is a *soft* preference, not a binding assignment. Election ([02]) picks the hinted node **iff
that node is both alive and a current member of the group**; otherwise it falls back to the
**lowest stable id among the alive members**. This keeps election deterministic and
split-brain-safe — every node computes the same winner from the replicated `(MasterHint,
membership, liveness)` with no extra coordination. Note the **rejoin-flip cost**: if a hinted
node was down (so a fallback node became master) and then rejoins, election flips mastership
back to the hinted node, which is a master change and therefore **bumps `streamGen`** (§4.3.3,
R11) — one ring-refill of re-buffering. Operators who don't want that churn should clear or
re-point `MasterHint` rather than relying on the dead node returning.

---

## 4.7 Summary of names introduced here (all subordinate to §6)

| Name | Kind | Defined where | Notes |
|---|---|---|---|
| `Profile` | struct | §4.3.2 | Concrete realization of §6.5 `GroupRecord.profile` (codec/FEC/rate/FramesPerChunk). |
| `NegotiateProfile` | func | §4.3.2 | Master-side least-common-capable selection over the `Render=true` listeners' `Caps` (`DecodeCodecs`/`FEC`/`MaxRate`), bounded by the master's `Caps.EncodeCodecs`. |
| group engine states `solo`/`master`/`follower`/`orphan` | states | §4.2 | The per-node role machine; `master`/`follower` align with spine §4 roles. |
| `ClockSource` adapters (master/follower/orphan) | impls | §4.1.3 | Implement §6.2 `ClockSource`; follower wraps reused `clock.Follower`. |
| sample-domain `Timeline` | impl | §4.4.2 | Implements §6.2 `Timeline`; pattern from `internal/sync.Timeline`, counting frames not seconds. |

Everything else (`ClockSource`, `Timeline`, `ConfigDoc`, `NodeRecord`, `GroupRecord`,
`streamGen`, `sampleIndex`, `masterMono`, the `Capabilities`/`Caps` struct and its fields
`Render`/`Sinks`/`EncodeCodecs`/`DecodeCodecs`/`FEC`/`MaxRate`, `CodecID`, `FECID`,
`FramesPerChunk`) is the spine's; this document only elaborates them.
