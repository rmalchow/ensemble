# 01 — Architecture & Packages

> Elaborates README **§4 (System architecture)** and **§5 (Package / module
> structure)**. This document is the structural map of *Ensemble*: what every
> `internal/` package is responsible for, who is allowed to import whom, how the
> three traffic planes are bound and gated, how a node flips between
> **group-master** and **group-follower** at runtime, how the binary is built and
> run, and four end-to-end sequence diagrams (cold boot, adoption, group play,
> master failover).
>
> It defines **no new contracts**. Every interface, type, field, and endpoint
> named here is the canonical one from README §6 — this doc only explains *where
> it lives* and *how the pieces wire together*. Behavior detail lives in the
> sibling docs, cross-referenced inline:
> [02 cluster/discovery/membership](./02-cluster-discovery-membership.md) ·
> [03 adoption/security/PKI](./03-adoption-takeover-security-pki.md) ·
> [04 clock & groups](./04-clock-and-groups.md) ·
> [05 streaming protocol](./05-audio-streaming-protocol.md) ·
> [06 output & scheduling](./06-audio-output-scheduling.md) ·
> [07 config & replication](./07-config-and-replication.md) ·
> [08 HTTP API](./08-http-api-reference.md) ·
> [09 UI screens](./09-ui-screens.md) ·
> [10 roadmap & dumb nodes](./10-roadmap-and-dumb-nodes.md).

---

## 1. The shape in one paragraph

Every **full node** (README §2) runs one binary, `cmd/ensemble`. That binary
wires together a fixed set of always-on subsystems (web/API server, discovery,
PKI agent, config store) and, once the node is **configured** (belongs to a
cluster), a full **active session**: membership gossip, per-group election, a
clock peer, the **group engine**, and an audio **renderer + sink**. The node's
role *within its group* — **master** or **follower** (README §2 glossary) — is
not a startup flag; it is a pure function of the live member set, recomputed
continuously, and the node starts/stops goroutines to match it **without
restarting** (§4 below). All control traffic is **mTLS** HTTP; clock and audio
are **unauthenticated UDP gated by a source-IP allowlist** (§3 below).

This mirrors the proven structure of the `mpvsync` video-wall daemon already in
this repository under `internal/` — Ensemble lifts its `discovery`, `cluster`
(membership + election), `clock`, gossip `state`, the web embed/`Deps` seam, and
the `ring`/`resampler` designs essentially as-is, and adds the audio-specific
packages (`pki`, `auth`, `allowlist`, `group`, `stream/*`, the audio
`render`/`sink`, and the corrected `drift` loop). Where this document says
"reuse," it means the corresponding mpvsync package named in §2 is the concrete
prior art.

---

## 2. Package responsibilities & layering rules

### 2.1 The package table

Each `internal/` package, its single responsibility, what it is allowed to
import (within the module), and its reuse provenance. **Allowed imports** lists
*sibling internal packages only*; every package may freely import the stdlib and
vetted third-party deps (`memberlist`, `zeroconf`, `argon2`, source-decode libs
`go-mp3`/`mewkiz/flac`, optional `opus` bindings, etc.).

