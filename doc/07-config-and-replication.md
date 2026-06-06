# 07 — Config document & replication

> **Scope.** This document is the authoritative elaboration of the spine's
> [§6.5 Config document](./README.md) and decision **D8** (memberlist gossip;
> versioned config doc; last-writer-wins; replicated to all full nodes). It
> defines **every type and field** of the replicated `ConfigDoc`, the
> **replication / merge / persistence** mechanics (reused verbatim from
> mpvsync's `internal/state`), the **allowlist derivation** that feeds the
> realtime planes, and the **consistency caveats** of running LWW over
> security-sensitive state.
>
> **Cross-references.** Discovery & gossip transport and per-group election:
> [02-cluster-discovery-membership.md](./02-cluster-discovery-membership.md).
> mTLS / cluster CA / adoption / takeover / **forget & revocation** and the auth
> field semantics: [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md).
> Group engine, timeline, **profile negotiation** that produces a `TransportProfile`:
> [04-clock-and-groups.md](./04-clock-and-groups.md). Channel / gain / hardware
> delay rendering: [06-audio-output-scheduling.md](./06-audio-output-scheduling.md).
> The HTTP write surface (`If-Match`, `409`, proxying) that mutates this doc:
> [08-http-api-reference.md](./08-http-api-reference.md).
>
> **Implementation reuse.** The store is a near-verbatim generalization of
> mpvsync `internal/state/state.go` — same `Version`/`Apply`/`Merge`/`Changed`
> contract, same gossip envelope, same atomic temp+rename persistence. We cite
> that file throughout so the audio implementation can lift the mechanics and
> only swap the document type (`Doc` → `ConfigDoc`). Where this spec says
> "as in mpvsync state.go" it means *byte-for-byte the same control flow*, only
> the payload struct differs.

---

## 1. Model & invariants

The `ConfigDoc` is **the** piece of cross-node truth that rides memberlist
gossip; everything else (media blobs, clock, audio) fans out over HTTP/UDP. As
in mpvsync it is kept **deliberately small**: it references media by file name,
certs by PEM (a few KB), never by large blob. It is:

- **Versioned** — a single `uint64 Version`, bumped on every accepted write.
- **Optimistic, last-writer-wins (LWW)** — no Raft, no vector clocks. A writer
  submits the version it edited; a stale version is rejected (`ErrConflict` /
  HTTP `409`). Gossip anti-entropy reconciles divergent replicas by **higher
  `Version`, tie broken by node id**.
- **Replicated to all full nodes** (D8). Every full node holds a complete copy
  and can serve/mutate it; cross-node writes are proxied over mTLS (§6.6).
- **Persisted per node** to a local JSON file via atomic temp+rename, so a full
  cluster restart recovers and gossip then reconciles the highest version.

### 1.1 Authoritative-replicated vs locally-derived

A hard rule that the rest of this document depends on: **the `ConfigDoc`
carries durable intent, never live runtime state.** Anything that changes on a
sub-second cadence, differs per observer, or is recomputed from the doc is
**not** in the doc.

| State | In `ConfigDoc`? | Where it lives | Why |
|---|---|---|---|
| Cluster identity, CA **public** cert | ✅ replicated | `ClusterInfo` | Durable trust root (public material). |
| CA **private** key (PEM) + cluster shared secret | ✅ replicated (plaintext) | `ClusterSecrets` (§2.8) | No sealing (D18); replicated to full nodes so any node can sign adoptions. |
| Admin password hash, API keys | ✅ replicated | `AuthConfig` | Must be uniform cluster-wide. |
| Node identity, cert, addrs, channel, gain, hw delay, caps | ✅ replicated | `NodeRecord` | Durable per-node intent. |
| Group membership, media selection, transport profile | ✅ replicated | `GroupRecord` | Durable per-group intent. |
| Revoked cert set | ✅ replicated (monotonic) | `RevokedSet` | Durable, security-critical, **grows only**. |
| **Online / reachable** | ❌ derived | `cluster` memberlist (02) | Per-observer, sub-second; gossip already carries it. |
| **Elected master per group** | ❌ derived | per-group election (02/04) | Deterministic from live membership + `MasterHint`. |
| **Allowlist (source-IP set)** | ❌ derived | `internal/allowlist` (§3) | A pure function of doc addrs ∪ live members. |
| **Clock offset, drift, ring fill** | ❌ runtime | `clock`/`audio` (04/06) | Realtime measurement. |
| `lastSeen` timestamp | ⚠️ see §2.5 | `NodeRecord.LastSeen` | A *cached hint*, not a liveness source of truth. |

> **`Online` is never a doc field.** A node's liveness is whatever the local
> memberlist says *right now* (see [02 §membership](./02-cluster-discovery-membership.md)).
> The UI computes `online = node.id ∈ liveMembers` at render time. Putting it in
> the LWW doc would create a flapping write storm and let a stale replica
> "resurrect" a dead node. The doc says *who should exist*; memberlist says
> *who is here*.

---

## 2. The `ConfigDoc` schema

All structs live in `internal/state`. JSON is the canonical wire + disk form
(camelCase tags, matching mpvsync convention). Field order below is the
canonical marshal order.

### 2.1 Top-level document

```go
// ConfigDoc is the whole replicated cluster configuration. It is the audio
// project's analog of mpvsync state.Doc: Version is the optimistic-concurrency
// / LWW stamp, bumped on every accepted Apply; UpdatedBy is the Merge
// tiebreaker writer id. Extends README §6.5 — do not contradict those tags.
type ConfigDoc struct {
	Version   uint64        `json:"version"`             // LWW stamp; bumped on every accepted Apply
	Cluster   ClusterInfo   `json:"cluster"`             // identity + CA PUBLIC cert (trust root)
	Secrets   ClusterSecrets `json:"secrets"`            // CA PRIVATE key + shared secret, PLAINTEXT, no sealing (D18, §2.8)
	Auth      AuthConfig    `json:"auth"`                // admin pw hash + API keys (D11)
	Nodes     []NodeRecord  `json:"nodes"`               // all known cluster members (durable intent)
	Groups    []GroupRecord `json:"groups"`              // all groups + their media/profile/transport
	Revoked   RevokedSet    `json:"revoked"`             // monotonic set of revoked/superseded certs (03)
	UpdatedBy string        `json:"updatedBy"`           // node id of last writer (Merge tiebreak)
	UpdatedAt string        `json:"updatedAt,omitempty"` // RFC3339; human/debug only, NOT a merge input
}
```

> **`UpdatedBy` is load-bearing.** It is exactly mpvsync's gossip-envelope
> `NodeID` promoted into the doc itself, so the tiebreak is self-describing even
> when the doc is read from disk rather than gossip. `Merge` still also receives
> the sender id from the envelope (§2.4); when both are present they must agree —
> if they differ, the envelope id wins for the tiebreak (it's the actual sender)
> and `UpdatedBy` is treated as advisory provenance.

> **`UpdatedAt` is never a merge input.** Wall clocks across LAN nodes are not
> trustworthy for ordering; only `Version` (and id tiebreak) order writes. The
> timestamp exists purely for the UI's "last changed" display.

### 2.2 `ClusterInfo`

```go
// ClusterInfo is the durable cluster identity and trust root. CACertPEM is the
// public cluster CA certificate distributed to every node so any node can
// verify any peer's CertPEM (NodeRecord). It changes only on cluster create or
// CA rotation (rare; a versioned write like any other).
type ClusterInfo struct {
	Name        string `json:"name"`                  // human cluster name (e.g. "Home")
	CACertPEM   string `json:"caCertPem"`             // PUBLIC cluster CA cert (PEM). Private key NEVER here.
	Created     string `json:"created"`               // RFC3339 cluster-creation timestamp
	Fingerprint string `json:"fingerprint"`           // SHA-256 of the CA DER, hex; stable cluster id for UI/takeover
}
```

- `CACertPEM` carries **only the public certificate.** The CA private key never
  enters the doc and never leaves the node that holds it; see
  [03 §PKI](./03-adoption-takeover-security-pki.md). Distributing the public CA
  in the doc is what lets a freshly-gossiped node validate every peer's
  `NodeRecord.CertPEM` without a side channel.
- `Fingerprint` is the stable cluster identifier surfaced in the UI and used by
  **takeover** detection: a node that already carries a *different* fingerprint
  is "owned by another cluster" and must be force-re-adopted (03, D9).

### 2.3 `AuthConfig`

```go
// AuthConfig holds the single cluster admin credential and the revocable API
// keys (D11). Only verifier material (argon2id hashes) is stored — never a
// plaintext password or raw key. Replicated so any node authenticates the same
// admin/keys; see 03 §auth and 08 (auth endpoints).
type AuthConfig struct {
	AdminHash   string   `json:"adminHash"`            // argon2id encoded hash of the admin password (PHC string)
	Argon       Argon2id `json:"argon"`                // argon2id cost params used for AdminHash (and key hashes)
	APIKeys     []APIKey `json:"apiKeys"`              // revocable API keys (hashed)
	PINHash     string   `json:"pinHash,omitempty"`    // argon2id hash of the adoption PIN (D9); "" => default "0000"
}

// Argon2id captures the cost parameters so every node verifies with identical
// settings and the admin can raise cost cluster-wide via a single versioned write.
type Argon2id struct {
	MemKiB  uint32 `json:"memKiB"`                       // memory cost in KiB (e.g. 65536 = 64 MiB)
	Time    uint32 `json:"time"`                         // iterations (e.g. 3)
	Threads uint8  `json:"threads"`                      // parallelism (e.g. 4)
	KeyLen  uint32 `json:"keyLen"`                       // output length in bytes (e.g. 32)
	SaltLen uint32 `json:"saltLen"`                      // per-credential salt length (e.g. 16)
}

// APIKey is one revocable key. The raw key is shown exactly once at creation
// (08); only Hash is persisted. Revocation = remove from this slice (and, for a
// key, that is sufficient — keys are not certs, so RevokedSet does not apply).
type APIKey struct {
	ID       string `json:"id"`                          // opaque key id (also the lookup handle)
	Name     string `json:"name"`                        // human label ("kitchen-tablet")
	Hash     string `json:"hash"`                         // argon2id hash of the raw key (PHC string)
	Created  string `json:"created"`                      // RFC3339
	LastUsed string `json:"lastUsed,omitempty"`           // RFC3339; best-effort, see caveat
}
```

> **`LastUsed` write amplification caveat.** Updating `LastUsed` on every
> authenticated request would make each request a versioned LWW write and a
> gossip round — pathological. Treat `LastUsed` as a **lazily, coarsely**
> updated hint (e.g. at most once per key per hour, and only on the node that
> served the request; it converges via normal LWW). It is **never** consulted
> for an authorization decision. If precise audit is ever needed it belongs in a
> local log, not the gossip doc.

> **`Argon` lives in the doc** so cost parameters are uniform and tunable
> cluster-wide. The per-credential salt is embedded in the PHC hash string
> (`$argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>`), so `SaltLen`/`Memkib`/etc.
> here are the *generation* parameters for the next credential write; existing
> hashes self-describe their own params for verification.

### 2.4 `NodeRecord`

Expands spine §6.5. One record per node that has ever been adopted (a node is
removed only by **forget**, §4.2).

```go
// NodeRecord is the durable, replicated intent for one node. It carries the
// node's signed public cert (so trust distributes with the doc), its known
// addresses (which drive the allowlist, §3), and its render configuration
// (channel/gain/hw-delay, applied by 06). It does NOT carry liveness or the
// elected-master flag (§1.1) — those are runtime-derived.
type NodeRecord struct {
	ID        string   `json:"id"`                       // stable node id (cert CN / public-key derived; survives IP change)
	Name      string   `json:"name"`                     // human label ("living-room")
	CertPEM   string   `json:"certPem"`                  // node's CA-signed PUBLIC cert (PEM) — distributes trust
	Addrs     []string `json:"addrs"`                    // known reachable IPs (host only, no port); drives allowlist
	HWDelayUs int      `json:"hwDelayUs"`                // hardware/output latency trim, microseconds (06, D13)
	Channel   string   `json:"channel"`                  // "stereo" | "left" | "right" (D13)
	GainDB    float64  `json:"gainDb"`                   // per-node output gain trim, dB
	Caps      Capabilities `json:"caps"`                 // EFFECTIVE caps = detected ∩ enabled (§2.4.1); negotiation input (D16)
	LastSeen  string   `json:"lastSeen,omitempty"`       // RFC3339 cached hint (§2.5); NOT authoritative liveness
}

// Capabilities is a node's advertised, EFFECTIVE ability set (README §6.5, D16):
// probed at runtime, then masked by per-node config so a present-but-unwanted path
// can be turned off. Identical shape to README §6.5 — do not contradict these tags.
type Capabilities struct {
	Render       bool     `json:"render"`  // false => control/media-only node (no audio sink), e.g. NAS/docker (D17)
	Sinks        []string `json:"sinks"`   // usable+enabled output backends: "alsa","pipewire","exec:aplay"
	EncodeCodecs []string `json:"encode"`  // codecs this node can ORIGINATE as a group master: "pcm","opus"
	DecodeCodecs []string `json:"decode"`  // codecs this node can PLAY as a listener: "pcm","opus"
	FEC          []string `json:"fec"`     // FEC schemes supported: "none","xorParity","duplicate"
	MaxRate      int      `json:"maxRate"` // highest sample rate this node will run, Hz
}
```

| Field | Replicated? | Consumer | Notes |
|---|---|---|---|
| `ID` | ✅ authoritative | everything | Derived from the node's keypair at first adoption; **never** changes, even when the IP does. The identity anchor. |
| `Name` | ✅ | UI (09) | Free text; editable from any node's UI. |
| `CertPEM` | ✅ | `pki`, mTLS | Public cert only. Rotated on takeover/re-adopt → old cert goes to `RevokedSet`. |
| `Addrs` | ✅ | `allowlist` (§3) | The **only** doc-side input to the source-IP gate. Self-maintained (§3.2). |
| `HWDelayUs`, `Channel`, `GainDB` | ✅ | `audio/render` (06) | Pure render intent. |
| `Caps` | ✅ | `group` profile negotiation (04) | **Effective** caps struct (§2.4.1); negotiation narrows a group's `TransportProfile` to the least-capable member. |
| `LastSeen` | ⚠️ hint | UI | See §2.5. |

> **String enums in JSON; integer ids only on the wire.** All ConfigDoc (and API)
> codec/FEC/transport fields are **string enums** — `EncodeCodecs`/`DecodeCodecs` use
> `"pcm"|"opus"`, `FEC` uses `"none"|"xorParity"|"duplicate"`. The integer `CodecID`/
> `FECID` exist **only at the wire layer** (§6.4) and are mapped to/from these names via
> a name↔id registry; they never appear in JSON. See R4(a).

> **`Caps` is the negotiation input.** [04 profile negotiation](./04-clock-and-groups.md)
> intersects the `Caps.EncodeCodecs`/`DecodeCodecs`/`FEC`/`MaxRate` across all
> `MemberNodeIDs` to pick `TransportProfile.Codec` and `FEC`, falling back toward
> the mandatory baseline `"pcm"` for the least-capable member, and caps `Rate` at the
> minimum `MaxRate`. A member with `Render=false` is a valid **master/origin** (it must
> have the negotiated codec in `EncodeCodecs`) but is never assigned as a listener. The
> result is *written back* into `GroupRecord.Profile`, so the doc records the negotiated
> outcome, not just the inputs.

#### 2.4.1 Effective capabilities = detected ∩ enabled (D16)

The `Caps` written into a node's **own** `NodeRecord` are not the raw hardware
probe. They are the **effective** set:

> **effective caps = detected(runtime probe, see [06](./06-audio-output-scheduling.md)) ∩ enabled(per-node config)**

computed **once at startup** (and on a config reload of the masking keys) and
written by the node into **its own** `NodeRecord.Caps` via the normal versioned
self-write (`If-Match`, §6.6, like `Addrs` in §3.2), after which it is gossiped to
all full nodes and consumed by profile negotiation (04).

- **detected** — what the backend registry's `Probe()` (README §6.1) and the
  codec/FEC registries actually report as working on this machine at runtime (the
  ALSA device opened via ioctl, PipeWire is reachable, `aplay` is on `$PATH`, the Opus
  encoder linked, the max rate the device accepts, …). No build-time switch (D12).
- **enabled** — the per-node masking config (§2.4.2). A path that is present but
  disabled by config is **removed** from the effective set, so it is never
  advertised and never selected by negotiation.
- A node whose effective `Sinks` is empty (no usable+enabled backend) sets
  `Render=false` and advertises a control/media-only node (D17) — it can still be a
  master/origin, media store, clock peer, and UI.

Because only the **effective** set is gossiped, peers and the negotiator never see a
capability the operator has masked off; the raw probe stays local to the node.

#### 2.4.2 Per-node capability masking / backend selection (config)

A node's effective caps are shaped by **per-node** configuration (durable per-node
intent, not replicated cluster state): keys in the node's local `config.yaml`
and/or set via the node-config API (the same write surface that edits a node's own
record, [08](./08-http-api-reference.md)). These knobs let an operator force a
node control-only, or pick which backends/codecs it uses when several are present:

```yaml
# config.yaml (per node) — capability masking & backend selection.
audio:
  render: false                  # force control-only / sink-less (no audio output), e.g. a NAS/docker node
  backends:
    disable: [pipewire]          # never use these probed backends
    prefer:  [alsa, exec:aplay]  # ordered preference among the remaining enabled+probed backends (Open(preferred,…), README §6.1)
  codecs:
    disable: [opus]              # drop these codecs from EncodeCodecs/DecodeCodecs even if the encoder links
```

| Key | Effect on effective `Caps` |
|---|---|
| `audio.render: false` | Force `Render=false` (sink-less / control-only) regardless of probe; `Sinks` emptied. |
| `audio.backends.disable: [...]` | Remove the named backends from detected before they enter `Sinks`. |
| `audio.backends.prefer: [...]` | Ordering only (does not add/remove caps); becomes the `preferred` list passed to `Open()` (README §6.1). |
| `audio.codecs.disable: [...]` | Remove the named codecs from `EncodeCodecs` and `DecodeCodecs`. |

These keys live in the node's **local** `config.yaml` / node-config API and feed the
startup computation in §2.4.1; only the resulting effective `Capabilities` struct is
written into the replicated `NodeRecord.Caps` and gossiped. The mask is *intent*; the
intersection with the live probe is what actually ships.

#### 2.5 `LastSeen` — a hint, not liveness

`LastSeen` is the one field that straddles the durable/runtime line, and it is
**deliberately not** the source of online status. Authoritative liveness is the
local memberlist (02). `LastSeen` exists so that a node the cluster hasn't seen
in a while still shows a meaningful "last contact" in the UI, and to inform the
*display ordering* of the cluster screen. It is written coarsely (like
`APIKey.LastUsed`, §2.3) to avoid LWW write storms, and a stale `LastSeen` must
never gate any decision. **Online status = `id ∈ liveMembers`, computed at
render time.**

### 2.6 `GroupRecord`, media, transport profile

```go
// GroupRecord is the durable intent for one group: who is in it, what it plays,
// and how it streams. The elected master is NOT stored (runtime, 02/04); only an
// optional operator hint is. MemberNodeIDs reference NodeRecord.ID values.
type GroupRecord struct {
	ID            string         `json:"id"`             // stable group id
	Name          string         `json:"name"`           // human label ("downstairs")
	MemberNodeIDs []string       `json:"memberNodeIds"`  // NodeRecord.IDs; a node is in EXACTLY one group (glossary)
	Media         MediaSelection `json:"media"`          // what this group plays (selection + loop intent)
	Playing       bool           `json:"playing"`        // durable play/stop intent; play/stop endpoints flip it (R4b)
	Profile       TransportProfile `json:"profile"`      // negotiated audio params (04)
	MasterHint    string         `json:"masterHint,omitempty"` // operator-preferred master node id; soft election preference (R12)
}

// MediaSelection is the chosen clip and loop intent. Media is referenced by
// file name within the per-node data/ folder (D14), never by blob — exactly
// mpvsync's "reference by name, never blob" rule. Selection is independent of
// play state: a stopped group can still carry a selected File (R4b).
type MediaSelection struct {
	File string `json:"file"`                            // file name within data/ (e.g. "calm-loop.mp3")
	Loop bool   `json:"loop"`                            // loop at end-of-file (D14)
}

// TransportProfile is the group's negotiated streaming parameters (D2–D5).
// Produced by 04 profile negotiation from member Caps; consumed by stream/* (05).
// Codec/FEC/transport are STRING enums in JSON; integer wire ids live only at the
// wire layer (§6.4), mapped via the name↔id registry (R4a).
type TransportProfile struct {
	Codec          string `json:"codec"`                 // "pcm" (mandatory baseline) | "opus"
	FEC            string `json:"fec"`                   // "none" | "xorParity" | "duplicate"
	Rate           int    `json:"rate"`                  // canonical sample rate, Hz (e.g. 48000)
	FramesPerChunk int    `json:"framesPerChunk"`        // codec frame size (default 480 = 10 ms @ 48k, §6.4)
	FECK           int    `json:"fecK"`                  // XOR-parity group size K (source pkts per repair); 0 if FEC "none"
	Interleave     int    `json:"interleave"`            // FEC interleave depth (burst-loss spread); 0/1 = none
	Transport      string `json:"transport"`             // "udp" (default, D2) | "tcp" (fallback)
}
```

> **Wire PCM is `S16LE`, fixed (m5).** There is **no profile bit-depth field**: the wire
> PCM format is always signed 16-bit little-endian (f32↔s16 conversion happens at the wire
> boundary; the internal pipeline stays f32). See [05](./05-audio-streaming-protocol.md).

| `GroupRecord` field | Source of truth | Notes |
|---|---|---|
| `MemberNodeIDs` | operator (UI/API) | A node appears in exactly one group's list (glossary). Reassigning moves it; the engine (04) tears down/rebuilds. |
| `Media` | operator | `File` is a name within `data/` (D14). Selection is **independent of play state**: a stopped group may still have a `File` selected (R4b). |
| `Playing` | operator (play/stop endpoints) | Durable play/stop **intent**, replicated. `play`/`stop` (08) flip this; on **master failover the new master reads `Playing` and resumes** if true. Replaces the old implicit "stopped == `Media.File==""`" model (R4b). |
| `Profile` | **negotiated**, written back | Initialized by operator intent, then narrowed by 04 to the least-capable member's `Caps`. |
| `MasterHint` | operator (optional) | A **soft election preference** — election picks it iff that node is alive and a group member, else the lowest stable id among alive members. The actual master is runtime state (§1.1), never stored. Consumption defined in [04](./04-clock-and-groups.md) / A.5 (R12). |

### 2.7 `RevokedSet` — monotonic, security-critical

```go
// RevokedSet is the set of certificates that must NOT be trusted, even if a
// NodeRecord referencing them reappears via a stale replica. It is MONOTONIC:
// entries are only ever added, never removed by normal operation (see §4.3).
// This is the mitigation for LWW's "stale node re-adds a forgotten peer" risk.
type RevokedSet struct {
	Entries []RevokedCert `json:"entries"`
}

// RevokedCert identifies a revoked/superseded cert by a stable, collision-
// resistant fingerprint (not by node id — takeover reuses the id with a NEW cert).
type RevokedCert struct {
	Fingerprint string `json:"fingerprint"`              // SHA-256 of the cert DER, hex — the match key
	NodeID      string `json:"nodeId,omitempty"`         // node the cert belonged to (provenance/UI)
	Reason      string `json:"reason"`                   // "forget" | "takeover" | "rotate"
	At          string `json:"at"`                       // RFC3339 when revoked
}
```

The PKI layer (03) consults `RevokedSet` on **every** mTLS handshake and on
**every** ingest of a peer `NodeRecord.CertPEM`: a cert whose fingerprint is in
the set is rejected regardless of what any `NodeRecord` says. Because the set
only grows, a stale replica that still lists a forgotten node cannot
re-establish trust to it — the forgotten node's cert is revoked, and even if its
`NodeRecord` is merged back in, its handshakes fail. See §4 for the full
argument.

### 2.8 `ClusterSecrets` — CA private key, in plaintext (D18, no sealing)

The cluster CA must sign **future** adoptions/renewals, and the spine requires that
**any** full node can drive adopt/takeover/forget (README §1/§2). Per **D18** there
is therefore **NO sealing**: the CA private key is replicated to full nodes **in
plaintext** rather than as an AEAD blob. We carry it (and any cluster shared secret)
in a dedicated `ClusterSecrets` value, coordinated with the bootstrap handshake in
[03](./03-adoption-takeover-security-pki.md) (the `encClusterSecrets` payload that
delivers `clusterRoot ‖ gossipKey` to a joining node):

```go
// ClusterSecrets carries the cluster's PRIVATE key material. Per D18 there is NO
// sealing — these are stored and replicated IN PLAINTEXT (limited LAN threat model,
// see 03). It is the one part of the replicated config that is never returned by any
// read API and never logged. The CA *public* cert stays in ClusterInfo; only the
// PRIVATE side lives here. Name/shape coordinated with 03's bootstrap encClusterSecrets.
type ClusterSecrets struct {
	CAKeyPEM     string `json:"caKeyPem"`     // cluster CA PRIVATE key (PEM, PKCS#8) — plaintext, no sealing (D18)
	SharedSecret string `json:"sharedSecret,omitempty"` // cluster root / gossip-key seed (the "clusterRoot" of 03), base64; plaintext
}
```

- **Replicated like the rest of the config.** `ClusterSecrets` rides the same
  versioned, LWW-merged `ConfigDoc` and gossips to **all full nodes** (D8), so any
  full node can sign. (Implementations may instead carry it as a **sibling
  replicated blob** alongside the doc — same gossip envelope, same persistence — if
  it is preferable to keep the signing key out of every doc snapshot served to the
  UI; either way it is plaintext and full-node-replicated. This doc owns its
  persistence/merge; 03 owns its *use*.)
- **No `Seal`/`Unseal`.** D18 supersedes any at-rest sealing: there is no
  `caKeySealed`, no seal key, no `SealCAKey`/`UnsealCAKey` step. A full node reads
  `CAKeyPEM` directly to sign.
- **Limited-threat-model rationale.** This is a single-segment, trusted home LAN
  with no internet/cloud/multi-site exposure (README §1 non-goals). Compromising any
  full node already implies the attacker can sign as the CA in memory, so encrypting
  the key at rest on those same nodes buys little; D18 trades that marginal at-rest
  protection for simplicity. Revisit (add sealing or Model C quorum signing) if the
  threat model widens — recorded as future work alongside the 03 threat model.
- **Never read back.** Like `AuthConfig` hashes, `ClusterSecrets` is never returned
  by a read endpoint, never appears in the UI, and is never logged; it exists only
  for the local PKI agent to sign with. Persistence is `0600` (§5.3).

---

## 3. Allowlist derivation

Decision **D2/D10**: clock and audio planes are **unencrypted but
source-allowlisted**. The allowlist is a **purely derived** runtime set — never
a doc field — recomputed from the `ConfigDoc` plus live membership. It lives in
`internal/allowlist` and is consumed by the clock and audio UDP sockets, which
**drop any packet whose source IP is not in the set** before parsing.

### 3.1 The derivation function

```go
// Allowlist is the source-IP gate set. AllowedSources derives it from the doc's
// node addresses UNION the live members' observed addresses. Both inputs matter:
// doc Addrs cover nodes that are configured but momentarily quiet; live members
// cover the just-observed reality (and multi-homed / freshly-DHCP'd nodes whose
// doc Addrs may lag, §3.3).
func AllowedSources(doc ConfigDoc, live []MemberAddr) map[netip.Addr]struct{} {
	set := make(map[netip.Addr]struct{})
	for _, n := range doc.Nodes {
		// A forgotten node has no NodeRecord (§4.2), so it contributes nothing.
		for _, a := range n.Addrs {
			if ip, err := netip.ParseAddr(a); err == nil {
				set[ip] = struct{}{}
			}
		}
	}
	for _, m := range live { // memberlist's currently-alive peers (02)
		set[m.Addr] = struct{}{}
	}
	return set
}
```

- **Union, not intersection.** A node may be a valid source before its
  `NodeRecord.Addrs` has caught up (e.g. just got a new DHCP lease), or while
  memberlist briefly considers it suspect. Taking the union avoids dropping
  legitimate realtime packets during these transients. The cost is a slightly
  larger trust surface (§4.4).
- **Host-only.** `Addrs` store IPs without ports; the gate matches on source IP,
  not port (realtime ports are fixed per plane, 04/05).

### 3.2 Dynamic recompute on `Changed()`

The allowlist is recomputed whenever either input changes:

1. **`ConfigDoc` changes** — the store's `Changed()` channel fires (exactly the
   mpvsync `Store.Changed()` coalesced signal). The allowlist owner does
   `doc := store.Get()` and rebuilds.
