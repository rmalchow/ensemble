# 10 — Roadmap & dumb-node accommodation

> Section 10 of the *Ensemble* spec. Read the [README spine](./README.md) first — this
> document does not redefine any decision (§3), package (§5), or contract (§6); it
> sequences their construction and shows how the locked design already admits future
> microcontroller **dumb nodes** ([D15](./README.md#3-locked-decisions-decision-log)).
> Three parts: **(1) build phases**, **(2) dumb-node accommodation**, **(3) consolidated
> open questions**.

This section is intentionally *plan* and *design-only*: no dumb-node implementation is
in scope now (per [Goals/non-goals](./README.md#1-goals--non-goals)), but every phase is
checked against "does this keep the minimal-profile path open?"

---

## 1. Build phases

The system is built bottom-up along its real dependency edges: identity and config
before PKI; PKI and mTLS before any node-to-node call; membership and the replicated
`ConfigDoc` before groups; clock and group engine before any audio; a single
PCM-only synced group before codecs, FEC, and multi-group. Each phase has a **goal**
and **done-criteria** (an observable, testable end state) and maps to the packages of
[§5](./README.md#5-package--module-structure) and the sibling section docs.

The guiding rule: **every phase ends with something runnable.** P0 produces a daemon
that boots and serves a page; P4 produces the first audibly-synced group; later phases
widen capability without breaking that spine.

### 1.1 Phase table

| Phase | Goal | Primary packages (§5) | Implements docs | Done-criteria |
|---|---|---|---|---|
| **P0 — Skeleton** | A daemon that boots, reads/writes `config.yaml`, has a stable node identity, and serves an (unsecured, localhost) page. | `cmd/ensemble`, `internal/config`, `internal/web` (skeleton) | [01](./01-architecture-and-packages.md) | Binary builds for amd64 + arm64; `config/` persists `Identity{id,name,hwDelay}`; daemon wiring follows the `web.Deps` function-value seam ([A.14](./README.md#a14--clusterruntime-glue)); `/healthz` responds. |
| **P1 — Trust & access** | Cluster CA, mTLS everywhere on the control plane, admin auth, and the setup wizard that bootstraps an uninitialized node. | `internal/pki`, `internal/auth`, `internal/web` | [03](./03-adoption-takeover-security-pki.md), [09](./09-ui-screens.md) (wizard, settings) | Fresh node shows **Setup Wizard**; wizard creates cluster CA + self-cert + admin password (argon2id); all `/api/v1` calls require mTLS node cert **or** admin session/API key; login works; CA private key replicated **in plaintext** to full nodes (**no sealing**, [D18](./README.md#3-locked-decisions-decision-log) — limited LAN threat model). |
| **P2 — Cluster** | Zero-config discovery, gossip membership, replicated versioned `ConfigDoc`, adoption/takeover/forget, and the derived allowlist. The cluster/runtime glue here — `PeerStore`/`peers.json`, the memberlist wrapper (`Config` + delegate `NotifyJoin/Leave/Update`, `LocalState`/`MergeRemoteState`) — is specified in [A.14](./README.md#a14--clusterruntime-glue) ([R8](./RECONCILIATION-LEDGER.md)). | `internal/discovery`, `internal/cluster` (membership), `internal/state`, `internal/allowlist` | [02](./02-cluster-discovery-membership.md), [03](./03-adoption-takeover-security-pki.md), [07](./07-config-and-replication.md) | Two fresh nodes discover each other; **adopt** (CA signs CSR, PIN-gated [D9](./README.md#3-locked-decisions-decision-log)) joins node B; `ConfigDoc` replicates to **all full nodes** (LWW, `If-Match`/`409`); **forget** revokes + drops from config and allowlist; **takeover** re-issues a foreign node; allowlist derived from `Nodes[].Addrs`; each node **runtime-probes its backends** ([D12](./README.md#3-locked-decisions-decision-log)) and **advertises effective `Caps`** (detected ∩ enabled, [D16](./README.md#3-locked-decisions-decision-log)) into its `NodeRecord` once `ConfigDoc` replication exists — a node with zero usable+enabled sinks advertises `Render=false` ([D17](./README.md#3-locked-decisions-decision-log)). |
| **P3 — Clock & groups** | Per-group NTP-style clock, the group engine with master election, and the group `Timeline`. The `applyRole` master⇄follower role loop is specified in [A.14](./README.md#a14--clusterruntime-glue) ([R8](./RECONCILIATION-LEDGER.md)). | `internal/clock`, `internal/cluster` (election), `internal/group` | [02](./02-cluster-discovery-membership.md) (election), [04](./04-clock-and-groups.md) | Create a group; per-group election picks a **master**; followers' `ClockSource.Offset()` converges (sub-ms, stable) over the allowlisted clock plane; `Timeline.NowSample()` returns a coherent sample index across members; no audio yet. |
| **P4 — First synced group (PCM)** | End-to-end audio: master decodes media and unicasts **PCM** chunks (no codec dependency — PCM is the mandatory wire baseline, [R2](./RECONCILIATION-LEDGER.md)); followers buffer, **corrected** drift-correct, and render to a sink. **The milestone release.** | `stream/source`, `stream/codec` (PCM), `stream/wire`, `stream/origin`, `stream/sink_net`, `audio/ring`, `audio/resampler`, `audio/drift`, `audio/render`, `audio/sink` | [04](./04-clock-and-groups.md), [05](./05-audio-streaming-protocol.md), [06](./06-audio-output-scheduling.md) | A 2-node group plays the same mp3 **sample-aligned** over WiFi (mp3/FLAC/WAV **source-decode** only, `stream/source`; FLAC is never a wire codec, [R2](./RECONCILIATION-LEDGER.md)); `ESND` wire header ([§6.4](./README.md#64-audio-wire-format--internalstreamwire)) on the wire; `~300 ms` ring; **content-domain** PI drift loop (corrected mpvsync design) holds sync for ≥1 h; the **runtime sink registry** ([D12](./README.md#3-locked-decisions-decision-log), [§6.1](./README.md#61-audio-output--internalaudiosink)) lands here — every OS-supported backend is compiled in and `Probe()`d at startup (no build-time `-tags` switch), so both the precise ALSA sink (**direct kernel ioctl on `/dev/snd/pcmC*D*p`, pure-Go via `syscall`/`x/sys/unix` — no `libasound`, no cgo, no dlopen**, [R7](./RECONCILIATION-LEDGER.md)) and the coarse `exec` (aplay/pw-play) sink work and are selectable/disable-able at runtime; the precise ioctl sink is a **verification spike** (prove on a Pi + DAC HAT, outstanding-frame feedback via `SNDRV_PCM_IOCTL_DELAY`); per-node `channel`/`gainDb`/`hwDelayUs` honored. |
| **P5 — Optional Opus & FEC** | The **optional, capability-gated** Opus wire codec and light FEC behind the `§6.3` interfaces, with per-group profile negotiation to the least-capable member. (PCM baseline already lands in P4; the wire `CodecID` set is `{PCM, OPUS}` — **no FLAC wire codec, no FLAC encoder**, [R2](./RECONCILIATION-LEDGER.md).) | `stream/codec` (Opus, optional), `stream/fec` (XOR-parity, duplication), `group` (negotiation) | [04](./04-clock-and-groups.md) (negotiation), [05](./05-audio-streaming-protocol.md) | Group runs the **PCM baseline** [D3](./README.md#3-locked-decisions-decision-log) and, where capability-gated, **optional Opus**; **XOR-parity** [D4](./README.md#3-locked-decisions-decision-log) and **duplication** recover induced packet loss; profile negotiated from `Nodes[].Caps` down to the least-capable member; `streamGen` resets correctly on media/seek; no Reed–Solomon. |
| **P6 — Multi-group & media UX** | Many concurrent independent groups, full channel roles, the media browser/control UI, and the node calibration workflow. | `group` (multi), `audio/render` (channel logic), `web/` UI, `stream/source` (browse) | [04](./04-clock-and-groups.md), [06](./06-audio-output-scheduling.md), [08](./08-http-api-reference.md), [09](./09-ui-screens.md) | ≥3 groups play different media simultaneously, each with its own clock/master; stereo/left/right roles audibly correct; **Media** screen browses `data/`, selects, play/stop/loop; **Node detail** calibration sets `hwDelayUs`/gain with audible verification; Dashboard reflects live state. |
| **P7 — Hardening** | Takeover-under-contention, failover polish, and operational robustness. | `cluster` (failover), `group`, `pki`, `allowlist`, `web` | [01](./01-architecture-and-packages.md) (failover seq), [02](./02-cluster-discovery-membership.md), [03](./03-adoption-takeover-security-pki.md) | Master loss triggers re-election with bounded audio gap; TCP fallback [D2](./README.md#3-locked-decisions-decision-log) exercised on UDP-hostile links; revoked certs rejected; allowlist churn under membership flap is correct; takeover races resolve deterministically; soak + chaos tests green. |

### 1.2 Dependency rationale (why this order)

- **P0 → P1:** identity (`config`) is the subject of the cert; the CA in `pki` signs it.
  Nothing talks node-to-node before mTLS exists, so trust precedes the network.
- **P1 → P2:** adoption is a PKI act (CSR → CA sign, [D9](./README.md#3-locked-decisions-decision-log));
  membership and `ConfigDoc` replication assume an authenticated control plane ([D10](./README.md#3-locked-decisions-decision-log)).
- **P2 → P3:** the per-group clock and audio planes are **allowlisted** off `ConfigDoc`
  membership ([§6.5](./README.md#65-config-document--internalstate-replicated-versioned-lww)),
  so the allowlist must exist before any UDP plane opens.
- **P3 → P4:** audio playout is defined as the group `Timeline` (sample index at a
  master-mono instant), which needs both clock offset and the group engine first.
- **Runtime backend discovery threads across P2–P4** ([D12](./README.md#3-locked-decisions-decision-log)):
  there is **no build-time `-tags alsa` switch** — every OS-supported backend is compiled
  into one pure-Go cross-compilable binary and resolved at startup (ALSA via
  **direct kernel ioctl**, pure-Go `syscall`/`x/sys/unix`, no `libasound`/cgo/dlopen
  ([R7](./RECONCILIATION-LEDGER.md)), plus `exec` backends). The **capability probe + advertisement** (a
  node detecting its usable backends and publishing effective `Caps`, [D16](./README.md#3-locked-decisions-decision-log))
  lands once membership/`ConfigDoc` replication exists, in **P2** (and the per-group
  `Caps`-driven negotiation in **P3/P5**); the **sink registry** itself (`Probe()`/`Open()`,
  [§6.1](./README.md#61-audio-output--internalaudiosink)) lands with the first rendering
  path in **P4**. A node that probes zero usable+enabled sinks advertises `Render=false`
  ([D17](./README.md#3-locked-decisions-decision-log)) — a sink-less full node — and is
  excluded from listener-profile negotiation.
- **P4 → P5:** the codec/FEC interfaces ([§6.3](./README.md#63-stream-codec--fec--internalstreamcodec-internalstreamfec))
  are added behind a working PCM path so each can be swapped and A/B tested in isolation.
- **P5 → P6:** profile negotiation (a P5 capability) is what lets heterogeneous nodes
  share a group, which multi-group + the calibration UX then exercise broadly.
- **P6 → P7:** hardening targets the steady-state behaviors only meaningful once the
  full feature surface (multi-group, takeover, media) exists.

### 1.3 Dumb-node readiness checkpoints (no implementation)

Even though dumb nodes are out of scope, the build inserts **design gates** so the
minimal-profile path is never accidentally closed (detail in part 2):

| At phase | Keep-open check |
|---|---|
| P1 | Adoption/PIN flow expressible without a browser (machine-only CSR path). |
| P2 | "lite member" can be a `NodeRecord` that is **not** a gossip peer (proxied). |
| P4 | PCM and a small `FramesPerChunk` work on a constrained receiver budget. |
| P5 | Opus + duplication is a valid negotiated **least-capable** profile. |
| P6 | A lite member is selectable as a group member in the UI/`ConfigDoc`. |

---

## 2. Dumb-node (microcontroller) accommodation — design-only

A **dumb node** ([glossary](./README.md#2-glossary), [D15](./README.md#3-locked-decisions-decision-log))
is a future microcontroller player: enough to **join, follow the clock, receive one
minimal stream profile, and accept basic config** — and nothing more. This part
specifies the *contract* such a node would have to meet and, critically, separates
**what the current design already supports** from **what would need adding**. No code
ships now.

### 2.0 The capability model spans two opposite reductions

The single `Capabilities` struct ([D16](./README.md#3-locked-decisions-decision-log),
[§6.5](./README.md#65-config-document--internalstate-replicated-versioned-lww)) is
deliberately general enough to describe a node reduced from the full stack along
**either** of two opposite axes — and both are just different field settings on the same
record, with no schema change:

- **(a) The future *dumb node* — reduced to a minimal *listener*.** `Render=true`, but
  everything else trimmed: a tiny `DecodeCodecs` (e.g. **Opus** or **PCM** only), **empty
  `EncodeCodecs`** (it can never originate / be a master), and a small `FEC`/`MaxRate`. It
  is a **lite member** — *not* a full gossip peer ([D8](./README.md#3-locked-decisions-decision-log));
  it is sponsored/proxied by a full node (see 2.1). It only *consumes* a stream.
- **(b) The *sink-less full node* (NAS/docker) — reduced to a non-*renderer*.**
  `Render=false` ([D17](./README.md#3-locked-decisions-decision-log)), but **otherwise
  full participation**: a full gossip peer that can **master / originate, serve media,
  hold the `ConfigDoc`, run the clock, and serve the UI** — it simply isn't a listener.
  Its `EncodeCodecs` can be full and its `DecodeCodecs`/`Sinks` are empty.

These are mirror images: the dumb node keeps rendering and drops almost everything else;
the sink-less node keeps almost everything and drops only rendering. The same
`Capabilities` struct ([D16](./README.md#3-locked-decisions-decision-log)) expresses both,
and the **least-capable-LISTENER negotiation rule** ([D3](./README.md#3-locked-decisions-decision-log),
[04](./04-clock-and-groups.md)) handles both cleanly: a sink-less node is not a listener
so it never constrains the negotiated profile, while a dumb node, as the least-capable
*listener* in its group, pins that group's profile down to what it can decode.

### 2.1 The "lite member" model (proposed)

Full nodes are gossip peers and hold the entire replicated `ConfigDoc`
([D8](./README.md#3-locked-decisions-decision-log)). A microcontroller cannot run
memberlist gossip, hold the full document, or terminate mTLS for a full HTTP API. So a
dumb node is **not** a gossip peer. Instead:

> **Lite member:** a stream + clock **client** that is *registered and proxied by a
> sponsoring full node*. It appears in `ConfigDoc.Nodes` as a `NodeRecord` (so it has an
> identity, addrs for the allowlist, `channel`/`gainDb`/`hwDelayUs`, and `caps`), but it
> participates only in the **clock plane** and **audio plane** of its group. All control
> for it (adoption, config writes, group assignment, takeover, forget) is performed by
> its **sponsor** full node on its behalf over the proxied control plane.

This reuses the existing any-node-proxies-cross-node-ops convention
([§6.6](./README.md#67-ui-navigation--web)) — the sponsor is just a controller acting
for a member that can't host its own full API. The dumb node's only server obligation is
a tiny adoption/config endpoint (or even a serial/provisioning channel), not the whole
`/api/v1`.

### 2.2 What a dumb node MUST do

| Capability | Why | Plane |
|---|---|---|
| Minimal **adoption** | Obtain identity + trust to be allowlisted. A constrained PIN/CSR exchange ([D9](./README.md#3-locked-decisions-decision-log)) — PIN treated as a real secret — yielding a signed cert (or a pre-provisioned device key). | Control (proxied) |
| **Clock follower** | Estimate offset to the group master via the reused 40-byte NTP-style clock packets ([§6.4](./README.md#64-audio-wire-format--internalstreamwire), [D6](./README.md#3-locked-decisions-decision-log)). Implements the consumer side of `ClockSource` semantically. | Clock |
| **Stream listener (minimal profile)** | Receive `ESND` chunks for **one** negotiated minimal profile and render them sample-aligned to its `Timeline`. Minimal profile = **PCM** *or* **Opus + duplication** (see 2.5). | Audio |
| **Basic config acceptance** | Accept/store `channel`, `gainDb`, `hwDelayUs`, group assignment, and media-state changes pushed by its sponsor. | Control (proxied) |
| **Identity persistence** | Hold its node id, key/cert, and last config across reboots. | local |

### 2.3 What a dumb node MAY skip

| Skipped | Replaced by |
|---|---|
| **memberlist gossip** ([D8](./README.md#3-locked-decisions-decision-log)) | Sponsor registers it; it never holds the full `ConfigDoc`. |
| Holding/merging the replicated `ConfigDoc` | Receives only the slice it needs (its own record + its group's profile/media). |
| Hosting the full `/api/v1` over mTLS | Tiny adoption/config surface only; sponsor proxies the rest. |
| **Master role / election** | Lite members are never elected master (they are followers only). |
| **Media decode of arbitrary formats / source folder** | No `stream/source`; it only *receives* an already-encoded stream. |
| **FLAC** and heavy codecs | Negotiation pins the group to the dumb node's least-capable profile. |
| Multi-group hosting, takeover/forget *initiation* | All initiated by full-node controllers. |

### 2.4 Protocol implications already baked in

The spine was written with [D15](./README.md#3-locked-decisions-decision-log) in mind, so
several enablers exist **today** in the contracts:

- **MCU-friendly wire format** ([§6.4](./README.md#64-audio-wire-format--internalstreamwire)):
  fixed-size, big-endian, 44-byte header with explicit `codecID`/`fecID`/`rate`/`seq`/
  `sampleIndex`/`masterMono` — parseable with no allocation and no variable framing on a
  microcontroller. `flags` carries repair/keyframe bits cheaply.
- **Light FEC only** ([D4](./README.md#3-locked-decisions-decision-log)): XOR-parity and
  duplication are explicitly chosen over Reed–Solomon *because* of future dumb nodes —
  both are trivial to decode in fixed memory.
- **Least-capable-member profile negotiation** ([D3](./README.md#3-locked-decisions-decision-log),
  [04](./04-clock-and-groups.md)): a dumb node in a group forces the whole group down to a
  profile it can handle, driven by its `NodeRecord.Caps`.
- **Unicast per-listener stream** ([D5](./README.md#3-locked-decisions-decision-log)): the
  master already sends a tailored stream to each listener, so a dumb node simply receives
  its own unicast at its own profile — no shared-stream constraint to fight.
- **Unencrypted, allowlisted realtime planes** ([D2](./README.md#3-locked-decisions-decision-log),
  [D10](./README.md#3-locked-decisions-decision-log)): clock + audio are **not** mTLS, so
  the dumb node needs no TLS on the hot path — only source-IP allowlisting, which the
  sponsor arranges by entering its addr into `ConfigDoc`.
- **Reused clock packet** ([§6.4](./README.md#64-audio-wire-format--internalstreamwire)):
  the 40-byte mpvsync clock format is unchanged and small.
- **Per-node trim already in the record** ([§6.5](./README.md#65-config-document--internalstate-replicated-versioned-lww)):
  `Channel`/`GainDB`/`HWDelayUs`/`Caps` exist per node, so a lite member is just another
  `NodeRecord` — no schema change needed to *describe* one.

### 2.5 What would need adding (not in current design)

| Gap | What's needed |
|---|---|
| **Lite-member flag** | A way to mark a `NodeRecord` as non-gossip/proxied (e.g. a `Caps` token like `"lite"` or an explicit field) and a `sponsorNodeID`. Today every node is assumed to be a full gossip peer. |
| **Sponsor/proxy semantics** | Define who sponsors, what happens on sponsor loss (re-sponsor vs. drop), and how the lite member's allowlist/config slice is pushed. |
| **Constrained adoption channel** | A browserless adoption path (serial/Wi-Fi-provisioning + minimal HTTP) for the PIN/CSR exchange; the current wizard ([09](./09-ui-screens.md)) is browser-first. |
| **Pre-provisioned device identity** | [D9](./README.md#3-locked-decisions-decision-log) names "per-device provisioning later" — the actual scheme (factory key, enrollment) is undefined. |
| **Minimal-profile guarantee** | Pin a normative "dumb baseline" (PCM **or** Opus+duplication, a fixed `FramesPerChunk`, a fixed rate) that all masters MUST be able to emit. |
| **No-resampler fallback** | The drift loop ([06](./06-audio-output-scheduling.md)) assumes a resampler; an MCU may instead drift-correct by drop/insert. Needs a defined coarse mode. |
| **Subset push API** | An endpoint/mechanism to push just `{group profile, media state, this node's trims}` instead of the whole `ConfigDoc`. |

### 2.6 Constraints (RAM / buffer / decode budget)

Design targets to keep the minimal profile feasible on a microcontroller (illustrative,
to be confirmed against chosen silicon):

| Resource | Pressure | Mitigation in current design |
|---|---|---|
| **RAM** | Jitter buffer + decode scratch dominate. | `ring` is `~300 ms`; a dumb node can run a **shorter** ring (e.g. 80–150 ms) since it tolerates more dropouts; PCM needs no decoder state. |
| **Network buffer** | One `ESND` chunk = `FramesPerChunk` (default 480 = 10 ms @ 48 k). | Fixed header + bounded payload → static receive buffers; duplication FEC needs only 1 extra packet held. |
| **Decode** | FLAC/Opus CPU + memory. | PCM = zero decode; **Opus** is the heaviest a dumb node should attempt and only if silicon allows; FLAC is **not** a dumb baseline. |
| **Clock CPU** | NTP-style offset math. | 4-timestamp integer math on a 40-byte packet; cheap. |
| **Crypto** | mTLS handshake cost. | Confined to the rare proxied control path; **not** on the audio/clock hot path. |
| **Flash** | Storing identity/cert/config. | Only id + key/cert + a tiny config slice (not the full `ConfigDoc`). |

---

## 3. Consolidated open questions

Gathers the 🟡 **defaults** from [§3](./README.md#3-locked-decisions-decision-log) (cheap
to change, awaiting confirmation) plus cross-document ambiguities surfaced while writing
the spec. Each has a **current default/assumption** and a **decision needed**.

| # | Topic | Source | Current default / assumption | Decision needed |
|---|---|---|---|---|
| **OQ-1** | **Optional-Opus binding** | D3 🟡 / [R2](./RECONCILIATION-LEDGER.md) | Wire codec set is **decided**: `{PCM, OPUS}`, PCM the mandatory baseline, FLAC removed from the wire/encode path ([R2](./RECONCILIATION-LEDGER.md)). Opus is optional + capability-gated; no libopus binding committed. | Choose the Opus encode/decode binding (if/when Opus is enabled); confirm Opus as the dumb-node baseline codec; lock `FramesPerChunk`/rate defaults (480 / 48 k). |
| **OQ-2** | **FEC scheme** | D4 🟡 | XOR-parity default; duplication optional; no Reed–Solomon. | Confirm XOR default and its parameters (group size N:1); confirm duplication as the dumb-node FEC. |
| **OQ-3** | **UI/API auth model** | D11 🟡 | Single cluster admin password (argon2id) + revocable API keys + UI sessions. | Confirm single-admin (no per-user RBAC) for now; session lifetime/rotation policy; API-key scoping. |
| **OQ-5** | **Multicast (later)** | D5 🔒 (unicast) / [05](./05-audio-streaming-protocol.md) | Master unicasts per listener; multicast explicitly deferred. | Confirm multicast is *post-1.0* only (it interacts with per-listener profiles + allowlist + FEC); record as non-goal-for-now. |
| **OQ-6** | **Language/runtime & reuse** | D1 🟡 | Go; reuse mpvsync `discovery`/`cluster`/`clock`/`ring`/`resampler`/web. | Confirm Go and the module path (`gitlab.rand0m.me/ruben/go/ensemble` placeholder); confirm mpvsync reuse boundaries. |
| **OQ-7** | **Precise ALSA ioctl spike** | D12 🟡 / [R7](./RECONCILIATION-LEDGER.md) | Sink stack is **decided** ([R7](./RECONCILIATION-LEDGER.md)): all backends compiled in, runtime-probed/advertised/disable-able (no build-time switch); precise sink = **direct kernel ioctl ALSA** on `/dev/snd/pcmC*D*p` (pure-Go `syscall`/`x/sys/unix`, no `libasound`/cgo/dlopen), coarse sink = `exec` (aplay/pw-play), `Render=false` if neither works. purego is out of the ALSA path (may resurface only for optional Opus). | **Verification spike:** prove the direct-ioctl precise sink on a Pi + DAC HAT — outstanding-frame feedback via `SNDRV_PCM_IOCTL_DELAY` (or `SYNC_PTR` → `appl_ptr - hw_ptr`) is accurate/stable enough for the drift loop; confirm Linux/Pi-first; whether PipeWire/PulseAudio get a first-class sink beyond `exec`; macOS/other later. |
| **OQ-8** | **Lite-member model** | D15 🔒 (design) / part 2 | Dumb node = proxied "lite member" `NodeRecord`, sponsored by a full node, not a gossip peer. | Approve the lite-member/sponsor model; if approved, schedule the schema additions in §2.5 (lite flag, `sponsorNodeID`, subset push). |
| **OQ-9** | **Drift correction on constrained nodes** | [06](./06-audio-output-scheduling.md) / §2.5 | Full nodes use the corrected content-domain PI resampler loop. | Define the coarse drop/insert fallback for nodes without a resampler. |
| **OQ-10** | **PIN provisioning** | D9 🔒 | PIN = `"0000"` placeholder, treated as a real secret; per-device provisioning "later". | Specify the real provisioning scheme (rotating PIN? per-device factory enrollment?) before any field deployment. |
| **OQ-11** | **Sponsor-loss policy** | §2.1 (new) | Unspecified what happens to a lite member when its sponsor leaves. | Re-sponsor automatically vs. mark offline vs. drop; define for P-future. |
| **OQ-12** | **TCP fallback trigger** | D2 🔒 | UDP primary, TCP fallback exists. | Define *when* fallback engages (loss threshold? handshake failure?) — hardening (P7) needs this concrete. |

### Notes on status

- OQ-2, -3, -6 are 🟡 defaults from the decision log: the architecture is built to
  make these **cheap to swap**, so they can stay as defaults through P5 and be confirmed
  without rework. OQ-1 and OQ-7 are now **mostly decided** by the ledger ([R2](./RECONCILIATION-LEDGER.md)
  wire codecs, [R7](./RECONCILIATION-LEDGER.md) sink stack); only the optional-Opus binding
  and the precise-ALSA-ioctl verification spike remain open.
- OQ-10 (PIN provisioning) is the remaining **security-critical** item; it should be
  resolved before P1/P2 ship to any real network even though a placeholder exists.
  (CA-key custody is now **decided** — [D18](./README.md#3-locked-decisions-decision-log)
  no-sealing — and is no longer an open question.)
- OQ-8, -9, -11 are **dumb-node design** items: not blocking for the full-node roadmap
  (P0–P7) but must be answered before any dumb-node implementation phase is opened.

---

*Cross-references: [README spine](./README.md) · [01](./01-architecture-and-packages.md) ·
[02](./02-cluster-discovery-membership.md) · [03](./03-adoption-takeover-security-pki.md) ·
[04](./04-clock-and-groups.md) · [05](./05-audio-streaming-protocol.md) ·
[06](./06-audio-output-scheduling.md) · [07](./07-config-and-replication.md) ·
[08](./08-http-api-reference.md) · [09](./09-ui-screens.md).*