| Package | Responsibility | May import (internal) | Provenance |
|---|---|---|---|
| `config/` | `Paths`, `Identity` (node id, name, `HWDelayUs`), parse `config.yaml`, resolve the data-dir layout. Pure data + filesystem; no networking. | *(none)* | reuse + extend (mpvsync `config`) |
| `pki/` | Cluster **CA**: generate/persist CA keypair, sign node **CSR**s, the per-node cert store, and the `tls.Config` builders for mTLS client & server. | `config` | **new** |
| `auth/` | Admin password (**argon2id**), browser **sessions**, revocable **API keys**, the adoption **PIN** verify, and the HTTP auth **middleware**. | `config` | **new** (extends mpvsync `auth` controller) |
| `discovery/` | LAN broadcast / **mDNS** announce + browse; emits bootstrap seed addresses. Bootstrap only — never the membership source of truth. | `config` | reuse (mpvsync `cluster/discovery.go`) |
| `cluster/` | **Membership** (memberlist SWIM gossip), the live peer set, and per-group **election** (lowest stable node id = master). | `state`, `config`, `discovery` | reuse (mpvsync `cluster`) |
| `state/` | The replicated, versioned **`ConfigDoc`** (README §6.5): gossip-merged, **last-writer-wins**, persisted to disk. The one cross-node document. | `config` | reuse mechanics (mpvsync gossip `state`) |
| `clock/` | NTP-style **4-timestamp UDP** offset estimator; `ClockSource` (README §6.2). Master runs the server; follower runs the estimator. | *(none)* | reuse as-is (mpvsync `clock`) |
| `allowlist/` | Source-IP **gate** for the clock + audio UDP sockets, derived live from `ConfigDoc.Nodes[].Addrs` ∪ group membership. | `state` | **new** |
| `group/` | The **group engine**: holds the node's current role, drives the master/follower lifecycle, owns the per-group `Timeline` (README §6.2), runs **profile** negotiation. | `clock`, `state`, `stream/*`, `audio/*`, `allowlist`, `config` | **new** |
| `stream/source/` | Master input **source-decode** (mp3 via `go-mp3`, FLAC via `mewkiz/flac`, WAV/PCM) → PCM, from a local file or HTTP(S) stream + loop; `data/` folder browse. **Source formats only — never a wire codec.** | `config` | **new** |
| `stream/codec/` | `Codec` (README §6.3): the **wire** codec set PCM \| Opus behind one interface (PCM baseline, Opus optional/capability-gated). | *(none)* | **new** |
| `stream/fec/` | `FEC` (README §6.3): XOR-parity \| Duplicate behind one interface. | *(none)* | **new** |
| `stream/wire/` | Chunk framing: the `ESND` header (README §6.4), marshal/unmarshal, `Packet`. | *(none)* | **new** |
| `stream/origin/` | Master side: decode → encode → FEC → **unicast** send loop, per listener. | `stream/source`, `stream/codec`, `stream/fec`, `stream/wire`, `clock` | **new** |
| `stream/sink_net/` | Follower side: UDP recv → FEC recover → decode → hand frames to the ring. | `stream/codec`, `stream/fec`, `stream/wire`, `allowlist` | **new** |
| `audio/ring/` | Jitter buffer (~300 ms). | *(none)* | reuse design (mpvsync `ring`) |
| `audio/resampler/` | Near-unity ppm-drift resampler. | *(none)* | reuse design (mpvsync `resampler`) |
| `audio/drift/` | **Corrected** content-domain PI loop (error → ppm). See [06](./06-audio-output-scheduling.md). | `audio/ring`, `clock` | new (corrected mpvsync `drift`) |
| `audio/render/` | Maps the group `Timeline` → sink writes; applies channel select / gain / `HWDelayUs`. | `audio/ring`, `audio/resampler`, `audio/drift`, `audio/sink`, `clock`, `state` | new (extends mpvsync renderer) |
| `audio/sink/` | `AudioSink` (README §6.1): a **pure-Go runtime registry** in one binary (no cgo, no build tags). **Precise** = direct kernel ioctl ALSA on `/dev/snd/pcmC*D*p` (`golang.org/x/sys/unix`, no libs); **coarse** = exec subprocess (`aplay`/`pw-play`), `Delay()` ok=false; **`Render=false`** if neither works. | *(none)* | reuse iface (mpvsync `audio/sink`) |
| `web/` | HTTP handlers (mTLS), the **`Deps`** function-value seam, embedded UI assets. | `auth`, `pki`, `cluster`, `state`, `config` | reuse pattern (mpvsync `web`) |
| `cmd/ensemble` | `main`, flag parsing, the daemon **wiring** that constructs every subsystem and supplies `web.Deps`. | *everything* | reuse (mpvsync `cmd/run.go`) |

### 2.2 Layering rules (the load-bearing ones)

These are the rules that keep the dependency graph acyclic and the subsystems
independently testable. They are enforced by code review and the import graph in
§2.3; several are already enforced in the mpvsync code being reused.