2. **Membership changes** — memberlist join/leave/suspect events (02) trigger
   the same rebuild.

```go
for {
	select {
	case <-store.Changed():       // ConfigDoc advanced (Apply or Merge), README §6.5
	case <-membership.Changed():  // live peer set changed (02)
	}
	current.Store(AllowedSources(store.Get(), membership.Live()))
}
```

The set is swapped atomically (`atomic.Pointer`), so the UDP read path does a
lock-free lookup. Recompute is cheap (a few dozen IPs) and rare (human-driven
config + occasional membership flaps), so coalescing via the single `Changed()`
signal is sufficient — same rationale mpvsync gives for its coalesced channel.

### 3.3 Multi-homed nodes & changed / DHCP IPs

- **Multi-homed** nodes list **every** reachable address in `NodeRecord.Addrs`.
  All of them enter the allowlist; realtime packets may originate from any. The
  group engine (04) still streams to a single chosen address per listener, but
  the gate must accept any of the node's addresses to tolerate route changes
  mid-stream.
- **A node maintains its own `Addrs`.** On boot, and on any interface/address
  change, a node performs a versioned write to **its own** `NodeRecord.Addrs`
  via the API (`If-Match`, §6.6, full surface in 08), then gossip propagates it.
  Because a node only edits *its own* record's addresses, concurrent
  self-updates from different nodes touch disjoint records and rarely conflict
  beyond the version bump (which `Apply` resolves by refetch+retry, as in
  mpvsync `state.go`).
