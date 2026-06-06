# Ensemble — networked multiroom audio system

> **Working name:** *Ensemble* (players that sing together). Module path placeholder
> `gitlab.rand0m.me/ruben/go/ensemble` — change freely.

A standalone, self-organizing multiroom audio system. Each **player node** runs the
same binary, serves its own web UI, joins a LAN cluster via broadcast discovery, and
plays audio in perfect sample-aligned sync with the other members of its **group**.
The cluster/clock/discovery design is lifted from the proven `mpvsync` video-wall
project; this document set specifies the audio-first system built on that spine.

This file is the **authoritative spine**: goals, glossary, the **locked decisions**,
the **package map**, and the **shared contracts** (interfaces, wire formats, config
schema, API/UI conventions) that every section document elaborates but must not
contradict. Read it first.

---

## 1. Goals & non-goals

**Goals**
- Zero-config cluster: power on → discovered → adopted → playing, no static config.
- Sample-accurate audio sync within a group (sub-millisecond, stable) over WiFi.
- Per-node hardware delay calibration; per-node channel role (stereo / left / right).
- Any number of independent **groups**, each with its own clock, master, and media.
- All **configuration/control** traffic encrypted + mutually authenticated (mTLS).
- All **realtime** traffic (clock, audio) unencrypted but **source-allowlisted**.
- Every node hosts a full UI that can operate the *whole* cluster (adopt/takeover/
  forget, configure any node, run media).
- Uninitialized nodes present a setup wizard; auth on every UI/API surface.

**Non-goals (now)**
- Microcontroller "dumb node" *implementation* (the protocol is *designed* to admit
  them; see [10-roadmap-and-dumb-nodes.md](./10-roadmap-and-dumb-nodes.md)).
- Internet/cloud control, multi-site, user accounts/RBAC (single admin for now).
- Media beyond "loop an mp3 from a data folder" (architecture leaves room for more).

---

## 2. Glossary