1. **`web` reaches the realtime/engine layer only through a `Deps` seam — never
   by import.** `internal/web` must **not** import `group`, `stream/*`, or
   `audio/*`. Instead `cmd/ensemble` constructs a `web.Deps` struct of function
   values and concrete pointers and hands it to `web.New`. The web layer reads
   live node state and triggers operations exclusively through those closures.
   This is exactly the pattern already proven in mpvsync's `internal/web/deps.go`
   (e.g. `MasterTransport func(op, media string, pos float64) error`,
   `IsMaster func() bool`, `SetAudioConfig func(...)`, `State *state.Store`): the
   web package "depends on the lower-level packages but never on the
   orchestrator, which keeps the dependency one-directional (no import cycle) and
   the server independently testable." Ensemble keeps this verbatim — the engine
   that web must not import is `group` (Ensemble's orchestrator) plus
   `stream/*`/`audio/*`, just as mpvsync's web must not import `node`/`mpv` for
   transport. The audio analog is the same rule the mpvsync `Deps.Transcodes`
   comment spells out: a type the web layer must surface but must not import
   (there, `internal/asset`) is adapted into a web-owned view type and exposed as
   a `Deps` func.

2. **`pki` and `auth` are boundary packages.** `pki` owns *what a node is allowed
   to do at the transport layer* (cert issuance, mTLS `tls.Config`); `auth` owns
   *who is allowed to drive the UI/API* (admin password, sessions, API keys, PIN).
   Both may import `config` only. Neither imports `cluster`, `group`, or
   `state` — they are leaf trust primitives so they can be unit-tested with no
   cluster. The wiring direction is one-way: `cmd` and `web` import `pki`/`auth`;
   `pki`/`auth` import nothing upward. (mpvsync's `auth.Controller` already
   follows this: it holds only a `Configure` closure supplied by the orchestrator
   and never imports it.)

3. **`state` is imported widely; it imports almost nothing.** `state` holds the
   canonical `ConfigDoc` (README §6.5) and is the merge/persistence engine. It is
   a near-leaf (imports `config` only) precisely *because* `cluster`, `group`,
   `allowlist`, `audio/render`, and `web` all read it. Making `state` depend on
   any of them would create a cycle. The gossip seam is inverted: `cluster`'s
   memberlist delegate calls `state.MarshalGossip()` / `state.MergeGossip()`
   (push/pull anti-entropy), so `cluster` imports `state` and never the reverse —
   identical to the mpvsync `delegate` in `cluster/membership.go`.

4. **`allowlist` is derived state, downstream of `state`, upstream of the UDP
   sockets.** It imports `state` (to read `Nodes[].Addrs`) and is consumed by
   `clock` (server-side filter) and `stream/sink_net` (receiver filter). It must
   not import `group` — the gate is a pure function of the doc + live membership,
   handed in.

5. **`group` is the orchestrator below `cmd`.** It is the only package that
   imports both the realtime stack (`stream/*`, `audio/*`, `clock`) and `state`.
   It is *not* imported by `web` (rule 1) and *not* imported by `state`/`clock`
   (they are leaves). Everything `group` exposes to the UI flows out through the
   `Deps` closures that `cmd` builds.

6. **`cmd/ensemble` wires everything.** Only `main` is allowed to import the full
   set. It owns: flag/`config.yaml` parsing, constructing `pki`/`auth`/`state`,
   binding the three planes' sockets, constructing the `group` engine, and
   building the `web.Deps` that bridges `web` → `group`/`audio`/`stream` without
   an import edge. This mirrors mpvsync `cmd/mpvsync/run.go` → `node.Run`.

7. **No realtime package imports `web` or `cmd`.** `stream/*` and `audio/*` are
   the lowest layer; they know nothing of HTTP, auth, or wiring. They are driven
   by `group` and tested in isolation.

> **Decoding the canonical `Codec`/`FEC` at the wiring layer.** Just as mpvsync
> decodes its canonical audio target in `run.go` "so the config package never
> imports `internal/asset`," Ensemble resolves the negotiated group **profile**
> (README §2: codec + FEC + rate) into concrete `codec.Codec` / `fec.FEC`
> implementations in `cmd`/`group`, not in `state`. `state` stores the profile as
> plain strings/ids in `GroupRecord.profile`; it never imports `stream/codec` or
> `stream/fec`. This keeps `state` a leaf (rule 3).

### 2.3 Import graph

Arrows point **in the import direction** (`A → B` = "A imports B"). The graph is
a DAG; the dashed `Deps` edge is the *only* path from `web` into the engine and
it is a **runtime function-value seam, not a compile-time import**.

```
                              ┌─────────────────────┐
                              │     cmd/ensemble     │  (wires everything; builds web.Deps)
                              └──────────┬──────────┘
            ┌───────────────┬───────────┼───────────┬───────────────┬─────────────┐
            ▼               ▼           ▼            ▼               ▼             ▼
        ┌───────┐      ┌────────┐   ┌───────┐   ┌────────┐     ┌─────────┐   ┌─────────┐
        │  web  │      │  auth  │   │  pki  │   │ group  │     │ cluster │   │discovery│
        └───┬───┘      └───┬────┘   └───┬───┘   └───┬────┘     └────┬────┘   └────┬────┘
            │   ╎Deps╎     │            │           │               │             │
            │   ╎(fn  ╎    │            │     ┌─────┼──────┬─────────┴───┐         │
            │   ╎value)╎   │            │     ▼     ▼      ▼             ▼         ▼
            │   └╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌►│ stream/* │ audio/* │ allowlist │  (state) │ (config)
            │              │            │   └──┬───┘  └──┬──┘    └────┬────┘
            ▼              ▼            ▼      │         │            │
        ┌────────────────────────────────────┴─────────┴────────────┘
        ▼                          ▼                       ▼
   ┌────────┐                 ┌─────────┐             ┌─────────┐
   │ state  │◄────(gossip)────│ cluster │             │  clock  │   (leaves: config-only or nothing)
   └───┬────┘    delegate     └─────────┘             └─────────┘
       ▼
   ┌────────┐
   │ config │   (pure leaf: filesystem + data types)
   └────────┘
```

Key invariants visible in the graph:

- **`web` has no solid edge into `group`/`stream/*`/`audio/*`** — only the dashed
  `Deps` seam. Compile-time, `web` imports just `auth`, `pki`, `cluster`,
  `state`, `config`.
- **`state`, `clock`, `config`** sit at the bottom as leaves (or near-leaves);
  everyone reads them, they read almost nothing.
- **`cluster → state`** is the gossip delegate edge; never `state → cluster`.
- **`group`** is the convergence point of the realtime stack and is itself only
  imported by `cmd`.

---

## 3. The three traffic planes

README §4 names three tiers. Here is exactly who binds which socket, on which
port, and how the two UDP planes are gated by the allowlist.

| Plane | Transport | Bound by | Default port | Auth model | Gate |
|---|---|---|---|---|---|
| **Control** | HTTP/1.1 + WebSocket over **TLS (mTLS)** | `web.Server`, via a listener `cmd` pre-binds | `:8443` (HTTPS, auto from `:443x`, configurable) | mTLS **node cert** *or* admin **session/API key** (README §6.6) | TLS handshake (CA-issued client cert) **or** `auth` middleware |
| **Clock** | **UDP**, per group, unauthenticated | `clock.Server` on the **master**; followers source-bind ephemeral | `:9000` (configurable) | none (realtime) | **allowlist**: source IP ∈ cluster members |
| **Audio** | **UDP unicast**, per group, unauthenticated; **TCP fallback** (D2) | `stream/origin` send sockets (master) → `stream/sink_net` recv socket (follower) | `:9100` (configurable) | none (realtime) | **allowlist** on the receiver socket |

### 3.1 Control plane (mTLS)

- **Who binds:** `cmd/ensemble` pre-binds a TCP listener (so it knows the actual
  port before mDNS/Meta advertise it — exactly as mpvsync's `node.Run` does for
  the plaintext UI), then hands it to `web.Server.Serve(ctx, ln)`. The server
  wraps it in TLS using a `tls.Config` built by `pki`:
  `MinVersion: TLS1.3`, `ClientAuth: tls.VerifyClientCertIfGiven`, `RootCAs`/
  `ClientCAs` = the cluster CA, server cert = this node's signed leaf.
- **Port:** HTTPS, defaulting to `:8443` and selected automatically from the
  `:443x` family with the +1-on-conflict retry the mpvsync `listenWeb` helper
  already implements (so several nodes share one host — §4.3). Overridable via
  `config.yaml` / `--web-port`.
- **Two auth paths into one server (README §6.6):**
  1. **Node↔node** (cross-node proxy, gossip-adjacent control): the caller
     presents its CA-issued **client cert**; mTLS verification *is* the auth.
  2. **Browser** (operator): no client cert; the request must carry a valid
     **admin session** cookie or **API key**, checked by the `auth` middleware.
  An uninitialized node (no CA yet, no admin password) serves only the **setup
  wizard** surface and `/api/setup`-class endpoints (§5a).
- **What rides it:** all of `ConfigDoc` writes (`If-Match` optimistic
  concurrency, `409` on conflict — README §6.6), cluster ops (adopt / takeover /
  forget), node config, group config, media commands, and the WebSocket UI push.
  Cross-node operations are **proxied node→node over mTLS** ([03](./03-adoption-takeover-security-pki.md), [08](./08-http-api-reference.md)).

### 3.2 Clock plane (UDP, allowlisted)

- **Who binds:** the **master** of each group runs one `clock.Server`
  (`clock.Listen(":9000")`), reused unchanged from mpvsync. Followers do not
  bind a well-known port; their estimator sends requests from an ephemeral port
  and matches replies by `seq`.
- **Wire:** the unchanged mpvsync **40-byte** clock packet (README §6.4: "Clock
  packets reuse mpvsync's 40-byte format unchanged") — `magic`, version, kind,
  `seq`, `t1/t2/t3`. The estimator computes `offset = ((t2−t1)+(t3−t4))/2`,
  feeds a min-delay + EWMA filter, and exposes `ClockSource.Offset()` /
  `MinDelay()` (README §6.2).
- **Gate:** the server-side read loop drops any datagram whose **source IP is not
  a current cluster member** per the `allowlist`. Detail and rationale in
  [04](./04-clock-and-groups.md) and [03](./03-adoption-takeover-security-pki.md).

### 3.3 Audio plane (UDP unicast, allowlisted)

- **Who binds:** on the master, `stream/origin` opens send sockets and unicasts
  one timestamped chunk stream **per listener** (D5: "Master decodes once →
  distributes timestamped chunks unicast per listener"). On each follower,
  `stream/sink_net` binds the receive socket (default `:9100`).
- **Wire:** the **`ESND`** chunk header (README §6.4) — `streamGen`, `seq`,
  `sampleIndex` (canonical-rate frame index on the group `Timeline`),
  `masterMono`, `codecID`, `fecID`, flags (repair/keyframe), payload. One chunk =
  `FramesPerChunk` (default 480 = 10 ms @ 48 k). Semantics in [05](./05-audio-streaming-protocol.md).
- **Gate:** the receiver applies the **allowlist** before parsing — a datagram
  from a non-member source IP is dropped. The sender knows its listeners from the
  group's `memberNodeIDs[]` → `Nodes[].Addrs` (README §6.5).
- **TCP fallback (D2):** when UDP is impractical (lossy link / NAT), the same
  framed chunk stream is carried over a per-listener TCP connection on the same
  port family; still allowlisted by peer IP. See [05](./05-audio-streaming-protocol.md).

### 3.4 How the allowlist is derived and applied

```
ConfigDoc.Nodes[].Addrs  ─┐
                          ├─►  allowlist.Set  ──►  UDP read loops drop !member src IP
live cluster membership  ─┘     (set of IPs)        ├─ clock.Server  (clock plane)
        (cluster.Member.Addr)                       └─ stream/sink_net (audio plane)
```

`allowlist` recomputes its IP set whenever the `ConfigDoc` changes (a node
adopted/forgotten changes `Nodes[].Addrs`) **or** membership changes (a member's
observed `cluster.Member.Addr`). It is the union of both: persisted addresses
plus live-observed gossip addresses, so a node that just joined is accepted even
before its `Addrs` are written back to the doc. Full IP-handling rules (multiple
NICs, address churn) in [07](./07-config-and-replication.md).

---

## 4. The dynamic role model (master ⇄ follower at runtime)

Role is **not** a CLI flag and **never** requires a restart. Every active node
continuously recomputes the elected master for its group and reconfigures its
running goroutines to match. This is the single most important runtime behavior
in the system; it is lifted directly from mpvsync's `node.loop` /
`applyRole` and re-pointed at the audio engine.

### 4.1 Election is stateless

`cluster.Election.Update(members)` returns the **lowest stable node id** among
alive members and a `changed` flag, and bumps a **generation** counter on every
change (mpvsync `cluster/election.go`, reused as-is). It is deliberately
*stateless*: the master is a pure function of the current member set, so every
node that sees the same membership agrees on the same master (no split brain).
The cost is that a lower-id node appearing triggers a master change — and thus a
clock re-baseline and a stream re-origin — which only happens on a topology
change. Detail in [02](./02-cluster-discovery-membership.md).

### 4.2 Which goroutines run in each role

The `group` engine owns a per-group **role loop** (the analog of mpvsync's
`node.loop`). On each membership tick / change it calls `applyRole`, which
cancels the current role's child context and starts the goroutines for the new
role under a fresh context bound to the **election generation**.

| Role | Goroutines started (per group) | Binds |
|---|---|---|
| **group-master** | `clock.Server` (clock plane) · `stream/origin` send loop (decode → encode → FEC → unicast, per listener) · the group `Timeline` owner + publisher · the **local** `audio/render` + `sink` (master renders too) | clock UDP :9000, audio UDP send sockets |
| **group-follower** | `clock` estimator (`ClockSource`) pointed at the master · `stream/sink_net` recv loop · `audio/ring` jitter buffer · `audio/drift` PI loop · `audio/render` + `sink` | audio UDP recv :9100 |

When the elected master **changes**, `applyRole`:

1. cancels the old role context → the previous role's goroutines unwind
   (origin send loop stops, or follower's recv/clock/render stop);
2. if **this node is now master**: starts `clock.Server`, the `Timeline`, and the
   `stream/origin` loop, and re-points its own renderer at the local timeline.
   On a **promotion** (was follower), the new master **seeds the timeline from
   the last sample index it was rendering**, so playout continues seamlessly
   rather than resetting — the audio analog of mpvsync seeding the new master's
   timeline from `lastBeaconT` for "failover continuity on promotion." It bumps
   `streamGen` (README §6.4) so followers discard stale chunks;
3. if **this node is now a follower**: dials the new master's clock + audio
   sockets, rebuilds the `ring`/`drift`/`render` chain, and re-locks.

Because election is generation-stamped, a stale role goroutine that wakes up
after a master change is cheaply ignored (its generation no longer matches), the
same guard mpvsync uses.

### 4.3 A node is in exactly one group; groups are independent

README §2: "A node is in exactly one group (possibly a group of one)." A
**solo** group (one member) elects *itself* master and runs the master goroutines
against itself — it decodes and renders locally with no network listeners. Each
group has its **own** clock, master, `Timeline`, and media, so the role loops of
different groups are independent. Moving a node between groups is a
`ConfigDoc` edit (`Groups[].memberNodeIDs[]`) that the role loop observes and
reacts to live — no restart. Group lifecycle detail in [04](./04-clock-and-groups.md).

### 4.4 Activation lifecycle (configured vs. unconfigured)

```
        ┌────────────┐  has cluster CA + cluster.yaml?            ┌───────────────────────────────┐
boot ─► │ UNCONFIGURED│ ── no ──► serve wizard + mDNS only ──┐    │           ACTIVE              │
        │  (idle)     │                                      │    │ membership · election · clock │
        └─────┬──────┘                                       │    │ group engine · render · sink  │
              │ adoption / wizard completes (PIN → CSR → CA) │    └───────────────┬───────────────┘
              └──────────── activate() in-process ───────────┴────────────────────┘
                                                                       │ forget()
                                                                       ▼
                                                                  UNCONFIGURED
```

An unconfigured node runs **only** the always-on parts — the web server (serving
the **setup wizard**) and the mDNS announce — and nothing else: no membership, no
clock, no group engine. When it becomes configured (wizard, adoption, or a
persisted identity at boot) it `activate()`s the full session **in-process, no
restart**; `forget()` deactivates back to unconfigured. This is mpvsync's exact
`activate`/`deactivate` lifecycle, re-scoped to the audio engine.

---

## 5. Build & run model

### 5.1 `cmd/ensemble` flags

`cmd/ensemble run` is the daemon (mirrors `cmd/mpvsync run.go`). Flags override
`config.yaml`; only the *set* flags override (the `fs.Visit` pattern).

| Flag | Meaning | Default |
|---|---|---|
| `--data <dir>` | data directory (config + identity + certs + media + persisted doc) | `./data` |
| `--config <path>` | explicit `config.yaml` (else `<data>/config.yaml`) | `<data>/config.yaml` |
| `--name <s>` | node friendly name (overrides config / persisted identity) | id prefix |
| `--web-port <n>` | control-plane HTTPS base port (retries +1 on conflict) | `8443` |
| `--clock-port <n>` | clock-plane UDP port | `9000` |
| `--audio-port <n>` | audio-plane UDP port | `9100` |
| `--bind-port <n>` | memberlist gossip port | `7946` |
| `--join <host:port>` | explicit gossip seed (repeatable) | *(mDNS)* |
| `--no-mdns` | disable mDNS announce/browse | mDNS on |
| `--device <s>` | audio sink device (e.g. ALSA `hw:0`) | sink default |
| `-v` | verbose cluster/engine logs | off |

The PIN, admin password, and channel/gain/`HWDelayUs` are **not** flags — they are
cluster/node state in the `ConfigDoc` and the persisted identity, set via the
wizard/UI ([09](./09-ui-screens.md)) and adoption ([03](./03-adoption-takeover-security-pki.md)).

### 5.2 Data-dir layout

`config.OpenDataDir(dir)` resolves and creates this layout (extends mpvsync
`config.Paths`):

```
<data>/
  config.yaml          operator config (ports, mDNS, audio defaults)   — flags override
  node.json            persisted Identity: node id (stable!), name, HWDelayUs, audio device  (0644)
  cluster.yaml         persisted cluster membership marker (group + activation)               (0600)
  certs/
    ca.crt             cluster CA public cert (also distributed in ConfigDoc.Cluster)
    node.key           this node's private key                                                (0600)
    node.crt           this node's CA-signed leaf cert
  doc.json             persisted ConfigDoc (README §6.5), reconciled by gossip on rejoin       (0644)
  data/                media folder — the mp3s this node can originate (per node)
  run/                 runtime sockets / scratch
```

- The **node id** in `node.json` is generated once and **must be stable** — it is
  the election tiebreak (a fresh id each boot would reshuffle the master). Same
  invariant as mpvsync `config.Identity`.
- `node.key` and `cluster.yaml` are mode **0600** (secret-bearing); the rest are
  0644. The persisted `doc.json` carries no plaintext secret (admin password and
  API keys are stored **hashed** in `ConfigDoc.Auth`, README §6.5).
- The CA private key lives only on whichever node holds it per
  [03](./03-adoption-takeover-security-pki.md); `ca.crt` (public) is replicated in
  `ConfigDoc.Cluster` so every node can verify peer certs.

### 5.3 Multiple test nodes on one box

The +1-on-conflict port retry plus per-node `--data` makes a multi-node dev
cluster on a single machine trivial — the repo already runs three nodes this way
(`data01/`, `data02/`, `data03/` exist alongside `scripts/dev`). Each node gets
its own data dir and a non-colliding port block:

```
ensemble run --data ./data01 --web-port 8443 --clock-port 9000 --audio-port 9100 --bind-port 7946
ensemble run --data ./data02 --web-port 8543 --clock-port 9010 --audio-port 9110 --bind-port 7956
ensemble run --data ./data03 --web-port 8643 --clock-port 9020 --audio-port 9120 --bind-port 7966
```

mDNS discovers the trio on the loopback/LAN; the lowest node id wins the election
and originates the stream to the other two. `scripts/dev` wraps this.

---

## 6. Sequence diagrams

Terminology matches the README §2 glossary (node, group, master, controller,
adoption, takeover, forget, ConfigDoc, profile, allowlist). All control hops are
HTTP over **mTLS** unless noted; clock/audio hops are **UDP**.

### 6a. Cold boot of an uninitialized node → setup wizard

```
Operator (browser)            Node N (UNCONFIGURED)                 filesystem
      │                              │                                   │
      │                     boot: config.OpenDataDir(<data>)             │
      │                              │── LoadOrCreateIdentity ──────────►│ node.json (new stable id)
      │                              │   no certs/, no cluster.yaml      │
      │                              │── pre-bind :8443 (TLS w/ SELF-    │
      │                              │   SIGNED leaf; no cluster CA yet) │
      │                              │── mDNS announce (group="")  ──────► LAN (shows as "discovered")
      │                              │   start ONLY: web + discovery     │
      │   GET /  (https, cert warn)  │                                   │
      │─────────────────────────────►  serve Setup Wizard SPA           │
      │   GET /api/setup             │                                   │
      │─────────────────────────────►  {configured:false, id, name}     │
      │◄─────────────────────────────                                   │
      │   POST /api/setup            │                                   │
      │   {clusterName, adminPw, PIN}│                                   │
      │─────────────────────────────►  auth: hash adminPw (argon2id)    │
      │                              │  pki: generate cluster CA         │
      │                              │       sign OWN CSR with CA        │
      │                              │── persist ──────────────────────►│ certs/{ca.crt,node.key,node.crt}
      │                              │  state: create ConfigDoc v1       │   cluster.yaml (0600)
      │                              │     Cluster{name,CA}, Auth{hash}, │   doc.json
      │                              │     Nodes:[self], Groups:[solo]   │
      │                              │── activate() in-process ──────────► full session (no restart)
      │                              │   membership(self) · election     │
      │                              │   (self=master) · clock.Server    │
      │                              │   · group engine · render+sink    │
      │◄──── 200 {configured:true} ──│  re-announce mDNS (group=name)    │
      │   browser establishes admin  │                                   │
      │   session; redirect Dashboard│                                   │
```

The node became a **cluster of one** (its own CA, a solo group, itself master).
Wizard screen detail in [09](./09-ui-screens.md); PKI/PIN detail in [03](./03-adoption-takeover-security-pki.md).

### 6b. Adoption: discovery → PIN → CSR → CA sign → join + ConfigDoc replicate

`A` = adopting controller node (already in the cluster, holds CA). `B` = the
uninitialized target. Operator drives `A`'s UI.

```
Operator   Controller A (member, has CA)        Target B (UNCONFIGURED)
   │              │                                    │
   │              │◄────── mDNS announce (group="") ───│   (B visible as "discovered")
   │  open Cluster│  screen: lists B as discoverable   │
   │─────────────►│                                    │
   │              │── GET https://B/api/local/probe ──►│  (no client cert required for probe)
   │              │◄── ProbeInfo{id,name,group:"",     │
   │              │       needsPassword:false, port}  ─│
   │  enter PIN   │                                    │
   │  for B  ────►│                                    │
   │              │── POST https://B/api/local/adopt ─►│  body: {clusterName, CA.crt, PIN,
   │              │     (mTLS: A presents node cert)   │         adminSession proxied}
   │              │                                    │  auth: verify PIN (const-time)  [D9]
   │              │                                    │  pki: gen keypair, build CSR
   │              │◄──────── CSR (B's public key) ─────│
   │              │  pki: CA signs CSR → B.crt         │
   │              │── return {B.crt, CA.crt, group, ──►│  persist certs/ (node.key/crt, ca.crt)
   │              │     gossip seeds [A's addr:port]}  │  state: receive ConfigDoc (B added to
   │              │                                    │         Nodes[] by A's write, v++)
   │              │                                    │  activate() in-process:
   │              │                                    │   membership.Join(seeds=[A]) ──┐
   │              │◄═══════ memberlist gossip ═════════╪═══════════════════════════════┘
   │              │   anti-entropy: ConfigDoc LWW merge│  (B now a member; allowlist on both
   │              │   converges highest version        │   sides admits the other's IP)
   │              │  election re-runs on new member set │  election re-runs; role loop reacts
   │◄── 200 ──────│  (lowest id = master; may change)  │
   │  UI shows B  │                                    │
   │  as adopted  │                                    │
```

`A` writes `B` into `ConfigDoc.Nodes[]` (with `B.crt`, `Addrs`, default
channel/gain) — the same `If-Match`/version write as any config change. Gossip
replicates the new doc to **all full nodes** (D8). Once `B`'s address is in
`Nodes[].Addrs` (and observed in membership), the **allowlist** on every node
admits `B`'s clock/audio packets. **Takeover** is the same flow against a node
that already has a cert/group (forced CA re-issue); **forget** revokes `B`'s
cert, drops it from `Nodes[]`, and removes it from the allowlist
([03](./03-adoption-takeover-security-pki.md)).

### 6c. Group play start: operator selects mp3 → master decode → followers render in sync

Group has master `M` and followers `F1`, `F2` (already clock-locked from §4.2).

```
Operator        Controller (any node)      Master M                F1 / F2 (followers)
   │                  │                        │                          │
   │ open Media,      │                        │                          │
   │ pick song.mp3 ──►│                        │                          │
   │  Play            │                        │                          │
   │                  │── POST /api/v1/groups/{g}/media (mTLS)             │
   │                  │   {select:"song.mp3", loop:true, action:"play"}    │
   │                  │   If-Match: <docVersion>                           │
   │                  │── proxied node→node to M (origin of the group) ───►│
   │                  │                        │ state.Apply: ConfigDoc    │
   │                  │                        │  Groups[g].media={song,   │
   │                  │                        │  loop}, version++         │
   │                  │◄══════ gossip replicate ConfigDoc (LWW) ══════════►│ (F1,F2 see media change)
   │                  │                        │ group engine (master):    │
   │                  │                        │  Timeline.Play(sample=0)  │
   │                  │                        │  bump streamGen           │
   │                  │                        │  stream/source: mp3→PCM   │
   │                  │                        │  (loop)                   │
   │                  │                        │  ┌─ per 10ms chunk (480f):│
   │                  │                        │  │ codec.Encode(PCM|Opus) │
   │                  │                        │  │ fec.Protect(seq,pkt)   │
   │                  │                        │  │ wire: ESND hdr {seq,   │
   │                  │                        │  │  sampleIndex,masterMono│
   │                  │                        │  │  streamGen,codec,fec}  │
   │                  │                        │  │ UDP unicast ──────────►│ :9100 (per listener)
   │                  │                        │  └─ master also renders    │  (allowlist admits M's IP)
   │                  │                        │     locally (its channel) │ sink_net: recv→allowlist ok
   │                  │                        │                           │ fec.Recover → codec.Decode
   │                  │                        │                           │ → audio/ring (jitter buf)
   │                  │                        │                           │ render: NowSample() from
   │                  │                        │                           │  Timeline (ClockSource.Offset
   │                  │                        │                           │  maps masterMono→local) →
   │                  │                        │                           │  drift PI trims resampler ppm
   │                  │                        │                           │  → channel select + gain +
   │                  │                        │                           │  HWDelayUs → AudioSink.Write
   │◄─ 200, UI playhead follows Timeline ──────│                           │  (all nodes emit sample N at
   │                  │                        │                           │   the same master-mono instant)
```

The synchronized quantity is `Timeline.NowSample()` (README §6.2): every node
maps the chunk's `sampleIndex`/`masterMono` (README §6.4) through its
`ClockSource.Offset()` to decide *which sample to emit now*, and the `drift` PI
loop nudges the resampler to hold alignment. Encode/FEC/recovery detail in
[05](./05-audio-streaming-protocol.md); scheduling/channel/gain/`HWDelayUs` in
[06](./06-audio-output-scheduling.md).

### 6d. Master failover: master dies → re-elect → new master re-origins → followers re-lock

Group had master `M` (lowest id) and followers `F1`, `F2`. `M` dies. Say `F1` has
the next-lowest id.

```
   M (dying)        F1 (id next-lowest)            F2
      ✗                  │                          │
      │   memberlist SWIM detects M failed (no acks) │
      │                  │◄═══ gossip: M marked dead ═══════════════════►│
      │                  │  cluster.Election.Update(members\{M})         │
      │                  │   → master = F1, changed=true, gen++          │   (F2 computes same: master=F1)
      │                  │                                               │
      │             group engine (F1) applyRole(promote):               │   applyRole(re-point):
      │              cancel follower ctx (stop recv/clock/render-as-     │    cancel old follower ctx
      │               follower)                                          │    (was locked to M)
      │              seed Timeline from last rendered sampleIndex ───────┼──► continuity (no reset to 0)
      │              bump streamGen (stale M chunks now ignored)         │
      │              start clock.Server (clock plane) ────────────────► :9000 on F1
      │              start stream/origin: mp3→decode→encode→FEC→unicast  │
      │               per listener (now F2; F1 renders locally)          │
      │                  │── clock UDP ──────────────────────────────────►│ F2 clock estimator re-points
      │                  │   (allowlist already admits F1's IP — member)  │  at F1, re-baselines Offset
      │                  │── audio UDP unicast (new streamGen) ──────────►│ F2 sink_net: drop old-gen,
      │                  │                                               │  accept new streamGen chunks
      │                  │                                               │  ring refills, drift re-locks
      │                  │  both F1 (local) and F2 emit aligned sample N  │
      │                  │  on the shared F1-mono timebase               │
```

Failover is leaderless and automatic: SWIM debounces churn so the master does not
flap; the **generation** bump and **`streamGen`** bump fence stale state; the new
master seeds its `Timeline` from where playout was so the song does not jump.
Followers re-baseline their `ClockSource` against the new master's
`clock.Server` and re-fill the `ring`. No operator action, no restart, no config
write (the `ConfigDoc` membership is unchanged — only the *runtime* role moved).
Election/failover mechanics in [02](./02-cluster-discovery-membership.md); timeline
continuity in [04](./04-clock-and-groups.md).

---

## 7. Cross-reference summary

| If you need… | See |
|---|---|
| Discovery, gossip membership, election/failover internals | [02](./02-cluster-discovery-membership.md) |
| mTLS/CA/CSR, PIN exchange, takeover/forget, allowlist rules, threat model, UI/API auth | [03](./03-adoption-takeover-security-pki.md) |
| Per-group clock, the group engine, `Timeline`, profile negotiation, group lifecycle | [04](./04-clock-and-groups.md) |
| `ESND` wire semantics, codec/FEC behavior, master send + receiver recovery, buffering | [05](./05-audio-streaming-protocol.md) |
| `AudioSink` ioctl-ALSA/exec sink, scheduling, the corrected drift PI loop, channel/gain/`HWDelayUs` | [06](./06-audio-output-scheduling.md) |
| Full `ConfigDoc` schema, gossip merge, persistence, allowlist derivation, IP handling | [07](./07-config-and-replication.md) |
| Every endpoint (cluster ops, node config, groups, media, auth) | [08](./08-http-api-reference.md) |
| Every UI screen and flow | [09](./09-ui-screens.md) |
| Build phases, dumb-node accommodation, open questions | [10](./10-roadmap-and-dumb-nodes.md) |