- **DHCP churn.** When a node's lease changes, its old IP lingers in `Addrs`
  until its self-update lands. The **union** with live members (§3.1) means the
  new IP is already accepted (memberlist saw it), and the stale old IP is
  harmless until the self-write prunes it. A node **replaces** its address list
  on self-update (set semantics), so the old IP does not accumulate.

### 3.4 Security caveat — freshness bound

> **The allowlist is only as good as the doc's freshness.** It is an
> *anti-spoofing / anti-stranger* gate on an unauthenticated plane, **not** an
> authentication mechanism. Three consequences follow:
>
> 1. **Stale-doc over-permission.** A replica that hasn't yet merged a **forget**
>    still lists the removed node's IP and will accept its realtime packets until
>    the merge lands. Realtime traffic is unauthenticated, so during that window a
>    forgotten node can still inject clock/audio packets into a group it can reach.
>    This is *acceptable by design* (D10: realtime is allowlisted, not
>    authenticated) and bounded by gossip convergence time. **Control-plane**
>    access is cut immediately because the forgotten node's cert is revoked (§4) —
>    only the unauthenticated realtime planes have this lag.
> 2. **IP reuse.** If a forgotten node's IP is later assigned (DHCP) to an
>    unrelated host, that host is briefly allowlisted until the stale `Addrs`
>    entry is pruned. The realtime protocol headers (`magic 'ESND'`, `streamGen`,
>    per-group ports, §6.4) make blind injection hard, but the gate alone does not
>    prevent it.
> 3. **No integrity.** The allowlist does not protect against an on-path attacker
>    spoofing an allowed source IP. That is the explicit D10 trade-off (realtime
>    performance over realtime crypto); the threat model in
>    [03](./03-adoption-takeover-security-pki.md) covers it.