| Term | Meaning |
|---|---|
| **Node / player** | One running instance of the binary on one machine. |
| **Full node** | OS-based node (Linux/Pi) running the complete stack. May be **sink-less** (no audio output). |
| **Sink-less node** | A full node with `Render=false` (no usable/enabled audio sink) — control/media/master only, e.g. a NAS/docker node. |
| **Dumb node** | Future microcontroller node: join + playback + basic config only. |
| **Cluster** | All nodes sharing one cluster CA + replicated config. |
| **Group** | A set of nodes playing the same media on a shared timeline. A node is in exactly one group (possibly a group of one). |
| **Master** | The node elected within a group as clock reference + stream origin. |
| **Controller** | Whatever node is currently driving an operation (any adopted node can; the UI you're on). Not a fixed role. |
| **Adoption** | Bringing an uninitialized node into the cluster (CA signs its cert, gated by PIN). |
| **Takeover** | Re-adopting a node that already belongs to another/old cluster (forced re-issue). |
| **Forget** | Removing a node from the cluster (revoke cert, drop from config + allowlist). |
| **Config doc** | The replicated, versioned cluster configuration document (§7). |
| **Profile** | A group's negotiated audio (codec + FEC + rate) parameters. |
| **Allowlist** | The set of source IPs from which unencrypted (clock/audio) packets are accepted. |

---

## 3. Locked decisions (decision log)

Status legend: 🔒 locked · 🟡 default (confirm/redirect; designed to be cheap to change).

| # | Decision | Choice | Status |
|---|---|---|---|
| D1 | Language/runtime | Go; reuse mpvsync cluster/clock/web/discovery | 🟡 |
| D2 | Audio transport | UDP, source-allowlisted; TCP fallback | 🔒 |
| D3 | Wire codec | Wire codecs `{PCM baseline, Opus optional}`; **PCM** is the mandatory baseline (always works), **Opus** is optional + capability-gated; FLAC removed from the wire/encode path; **negotiated per group** to least-capable member | 🟡 |
| D4 | Loss recovery | Light FEC, **XOR-parity default**; packet-duplication option; (no Reed–Solomon — too heavy for future dumb nodes) | 🟡 |
| D5 | Stream model | Master decodes once → distributes timestamped chunks **unicast** per listener | 🔒 |
| D6 | Clock sync | NTP-style 4-timestamp UDP, per group, master = reference (reuse mpvsync `clock`) | 🔒 |
| D7 | Discovery | mDNS multicast + memberlist gossip, zero-config (reuse). NB: reused mpvsync is mDNS-only; a raw-UDP-broadcast fallback for mDNS-blocked nets is net-new (see [02](./02-cluster-discovery-membership.md)) | 🔒 |
| D8 | Membership/replication | memberlist gossip; versioned config doc; last-writer-wins; replicated to **all full nodes** | 🔒 |
| D9 | Adoption trust | Cluster CA signs node CSR, **gated by PIN**; PIN = `"0000"` placeholder, **treated as a real secret** in the protocol; per-device provisioning later | 🔒 |
| D10 | Transport security | **mTLS** for all config/control (HTTP); realtime traffic unencrypted + allowlisted | 🔒 |
| D11 | UI/API auth | Single **cluster admin password** + revocable **API keys**; sessions for UI | 🟡 |
| D12 | Audio output | `AudioSink` iface; **all OS-supported backends compiled in, discovered + advertised + disable-able at runtime** (no build-time capability switch). **Precise sink = direct kernel ioctl ALSA** on `/dev/snd/pcmC*D*p` (pure-Go via `syscall`/`x/sys/unix`; no `libasound`, no dlopen, no cgo); **coarse sink = exec subprocess** (aplay/pw-play); `Render=false` if neither works | 🟡 |
| D13 | Channel role | Per node: stereo / left / right (+ per-node `HWDelayUs` + gain trim) | 🔒 |
| D14 | Media (now) | Pick an mp3 from a `data/` folder, play looped; master-side decode. **Source decode also accepts FLAC and WAV/PCM**, and input may be a local file **or an HTTP(S) stream** (not just an mp3 from the folder) | 🔒 |
| D15 | Dumb nodes | Designed-for, not implemented; protocol must admit a minimal profile | 🔒 |
| D16 | Capabilities | Each node advertises **effective caps = detected(runtime) ∩ enabled(config)** in `NodeRecord.Caps` (render / sinks / encode / decode / fec); a present-but-unwanted path is disabled per node | 🔒 |
| D17 | Sink-less nodes | A full node may run **without an audio sink** (`Render=false`) — e.g. NAS/docker — and still be a group **master/origin**, media store, clock, and UI; just not a listener | 🔒 |
| D18 | CA key custody | **No sealing** (limited LAN threat model): the cluster CA private key is replicated in plaintext to full nodes; revisit if the threat model widens | 🔒 |
| D19 | Adoption handshake | X25519 ECDH (confidentiality) + PIN-keyed HMAC (auth) + ChaCha20-Poly1305 — Appendix A.9, normative (refines D9) | 🔒 |
| D20 | HTTP API shape | Namespaced `/api/v1/{auth,cluster,nodes,groups,media,calibrate}/…`; coordinated self-forget via `POST /cluster/leave`; string enums in JSON, integer ids only on the wire | 🔒 |
| D21 | Calibration | Built-in click+tone signal + `POST /calibrate/play`; **manual** `HWDelayUs` entry (MVP); automated cross-correlation `/calibrate/measure` is a later enhancement | 🟡 |
| D22 | Failover stream | `streamGen` **always bumps** on master change; timeline position stays continuous; receivers re-prime (~one buffer) | 🔒 |
| D23 | Master election | `MasterHint` is a **soft preference** (hinted node iff alive & a member, else lowest stable id) | 🔒 |
| D24 | Build/release | **Pure-Go, `CGO_ENABLED=0`, cross-compiles with no C toolchain**; GitLab CI matrix + downloadable artifacts (see [11](./11-build-ci-and-release.md)) | 🔒 |

> Rows D19–D24 fold in the resolved reconciliation decisions; per-document specifics (FEC params, port keys, schema field types, etc.) live in the section docs and Appendix A.12.

---

## 4. System architecture (roles & flows)

Every full node runs all of: **web/API server** (mTLS), **discovery**, **membership/
gossip**, **config store**, **PKI agent**, **clock peer**, **group engine**, and an
**audio renderer** + **sink**. Role within a group is dynamic:

- **As group master:** runs the **stream origin** (decode media → encode chunks → FEC
  → unicast to each listener) **and** the **clock server**; also renders locally **if it has a sink** (a sink-less master just originates — D17).
- **As group follower:** runs a **clock follower** + **stream receiver** + renderer.

```
                 ┌─────────────────────────── cluster (one CA, replicated config) ───────────────────────────┐
                 │                                                                                            │
   group A (master = N1)                          group B (master = N3)                  group C (solo = N5)  │
   ┌───────────────────────────┐                  ┌───────────────────────┐              ┌──────────────┐    │
   │ N1 master: clock+stream    │   unicast audio  │ N3 master              │              │ N5 master    │    │
   │   src→decode→PCM→FEC→UDP ───┼──┐  + clock UDP   │   ...                  │              │   (own media)│    │
   │   renders (L)              │  ├──► N2 (R)      │   ──► N4 (stereo)      │              └──────────────┘    │
   └───────────────────────────┘  └──► N2 follower │                        │                                  │
                 ▲ mTLS config/control (any node ↔ any node, any node ↔ browser)                               │
                 └────────────────────────────────────────────────────────────────────────────────────────────┘
```

Three traffic tiers (see [03](./03-adoption-takeover-security-pki.md)):
1. **Control plane** — HTTP over **mTLS**, any-to-any + browser. Config, cluster ops, media commands.
2. **Clock plane** — UDP, per group, **source-allowlisted**, unauthenticated.
3. **Audio plane** — UDP unicast, per group, **source-allowlisted**, unauthenticated; TCP fallback.

**Default ports** (all configurable; on a single box extra nodes take +1): control mTLS `:8443`, clock UDP `:9000`, audio UDP `:9100`.

---

## 5. Package / module structure

```
ensemble/
  cmd/ensemble/            main, flags, daemon wiring (mirrors mpvsync cmd/run.go)
  internal/
    config/                Paths, Identity (node id, name, hardware delay), config.yaml
    pki/                   cluster CA, CSR/sign, cert store, mTLS tls.Config builders
    auth/                  admin password (argon2id), sessions, API keys, middleware
    discovery/             broadcast/mDNS announce + browse (reuse mpvsync)
    cluster/               membership (memberlist gossip), peers, per-group election
    state/                 replicated versioned ConfigDoc (gossip-merged, LWW), persistence
    clock/                 NTP-style UDP offset estimator (reuse mpvsync clock)
    allowlist/             source-IP gate for clock+audio sockets (derived from ConfigDoc)
    group/                 group engine: role (master/follower), lifecycle, profile negotiation
    stream/
      source/              media decode (mp3/FLAC/WAV→PCM) + loop; "data/" folder browsing or HTTP(S) stream input
      codec/               PCM | Opus wire encoders/decoders behind one interface
      fec/                 XOR-parity | duplication behind one interface
      wire/                chunk framing, seq/timestamp header, marshal/unmarshal
      origin/              master side: decode→encode→fec→unicast send loop
      sink_net/            follower side: udp recv→fec recover→decode→ring
    audio/
      ring/                jitter buffer (~300 ms)
      resampler/           near-unity ppm drift resampler (reuse mpvsync design)
      drift/               PI loop (content-domain error → ppm) (corrected mpvsync design)
      render/              maps group timeline → sink writes; channel select/gain/HWDelayUs
      sink/                AudioSink iface; sink_alsa.go (direct kernel ioctl, pure-Go, precise); sink_exec.go (coarse)
    web/                   HTTP handlers (mTLS), Deps seam, embedded UI assets
  web/                     Svelte+Vite UI (mirrors mpvsync web/)
  data/                    media folder (mp3s) — per node
  scripts/                 build, build-arm64, dev, make-test-clip
```

Reuse-as-is from mpvsync: `discovery`, `cluster` membership/election, `clock`, the
`resampler` and `ring` designs, the web embed/Deps pattern, gossip `state` mechanics.
New: `pki`, `auth`, `allowlist`, `group`, all of `stream/*`, `audio/render` channel
logic, the **corrected** `drift` loop (content-domain error — see
[06](./06-audio-output-scheduling.md)).

---

## 6. Shared contracts (canonical — elaborate, do not redefine)

Section docs MUST use these exact names/signatures. Behavior is specified in the
referenced section.

### 6.1 Audio output — `internal/audio/sink`
```go
type AudioSink interface {
    Start(rate, channels int) error
    Write(frames []float32) (n int, err error) // interleaved; blocks for backpressure
    Delay() (samples int, ok bool)             // outstanding in device; ok=false => coarse
    Close() error
}
// Runtime backend registry (NOT a build-time switch — every OS-supported backend is
// compiled in and probed at startup; details in 06-audio-output-scheduling.md):
//   Probe() []Backend                                   // backends that actually work here, minus config-disabled
//   Open(preferred []string, device string) (AudioSink, error)
//   type Backend struct{ Name string; Precise bool }    // "alsa"(direct kernel ioctl, precise) | "exec:aplay"(coarse)
// A node with zero usable+enabled backends reports Render=false (control/media-only).
```

### 6.2 Group timeline & clock — `internal/clock`, `internal/group`
```go
// Shared timebase (reuse mpvsync clock): master_mono ≈ follower_mono + Offset.
type ClockSource interface { Offset() (d time.Duration, ok bool); MinDelay() (time.Duration, bool) }

// The synchronized quantity per group: stream sample index N at a master-mono instant.
type Timeline interface {
    // NowSample returns the group's playout sample index expected at "now", whether
    // playing, and ok=false when no sync yet. Sample index is in canonical-rate frames.
    NowSample() (sample int64, playing bool, ok bool)
}
```

### 6.3 Stream codec & FEC — `internal/stream/codec`, `internal/stream/fec`
```go
type Codec interface {
    ID() CodecID                                  // CodecID enum = {PCM=0, OPUS=1}
    Encode(pcm []float32) ([]byte, error)         // master side
    Decode(payload []byte) ([]float32, error)     // follower side
}
type FEC interface {
    ID() FECID                                    // None=0, XORParity=1, Duplicate=2
    // Protect groups source packets and emits source+repair packets to send.
    Protect(seq uint64, pkt []byte) (out [][]byte)
    // Recover ingests a received packet; returns any newly-recoverable source packets.
    Recover(p Packet) (recovered []Packet)
}
```

### 6.4 Audio wire format — `internal/stream/wire`
Fixed-size header, big-endian, then payload. One **chunk** = one codec frame of
`FramesPerChunk`. **Appendix A.12 is the single source of truth for all tunables**
(frame size, FEC `k`/interleave, clock windows, timeouts, etc.); this README does not
restate those numbers — see A.12.
```
offset size field
0      4    magic 'ESND'
4      1    version (=1)
5      1    flags (bit0: repair packet, bit1: keyframe)
6      1    codecID
7      1    fecID
8      8    streamGen   (group stream generation; reset on media/seek change)
16     8    seq         (monotonic source packet sequence)
24     8    sampleIndex (first sample's canonical-rate frame index on the group timeline)
32     8    masterMono  (master monotonic ns when this chunk was sourced)
40     2    payloadLen
42     2    rate/100    (e.g. 480 => 48000 Hz; sanity/redundancy)
44     ...  payload (codec frame, or FEC repair data)
```
Clock packets reuse mpvsync's 40-byte format unchanged.

### 6.5 Config document — `internal/state` (replicated, versioned, LWW)
Canonical JSON shape (full schema + semantics in [07](./07-config-and-replication.md)):
```go
type ConfigDoc struct {
    Version   uint64            `json:"version"`
    Cluster   ClusterInfo       `json:"cluster"`   // name, CA cert (public), created
    Auth      AuthConfig        `json:"auth"`      // admin pw hash, api key hashes
    Nodes     []NodeRecord      `json:"nodes"`     // id, name, certPEM(pub), addrs[], hwDelayUs, channel, gainDb, capabilities
    Groups    []GroupRecord     `json:"groups"`    // id, name, memberNodeIDs[], profile, media(selected source, loop), playing, transport
    UpdatedBy string            `json:"updatedBy"` // node id (merge tiebreak)
}
type NodeRecord struct {
    ID, Name   string   `json:"id","name"`
    CertPEM    string   `json:"certPem"`     // node's signed cert (public) — distributes trust
    Addrs      []string `json:"addrs"`       // known IPs (drives the allowlist)
    HWDelayUs  int      `json:"hwDelayUs"`   // local hardware/output latency trim
    Channel    string   `json:"channel"`     // "stereo"|"left"|"right"
    GainDB     float64  `json:"gainDb"`
    Caps       Capabilities `json:"caps"`    // EFFECTIVE = detected(runtime) ∩ enabled(config); negotiation input
}
// Capabilities is a node's advertised, effective ability set: probed at runtime, then
// masked by per-node config so a present-but-unwanted path can be turned off.
type Capabilities struct {
    Render       bool     `json:"render"`  // false => control/media-only node (no audio sink), e.g. NAS/docker
    Sinks        []string `json:"sinks"`   // usable+enabled output backends: "alsa","exec:aplay"
    EncodeCodecs []string `json:"encode"`  // codec names this node can ORIGINATE (as a group master): "pcm","opus"
    DecodeCodecs []string `json:"decode"`  // codec names this node can PLAY (as a listener): "pcm","opus"
    FEC          []string `json:"fec"`     // FEC names supported: "none","xorParity","duplicate"
    MaxRate      int      `json:"maxRate"` // highest sample rate this node will run
}
// JSON (ConfigDoc + API) uses **string enums** for codec/fec/transport ("pcm"|"opus",
// "none"|"xorParity"|"duplicate", "udp"|"tcp"). Integer CodecID/FECID exist ONLY at the
// §6.4 wire layer, mapped to/from these names via a name↔id registry.
```
The **allowlist** is derived from `Nodes[].Addrs` ∪ group membership: clock/audio
sockets drop packets whose source IP is not a current cluster member's address.

> **Schema completeness** (full typed schema in [07](./07-config-and-replication.md)):
> `GroupRecord{ID, Name, MemberNodeIDs[], Profile, Media{File, Loop}, Playing bool, Transport}`.
> `Playing bool` is replicated in the ConfigDoc, separate from `Media{File,Loop}`; play/stop
> endpoints flip it, and on master failover the new master reads `Playing` and resumes.
> The group's `TransportProfile` exposes `framesPerChunk/fecK/interleave` (read-mostly) and
> carries codec/fec/transport as **string** values (not integer IDs) in JSON.
> `AuthConfig` also carries the **admin password hash**, **API-key hashes**, and the adoption **PIN hash**.
> The cluster **CA private key** is replicated to full nodes **in plaintext** (no sealing — limited LAN threat model, see D18 / [03](./03-adoption-takeover-security-pki.md)).
> A grow-only **`RevokedSet`** of superseded/forgotten cert **SHA-256 fingerprints** is merged as a **monotonic union** (not LWW), so a forgotten node cannot be resurrected by a stale replica.

### 6.6 HTTP/API conventions — `internal/web`
- Base `/api/v1`. All endpoints require **mTLS client cert (node)** OR a valid
  **admin session / API key** (browser path); see [03](./03-adoption-takeover-security-pki.md) §auth.
- **State-gated exceptions:** `POST /api/v1/setup` (first-init, uninitialized node only) and
  `POST /api/v1/login` (the credential exchange itself) cannot present a session/cert. The
  **bootstrap/adoption** endpoints (`/bootstrap/*`) live *outside* mTLS entirely and are
  PIN-gated (see [03](./03-adoption-takeover-security-pki.md)).
- JSON only. Error envelope: `{"error":{"code":"string","message":"..."}}`, proper HTTP status.
- Optimistic concurrency on config writes: client sends `If-Match: <version>`; `409` on conflict.
- Any node serves the full API; cross-node operations are proxied node→node over mTLS.
- **Protocol-version policy:** all nodes in a cluster share one protocol epoch; mixed epochs
  are unsupported (adoption refuses an epoch mismatch; upgrade together). `softwareVersion`
  is UI-visibility only.
- Full reference: [08-http-api-reference.md](./08-http-api-reference.md).

### 6.7 UI navigation — `web/`
Top-level screens (detail in [09](./09-ui-screens.md)):
`Setup Wizard` (uninitialized) · `Dashboard` (groups + nodes at a glance) ·
`Cluster` (discover/adopt/takeover/forget) · `Groups` (create/assign members/profile/
media transport) · `Node detail` (channel, hardware delay, gain, identity — for *any*
node) · `Media` (browse `data/`, select, play/stop/loop) · `Settings` (admin password,
API keys, cluster info).

---

## 7. Document index

| File | Scope |
|---|---|
| [01-architecture-and-packages.md](./01-architecture-and-packages.md) | Expanded architecture, package responsibilities, key sequence diagrams (boot, adopt, group play, failover). |
| [02-cluster-discovery-membership.md](./02-cluster-discovery-membership.md) | Broadcast/mDNS discovery, memberlist gossip, per-group master election, failover. |
| [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md) | mTLS, cluster CA/PKI, adoption PIN exchange, takeover/forget, allowlist, UI/API auth, threat model. |
| [04-clock-and-groups.md](./04-clock-and-groups.md) | Per-group clock sync, the group engine, timeline, profile negotiation, group lifecycle. |
| [05-audio-streaming-protocol.md](./05-audio-streaming-protocol.md) | Master decode/encode/FEC/unicast; receiver recovery; codec & FEC details; buffering; wire protocol semantics. |
| [06-audio-output-scheduling.md](./06-audio-output-scheduling.md) | AudioSink (ALSA + fallback), hardware-buffer-aware scheduling, the corrected content-domain drift PI loop, channel/gain/HWDelayUs. |
| [07-config-and-replication.md](./07-config-and-replication.md) | Full ConfigDoc schema, gossip replication/merge, persistence, allowlist derivation, IP handling. |
| [08-http-api-reference.md](./08-http-api-reference.md) | Every endpoint: cluster ops, node config, groups, media, auth — with request/response bodies. |
| [09-ui-screens.md](./09-ui-screens.md) | Every screen: layout, components, states, flows (wizard, dashboard, cluster, groups, node, media, settings). |
| [10-roadmap-and-dumb-nodes.md](./10-roadmap-and-dumb-nodes.md) | Build phases, dumb-node (microcontroller) accommodation, open questions. |
| [11-build-ci-and-release.md](./11-build-ci-and-release.md) | Build prerequisites, pure-Go cross-compilation, runtime requirements, the GitLab CI pipeline (`.gitlab-ci.yml`) + downloadable binary artifacts/releases. |
| [A-appendix-algorithms-and-pinned-choices.md](./A-appendix-algorithms-and-pinned-choices.md) | **Self-containment appendix:** inlined reuse algorithms (clock, resampler, drift, election, gossip, ring, allowlist), concrete adoption-PIN primitive, pinned Go deps, starting parameters, per-phase definition-of-ready. |

---

## 8. Hardware targets & resource budget

**Reference platform:** quad-core ARM Cortex-A53 (Raspberry Pi 3 A+ / Zero 2 W class),
**512 MB RAM**, **64-bit Raspberry Pi OS (arm64)**. A full node runs comfortably on this
— audio is orders of magnitude lighter than a video pipeline, so RAM/CPU are **not** the
binding constraints.

### Why 512 MB / quad-A53 is enough
- **One stream per node.** A node belongs to exactly one group and plays one stream.
- **Shared-encode master.** A master decodes + encodes **once**; only the per-listener
  UDP *send* scales with N (cheap). It is not N× encode work.
- **Unencrypted, allowlisted audio plane** ⇒ **no per-packet crypto** on the bulk data.
  (BCM2837 / RP3A0 lack the ARMv8 crypto extensions, so AES/SHA are software — but that
  only touches the low-volume mTLS *control* plane.)

| Role | Work | Budget on the reference platform |
|---|---|---|
| Follower (listener) | decode (PCM/Opus) + near-unity resample + sink | single-digit % of one core; ring ~115 KB/stream |
| Master (origin) | source-decode + encode-once + FEC + N× UDP send | PCM no encode; Opus ~10–20 %; sends cheap |
| Process RAM | Go heap + embedded UI + buffers | tens of MB (OS Lite ~120–150 MB; ~300 MB free) |

### The real constraints (none are the Pi's compute)
1. **WiFi — especially the master's uplink.** Unicast means master out = N × bitrate; on
   congested 2.4 GHz, mastering many PCM listeners stresses the *air*, not the CPU. For
   large groups prefer **Opus** (~128 kbps × N) or a **wired master**; prefer 5 GHz
   (3 A+) / Ethernet; the 300 ms buffer + FEC absorb jitter. **Plan group size against
   the master's link, not its CPU.**
2. **Audio output hardware.** The analog jack is PWM-noisy and the Zero 2 W has none —
   plan on an **I2S DAC HAT** (HiFiBerry/IQaudio) or HDMI → a clean ALSA device.
3. **64-bit OS (arm64).** Best for Go perf and for the **direct kernel ioctl ALSA** path (D12),
   which assumes direct hardware access (no sound server owns the card).

**Minimum floor:** dual-core ARM, 256 MB free RAM, arm64. Below that, prefer a
**sink-less role** (D17) or the future **dumb-node** profile ([10](./10-roadmap-and-dumb-nodes.md)).