---

## 4. Consistency caveats over LWW

LWW is fine for human-driven, rare edits (mpvsync's stated assumption, copied
here). It is **dangerous** for security-sensitive fields because a *delete* can
be undone by a stale replica re-asserting an older-but-larger document. This
section states each risk and its mitigation. The unifying principle:

> **Security state must be expressed as monotonic growth or as an irreversible
> side effect, never as the absence of a field that a stale replica could
> restore.**

### 4.1 The core hazard: resurrection by stale replica

LWW keeps the doc with the **highest `Version`**. Suppose:

- Node A forgets node X by *editing the doc* (removes X's `NodeRecord`), bumping
  to `Version = 50`.
- Node B was partitioned and still holds `Version = 49` **with X present**.
- B comes back, makes an unrelated edit, bumping its own copy to `Version = 51`
  — still containing X.
- Gossip `Merge` (mpvsync `state.go` LWW) takes B's doc because `51 > 50`. **X is
  resurrected.**

A naive "forget = delete the record" is therefore unsafe under LWW. Mitigations:

### 4.2 Forget = revoke, not just delete

**Forget** (glossary; full flow in [03](./03-adoption-takeover-security-pki.md))
performs **two** doc mutations plus a side effect, in one versioned write:

1. **Remove** the node's `NodeRecord` from `Nodes` (and from any
   `GroupRecord.MemberNodeIDs`).
2. **Add** the node's cert fingerprint to `RevokedSet.Entries` with
   `Reason:"forget"`.

The crucial part is **(2)**: even if **(1)** is undone by a stale replica
resurrecting the `NodeRecord` (§4.1), the cert is in the monotonic
`RevokedSet`, so:

- The forgotten node's mTLS handshakes are **rejected** (control plane cut).
- Any merged-back `NodeRecord.CertPEM` is **ignored** by the PKI layer because
  its fingerprint is revoked.
- The only residual exposure is the unauthenticated realtime allowlist window of
  §3.4, bounded by convergence.

To re-admit a forgotten node it must be **re-adopted**, which issues a **new**
cert with a **new** fingerprint (the old one stays revoked forever). This is the
takeover/re-adopt path (D9), not a doc edit.

### 4.3 Monotonic `RevokedSet`

`RevokedSet` is the one field where merge is **not** plain whole-doc LWW. The
store special-cases it:

> **Union-on-merge for `Revoked`.** When `Merge` decides which whole doc to keep
> (by `Version`, then id — unchanged from mpvsync `state.go`), it then
> **additionally unions** `Revoked.Entries` from *both* the incoming and the
> resident doc into the surviving doc, deduplicated by `Fingerprint`. Thus a
> revocation can never be lost to a higher-versioned doc that predates it.

```go
// In Merge, AFTER the LWW winner `take` is chosen and BEFORE storing:
winner := /* the doc LWW selected (mpvsync state.go logic, unchanged) */
winner.Revoked = unionRevoked(s.doc.Revoked, remote.Revoked) // grows-only
```

This keeps the bulk of the document plain LWW (cheap, simple, matches mpvsync)
while making **revocation strictly grow-only and partition-tolerant**. Entries
are removed only by an explicit, deliberate **CA rotation / compaction** admin
operation (out of band, 03) — never by ordinary edits or merges.

> **Why fingerprint, not node id, is the revoked key.** Takeover **reuses the
> node id** with a fresh cert (D9). Revoking by id would lock the node out of its
> own re-adoption. Revoking by the *cert fingerprint* invalidates exactly the old
> credential and leaves the (future, distinct) re-issued cert trustable.

### 4.4 LWW on other security-sensitive fields

| Field | Risk under LWW | Mitigation |
|---|---|---|
| `Auth.AdminHash` | A stale replica restores the **old** password after a change. | Password change is rare and human-driven; the writer must use `If-Match` on the current version (§6.6) so it cannot blindly clobber. A successful change bumps `Version`; a stale replica's later edit can still re-assert the old hash (residual LWW risk) — accepted for a single-admin Phase. Hardening (per-field version / a `credEpoch` counter that is itself monotonic) is noted as future work in [10](./10-roadmap-and-dumb-nodes.md). |
| `Auth.APIKeys` (revoke) | A stale replica re-adds a revoked key. | Keys are *not* certs and have **no realtime side channel**: they only authorize control-plane requests, which are also gated by mTLS. A re-added key is still constrained, and the same `credEpoch` future-work mitigation applies. For now, treat API-key revocation as best-effort under partition and prefer rotating the admin password for a hard cutoff. |
| `Revoked` | Lost revocation (resurrection). | **Monotonic union merge** (§4.3) — the headline mitigation. |
| `Nodes[].Addrs` | A stale replica restores an old IP (over-permissive allowlist). | Self-maintained, set-replacing (§3.3); over-permission is bounded by §3.4 and convergence; not security-critical because realtime is unauthenticated by design. |
| Concurrent edits to **different** records | Lost update (one writer's change discarded). | `If-Match`/`409` (§6.6) forces refetch+retry, exactly mpvsync `Apply`'s `ErrConflict`. Edits to disjoint records still conflict at the *document* level (single `Version`), so the UI must refetch+reapply; cross-node writes are serialized by proxying to one node (08). |

### 4.5 Optimistic concurrency on the API

Every config write follows mpvsync's `Apply` contract, surfaced over HTTP per
spine §6.6 and detailed in [08](./08-http-api-reference.md):

- Client sends `If-Match: <version>` it based the edit on.
- Server calls `store.Apply(updatedDoc)` where `updatedDoc.Version` is that
  value. `Apply` (mpvsync `state.go`) requires `update.Version ==
  s.doc.Version`, else returns `ErrConflict` → HTTP **409**; the client
  refetches the authoritative doc and retries.
- On success the version is bumped, the doc persisted (§5), and `Changed()`
  fires — driving allowlist recompute (§3.2), group re-evaluation (04), etc.
- Cross-node writes are **proxied to a single node over mTLS** (§6.6) so two
  browsers on two nodes don't both win an LWW race; the proxy target serializes
  them through one `Apply`.

> **Retry policy (m6).** At MVP scale config writes use **doc-level LWW with a
> jittered retry-on-`409` (refetch the authoritative doc, reapply the edit, resubmit)**.
> Per-section / per-field versioning is a future optimization, not the MVP contract.

---

## 5. Replication & persistence mechanics

These are **lifted from mpvsync `internal/state/state.go`** with only the
payload type changed (`Doc` → `ConfigDoc`). The control flow is identical; we
cite the original throughout.

### 5.1 The `Store`

```go
// Store is the concurrency-safe holder of the ConfigDoc. Mirrors mpvsync
// state.Store exactly: a mutex-guarded doc, a coalesced changed channel, a selfID
// used only as the Merge tiebreak, and an optional persistence path. Construct
// with Load (persistent) or New (tests).
type Store struct {
	mu      sync.Mutex
	selfID  string
	doc     ConfigDoc
	changed chan struct{}

	path   string       // config.json location; "" => no persistence
	saveMu sync.Mutex   // serializes file writes outside mu (mpvsync pattern)
}

func New(selfID string) *Store                  // empty, non-persistent (tests)
func Load(selfID, path string) *Store           // seeds from path if well-formed; missing/corrupt => empty
func (s *Store) Get() ConfigDoc                  // deep copy (caller may mutate)
func (s *Store) Apply(update ConfigDoc) (ConfigDoc, error) // optimistic; ErrConflict on stale Version
func (s *Store) Merge(remote ConfigDoc, remoteID string)   // LWW by Version, id tiebreak; + union Revoked (§4.3)
func (s *Store) Changed() <-chan struct{}        // coalesced signal
func (s *Store) MarshalGossip() []byte           // {nodeId, doc} envelope
func (s *Store) MergeGossip(b []byte)            // parse envelope -> Merge

var ErrConflict = errors.New("config version conflict")
```

`Apply` is mpvsync's `Apply` verbatim: reject on `update.Version !=
s.doc.Version`, else `next.Version = s.doc.Version + 1`, store, save, signal,
return the post-bump doc. `Get` returns a deep copy (a `cloneConfigDoc`
analogous to mpvsync's `cloneDoc`, deep-copying the slices: `Nodes`, `Groups`,
`Revoked.Entries`, `APIKeys`, and every nested slice including `Addrs`,
`MemberNodeIDs`, and the per-node `Caps` slices (`Sinks`, `EncodeCodecs`,
`DecodeCodecs`, `FEC`). The `Secrets` (`ClusterSecrets`) value is copied with the
rest of the doc.

### 5.2 Gossip merge

`Merge` is mpvsync's `Merge` plus the §4.3 `Revoked` union:

```go
func (s *Store) Merge(remote ConfigDoc, remoteID string) {
	s.mu.Lock()
	take := false
	switch {
	case remote.Version > s.doc.Version:
		take = true
	case remote.Version == s.doc.Version && remote.Version != 0 && remoteID > s.selfID:
		take = true // deterministic tiebreak so all replicas converge (mpvsync)
	}
	// Revoked grows-only regardless of who wins the doc (§4.3):
	merged := s.doc
	if take {
		merged = cloneConfigDoc(remote)
	}
	merged.Revoked = unionRevoked(s.doc.Revoked, remote.Revoked)
	changed := take || len(merged.Revoked.Entries) != len(s.doc.Revoked.Entries)
	if !changed {
		s.mu.Unlock()
		return
	}
	s.doc = merged
	out := cloneConfigDoc(merged)
	s.mu.Unlock()

	s.save(out)
	s.signal() // only when the doc actually advanced (mpvsync semantics)
}
```

The gossip envelope and entry points are mpvsync's verbatim: `MarshalGossip`
emits `{nodeId: selfID, doc: <ConfigDoc>}`, `MergeGossip` parses it and calls
`Merge(env.Doc, env.NodeID)`. Transport is memberlist `LocalState`/delegate
exactly as in [02](./02-cluster-discovery-membership.md).

### 5.3 Persistence

Persistence is mpvsync `save` **with one change: file mode**. The audio config
holds secret material: argon2id hashes (not plaintext, but not world-readable
either), the cluster CA *public* cert and node certs, and — per D18 — the cluster
CA **private** key and shared secret in **plaintext** (`ClusterSecrets`, §2.8). The
latter alone mandates a tight mode, so:

> **Persist `config.json` with mode `0600`** (mpvsync used `0644` because "the
> show carries no secret"; the ConfigDoc carries credential verifiers **and the
> plaintext CA private key**, so we tighten it). Atomic temp+rename and the `saveMu`-serialized,
> outside-`mu`-on-a-snapshot pattern are otherwise identical to mpvsync `save`.

```go
func (s *Store) save(doc ConfigDoc) {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil { /* log to stderr, swallow — never fail Apply/Merge */ return }

	s.saveMu.Lock(); defer s.saveMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { /* log, swallow */ return }
	_ = os.Chmod(tmp, 0o600)            // defeat umask
	if err := os.Rename(tmp, s.path); err != nil { /* log, swallow */ return }
	_ = os.Chmod(s.path, 0o600)
}
```

As in mpvsync: writes are **best-effort** (a disk error is logged to stderr and
never fails `Apply`/`Merge`), they run **outside `s.mu`** on a snapshot taken
under `mu` (no deadlock), and `saveMu` prevents two concurrent saves from
interleaving a half-written file. The reconcile-on-rejoin behavior is inherited
free: `Load` seeds the in-memory doc from disk if well-formed (missing/corrupt =
empty, non-fatal), and gossip then reconciles the **cluster's highest version**
on rejoin — the exact mpvsync guarantee.

### 5.4 On-disk path

`config.json` lives under the node's config dir (`internal/config` `Paths`,
spine §5), alongside the node's private key (which is **never** in the doc).
Suggested: `<configdir>/ensemble/config.json`.

---

## 6. Worked example

A 4-node, 2-group cluster. `living-room` + `kitchen` form group `downstairs`
(stereo split: living-room = left, kitchen = right, PCM over UDP with
XOR-parity). `bedroom` is a solo stereo group `upstairs` (Opus). `nas` is a
sink-less NAS/docker node (`render: false`) — control/media/clock/origin only, in
no listening group. The CA private key + cluster shared secret are carried in
`secrets` **in plaintext** (D18, no sealing). One previously forgotten node's cert
is recorded in `revoked`.

```json
{
  "version": 87,
  "cluster": {
    "name": "Home",
    "caCertPem": "-----BEGIN CERTIFICATE-----\nMIIB...clusterCApublic...AB\n-----END CERTIFICATE-----\n",
    "created": "2026-01-12T09:14:03Z",
    "fingerprint": "f3a1c9e2b7d40518a6c2e9f1772b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d"
  },
  "secrets": {
    "caKeyPem": "-----BEGIN PRIVATE KEY-----\nMC4CAQAw...clusterCAprivate-PLAINTEXT-no-sealing-D18...Fg==\n-----END PRIVATE KEY-----\n",
    "sharedSecret": "c2hhcmVkLWNsdXN0ZXItcm9vdC1zZWVkLWJhc2U2NA=="
  },
  "auth": {
    "adminHash": "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQxMjM0NQ$Xk9...adminhash...Q=",
    "argon": { "memKiB": 65536, "time": 3, "threads": 4, "keyLen": 32, "saltLen": 16 },
    "apiKeys": [
      {
        "id": "ak_7Qm2",
        "name": "kitchen-tablet",
        "hash": "$argon2id$v=19$m=65536,t=3,p=4$a2V5c2FsdEFBQkND$Lm3...keyhash...8=",
        "created": "2026-02-01T18:22:10Z",
        "lastUsed": "2026-06-05T07:41:00Z"
      }
    ],
    "pinHash": "$argon2id$v=19$m=65536,t=3,p=4$cGluc2FsdHh4eHh4$Pn7...pinhash...A="
  },
  "nodes": [
    {
      "id": "node-living-room-8f2a",
      "name": "living-room",
      "certPem": "-----BEGIN CERTIFICATE-----\nMIIB...livingroom...==\n-----END CERTIFICATE-----\n",
      "addrs": ["192.168.1.21", "fe80::21"],
      "hwDelayUs": 4200,
      "channel": "left",
      "gainDb": 0.0,
      "caps": {
        "render": true,
        "sinks": ["alsa", "exec:aplay"],
        "encode": ["pcm", "opus"],
        "decode": ["pcm", "opus"],
        "fec": ["none", "xorParity", "duplicate"],
        "maxRate": 48000
      },
      "lastSeen": "2026-06-05T07:40:58Z"
    },
    {
      "id": "node-kitchen-3c7b",
      "name": "kitchen",
      "certPem": "-----BEGIN CERTIFICATE-----\nMIIB...kitchen...==\n-----END CERTIFICATE-----\n",
      "addrs": ["192.168.1.22"],
      "hwDelayUs": 5100,
      "channel": "right",
      "gainDb": -1.5,
      "caps": {
        "render": true,
        "sinks": ["exec:aplay"],
        "encode": ["pcm"],
        "decode": ["pcm"],
        "fec": ["none", "xorParity"],
        "maxRate": 48000
      },
      "lastSeen": "2026-06-05T07:40:59Z"
    },
    {
      "id": "node-bedroom-9d4e",
      "name": "bedroom",
      "certPem": "-----BEGIN CERTIFICATE-----\nMIIB...bedroom...==\n-----END CERTIFICATE-----\n",
      "addrs": ["192.168.1.23", "192.168.1.24"],
      "hwDelayUs": 3000,
      "channel": "stereo",
      "gainDb": 0.0,
      "caps": {
        "render": true,
        "sinks": ["alsa"],
        "encode": ["pcm", "opus"],
        "decode": ["pcm", "opus"],
        "fec": ["none", "xorParity", "duplicate"],
        "maxRate": 96000
      },
      "lastSeen": "2026-06-05T07:41:01Z"
    },
    {
      "id": "node-nas-2b6f",
      "name": "nas",
      "certPem": "-----BEGIN CERTIFICATE-----\nMIIB...nas...==\n-----END CERTIFICATE-----\n",
      "addrs": ["192.168.1.10"],
      "hwDelayUs": 0,
      "channel": "stereo",
      "gainDb": 0.0,
      "caps": {
        "render": false,
        "sinks": [],
        "encode": ["pcm", "opus"],
        "decode": [],
        "fec": ["none", "xorParity", "duplicate"],
        "maxRate": 48000
      },
      "lastSeen": "2026-06-05T07:41:00Z"
    }
  ],
  "groups": [
    {
      "id": "grp-downstairs",
      "name": "downstairs",
      "memberNodeIds": ["node-living-room-8f2a", "node-kitchen-3c7b"],
      "media": { "file": "calm-loop.mp3", "loop": true },
      "playing": true,
      "profile": {
        "codec": "pcm",
        "fec": "xorParity",
        "rate": 48000,
        "framesPerChunk": 480,
        "fecK": 8,
        "interleave": 4,
        "transport": "udp"
      },
      "masterHint": "node-living-room-8f2a"
    },
    {
      "id": "grp-upstairs",
      "name": "upstairs",
      "memberNodeIds": ["node-bedroom-9d4e"],
      "media": { "file": "rain.mp3", "loop": true },
      "playing": true,
      "profile": {
        "codec": "opus",
        "fec": "none",
        "rate": 48000,
        "framesPerChunk": 480,
        "fecK": 0,
        "interleave": 0,
        "transport": "udp"
      }
    }
  ],
  "revoked": {
    "entries": [
      {
        "fingerprint": "aa11bb22cc33dd44ee55ff6677889900aabbccddeeff00112233445566778899",
        "nodeId": "node-guest-1a9c",
        "reason": "forget",
        "at": "2026-05-30T20:05:44Z"
      }
    ]
  },
  "updatedBy": "node-living-room-8f2a",
  "updatedAt": "2026-06-05T07:38:12Z"
}
```

**Reading the example.**
- The `downstairs` group's `profile.codec = "pcm"` (the mandatory baseline) because
  `kitchen`'s `caps.decode`/`caps.encode` list only `"pcm"`; negotiation (04) intersected
  codecs across both members and narrowed to PCM. Its `fec = "xorParity"` (D4 default)
  with `fecK = 8`, `interleave = 4` (canonical A.12). `rate = 48000` is within both
  members' `maxRate`. `playing: true` is the durable play intent (R4b).
- `kitchen`'s effective `caps` show the **detected ∩ enabled** result (§2.4.1): no
  `alsa` sink (only `exec:aplay`) and no Opus — whether because the device lacks
  them or because `audio.backends.disable`/`audio.codecs.disable` (§2.4.2) masked
  them; the doc records only the effective outcome, not the reason.
- The stereo split is in the **`NodeRecord`s**, not the group: `living-room` is
  `left`, `kitchen` is `right` (D13). `render` (06) applies these.
- `nas` advertises `caps.render = false` with empty `sinks` and empty `decode`
  (D17): it is in no group's `memberNodeIds`, never a listener, but its non-empty
  `encode` means it *could* be elected/assigned a group **master/origin**, plus it
  serves media, clock, and UI. It still contributes its addr to the allowlist.
- `upstairs` uses `"opus"` and `fec = "none"` because it's a solo stereo group with no
  loss-recovery need on the master's own loopback path; `playing: true` is its play intent.
- `masterHint` on `downstairs` prefers `living-room`; it is a **soft preference** (R12) —
  if it's offline, election (02/04) picks `kitchen` (lowest stable id among alive members)
  and the doc is **not** rewritten — the elected master is runtime state (§1.1). On any
  master change `streamGen` bumps and the new master resumes from `Playing` (R11/R4b).
- `secrets.caKeyPem` is the cluster CA **private** key in **plaintext** (D18, no
  sealing, §2.8); it replicates to full nodes so any of them can sign adoptions, is
  never returned by a read API, and is persisted `0600` (§5.3). `secrets` is the one
  replicated block whose absence from read endpoints is mandatory.
- The derived allowlist (§3.1) for this doc ∪ live members =
  `{192.168.1.10, 192.168.1.21, fe80::21, 192.168.1.22, 192.168.1.23, 192.168.1.24}`
  plus anything memberlist currently reports. The forgotten `node-guest-1a9c`
  contributes **nothing** (no `NodeRecord`) and its cert is in `revoked`, so even
  a stale replica that re-adds its record cannot re-establish mTLS to it (§4.2).
