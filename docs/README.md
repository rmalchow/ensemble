# ensemble — self-organizing multiroom audio

A multiroom audio system. Every node runs the same single binary. Nodes find each
other automatically (mDNS + gossip), organize into **groups**, and play audio in
sync: the group master runs an **audio source** that decodes media into
timestamped PCM (or Opus); every member — including the master itself —
**subscribes** to that source and plays the stream out through a clock-disciplined
sink. A master-anchored clock plus a per-node rate servo keeps playout aligned.

Design goal: **simple and basic**. One binary, four ports, no external services,
no database, no PKI. State is replicated via gossip; everything heals itself.

---

## 1. Node identity

- **Node ID**: 16 random bytes, lowercase hex (32 chars). Generated on first
  start, persisted to `DATA_DIR/node.json`, **immutable** forever after.
- **Node name**: display name, initially the first 8 chars of the node ID.
  Changeable at runtime via API/UI; persisted in `node.json` and replicated.
- **Volume**: per-node playback gain `0.0–1.0` (default `1.0`), applied
  **continuously** as software gain in the sink (§8.5) — the UI changes it
  live, no restart. Persisted in `node.json`, replicated in the node record.
- **Output delay** (`outputDelayMs`): per-node hardware latency calibration
  (default 0, clamped ±500 ms) for fixed downstream delay the system cannot
  measure — the pipe player's internal buffer, DAC/amp/Bluetooth chains.
  Subtracted from the playout deadline (§8.5). Changing it re-anchors playout
  via the RESTART/re-prime path (§8.6) — a sub-second stream restart.
  Persisted in `node.json`, replicated.
- **Following** (`following`): the node's last-known follow target as a 32-hex
  node id, or `""` for a solo master (default). Persisted in `node.json` so a
  node that temporarily disappears **rejoins its previous group on return**
  (§5; D45) — its live value is the replicated node-record field, this is only
  the boot seed + last-known. An absent or malformed value loads as `""` (never
  fatal). Written on every follow/unfollow/takeover/self-heal change.
- **Capabilities**: reported in the replicated node record. All of these are
  **probed at runtime on each start** — a `$PATH` scan for exec tools plus
  `dlopen` probes for optional shared libraries (`libopus.so.0`,
  `libasound.so.2`, via purego — no cgo, no build variants; see §8.3/§8.5).
  One universal binary; a host without a library simply reports the
  capability off:
  - `playback`: whether a real PCM output backend is available (§8.5)
  - `codecs`: streaming codecs (`["pcm"]`, + `"opus"` when libopus loads)
  - `backends`: sink backends usable on this host (§8.5),
    e.g. `["alsa","exec","null","file"]` (alsa only when libasound loads)
  - `sources`: media-source schemes this node can serve (§6.1),
    e.g. `["file","http","input"]`
  - `formats`: local media formats it can decode (`["wav","mp3","flac"]`)

  The reported capabilities are **effective** = probed minus operator-disabled
  (D40): a node may turn off `playback` (output), `opus` (codec), or `input`
  (capture) via `PATCH /api/node {disabled}`, persisted in `node.json`
  (`disabled:[…]`) and replicated. Disabling `playback` swaps the sink to the
  null backend live; disabling `opus` drops it from `codecs` (opus sessions
  including the node are then rejected); disabling `input` drops it from
  `sources`. Re-enabling restores the probed capability. The UI shows these as
  tri-state chips (available / unavailable / disabled).

## 2. Ports

Four configurable base ports. **Bind-or-increment**: try the base port; if it
is taken, increment by 1 and retry, up to 64 attempts, then fail. The *actually
bound* ports are what the node advertises (mDNS TXT + gossiped node record).

| Name          | Default | Protocols   | Purpose                                  |
|---------------|---------|-------------|------------------------------------------|
| `HTTP_PORT`   | 8080    | TCP         | REST API, WebSocket, SPA, node proxy      |
| `STREAM_PORT` | 9090    | TCP **and** UDP | member-side stream reception **and** clock sync (UDP, multiplexed by packet type) |
| `SOURCE_PORT` | 9200    | TCP **and** UDP | audio source: subscriptions + stream control (§8.7); inbound only matters while the node is a group master |
| `GOSSIP_PORT` | 7946    | TCP **and** UDP | memberlist gossip                         |

TCP and UDP for a given name must end up on the **same** port number: a
candidate port is accepted only if *all* its sockets bind; otherwise both are
closed and the next port is tried.

mDNS additionally uses standard multicast UDP 5353 (via the zeroconf library);
this is not a configurable ensemble port.

Configuration is via flags with env-var fallbacks:
`--http-port` / `ENSEMBLE_HTTP_PORT`, `--stream-port` / `ENSEMBLE_STREAM_PORT`,
`--source-port` / `ENSEMBLE_SOURCE_PORT`, `--gossip-port` /
`ENSEMBLE_GOSSIP_PORT`, `--data` / `ENSEMBLE_DATA_DIR` (default `./data`),
`--media` / `ENSEMBLE_MEDIA_DIR` (default `DATA_DIR/media`), `--name` (initial
node name, only applied on first start).
Additionally: `ENSEMBLE_OUTPUT` (env only) selects the PCM output backend by
name (§8.5; `auto` default | `alsa` where loadable | `exec` | `null` |
`file:<path>`), and `--join` / `ENSEMBLE_JOIN` (dev only) seeds gossip with a
comma-separated `host:gossipPort` list for multicast-less environments (tests).

## 3. Discovery

Two mechanisms, both always on:

1. **mDNS** (`grandcat/zeroconf`): every node registers service
   `_ensemble._tcp` with TXT records `id=<nodeId>`, `gossip=<port>`,
   `http=<port>`, `stream=<port>`, `source=<port>`. Every node browses
   continuously; any peer not yet in the gossip cluster is joined via its
   discovered address.
2. **Gossip** (`hashicorp/memberlist`): carries liveness *and* the replicated
   cluster state (§4). Once any two nodes have met via mDNS, state spreads to
   everyone transitively.

### 3.1 Addresses and observed-IP reporting

Every node lists its own (non-loopback, up) interface addresses in **CIDR
notation** (`192.168.1.17/24`, `fd00::5/64`) in its node record.

Self-reported addresses can be wrong (containers, VPNs, multi-homing). So
nodes also report what they **actually see**: whenever a node receives gossip
or HTTP traffic from a peer, it records the peer's remote IP as an *observed
address* (`peerId → ip, lastSeen`), and publishes its observation map in its
own node record.

When choosing an address to dial a peer (proxy, clock, stream subscription,
post-boot gossip), candidates are taken from the peer's self-reported CIDR
list **intersected with the cluster's observations**: an IP that no node has
ever observed is ignored. Observed addresses are preferred in order of most
recent observation. Two bootstrap exceptions: the *initial* gossip join dials
the address mDNS actually answered from, and a peer with no observations yet
falls back to its self-reported list (it tightens to observed-only as soon as
any traffic flows).

The stream path itself needs no address resolution at all: subscribers dial
the master's `SOURCE_PORT`, and the source streams back to **the address each
subscription actually came from** (§8.7) — observed-by-construction.

## 4. Replicated cluster state

A single eventually-consistent document replicated to all nodes through
memberlist broadcasts + push/pull sync. No leader, no consensus; merging is
**last-writer-wins per record** using a per-record monotonic version
(lamport-ish counter, tie-broken by node ID).

Records — **record keying** (D44): group **names** are keyed by the member-set
**XOR** (an override names a specific COMBINATION of rooms); group **settings**
and **playback** are keyed by the **master's node id** (= the live group id, §5):

- **Node records** — owned and only ever written by the node itself:
  `id, name, volume, outputDelayMs, addrs (CIDR), httpPort, streamPort,
  sourcePort, gossipPort, capabilities, following (nodeId or empty),
  observed (peerId → {ip, lastSeenUnix}), version, updatedAt`
- **Group names (override map)** — map `memberXOR → {name, version, updatedAt}`;
  written by whichever node renames the group (LWW). XOR-keyed so a name follows
  a specific member COMBINATION across master changes + re-forming (D44). An empty
  name clears the override (back to the §5 derived label).
- **Playback status** — per group, keyed by the **master id** (D44), written only
  by that group's master:
  `masterId → {state: idle|playing|paused, uri, startedAt, positionSec, codec,
  transport, version, updatedAt}`. `paused` (D39) is a frozen session: the
  master keeps the media source + session/gen alive but stops releasing frames;
  members treat anything other than `playing` as inactive and unsubscribe.

Liveness (`lastSeen`) comes from memberlist itself; alive/dead is not part of
the replicated doc. Node records and playback entries not updated for **30 days**
are purged (checked hourly). **Group names and group settings are exempt from the
purge entirely** (D41): they are a lookup table kept indefinitely, so a specific
member combination keeps its name whenever it reforms.

The group **override-names** map AND this node's **own group-settings record**
(keyed by self id, since the group id == the master id, D44) are persisted to
`DATA_DIR/cluster.json` (D41, amended by D47) — full records incl. version+writer
so the LWW merge applies on reload — loaded at start before joining gossip and
saved debounced (and on clean shutdown). A node that was offline therefore still
knows every group name it ever saw, and a master that restarts re-forms its group
with its last codec/transport/bufferMs instead of cluster defaults. Only the
node's OWN settings record persists (other groups' settings are master-keyed live
state owned by other nodes); node records and playback stay unpersisted
(runtime/replicated).

## 5. Groups

Groups are **derived, not stored**. The only stored fact is each node's own
`following` field:

- `following == ""` → the node is **master of its own group**.
- `following == M` → the node is a follower of node `M`.

Derivation, recomputed by every node from the replicated state + liveness:

- For each alive node `M` with `following == ""`: group
  `members = {M} ∪ {n alive : n.following == M}`, `master = M`.
- A node whose `following` points at a dead node, an unknown node, or a node
  that is itself following someone, behaves as solo — and additionally
  **resets its own `following` to ""** after a 10s grace period (self-heal).

**Rejoin on return** (D45). Each node persists its `following` to `node.json` and
re-seeds its own record with it at boot, gossiping it from the start exactly as if
it had been set live. So a node that temporarily disappears (reboot, crash, brief
network drop) **rejoins its previous group automatically** when it comes back: if
its old master is still alive and a master, the same derivation re-forms the group;
if the old master is absent on return, the dangling follow is just a stale `following`,
so the 10s self-heal grace fires and the node settles as its own solo group (and
clears the persisted `following` back to `""`). No special rejoin path — the existing
derivation + self-heal do all of it. The grace is measured from when the node first
*observes* the dangling follow, not from process start, so slow gossip convergence
never insta-clears a follow that is merely still propagating.

**Group ID = the master's node id** (D44). A solo node's group ID equals its
node ID. Keying the group — and its master-written playback + settings records
(§4) — by the master id means membership churn (a member joining/leaving, master
unchanged) never orphans those records: the id only changes on a *master* move,
which is a takeover that stops the session first. This replaces the former
XOR-of-members group id.

**Group label** — a group's display name is resolved as follows (D44):

- **Explicit override**: the XOR-of-members-keyed name map (§4). An override
  names a specific COMBINATION of rooms, so it survives master changes and group
  re-forming. When one exists for the current member set it is the label, and
  `GroupView.NameIsDerived` is `false`.
- **Derived label** (no override): computed server-side in `DeriveGroups` from
  the member NAMES — sorted, joined with `" + "`, capped at the first 3 names
  then `" +N more"` (e.g. `"bedroom + kitchen + living room +2 more"`); a solo
  group is just the node's name; a member missing from the snapshot falls back to
  its 8-char short id. `GroupView.NameIsDerived` is `true`. The UI renders derived
  labels muted/italic.

### 5.1 Joining (“I follow you”)

“Tell alice to join bob” = `POST /api/follow {"target":"<bobId>"}` on alice
(directly or via proxy). Alice verifies bob is alive and is a master (not
following anyone), then sets `following = bobId` and gossips it. Alice's solo
group ceases to exist; bob's group now contains bob + alice; **bob stays
master**. Joining while following someone else just re-points `following`.
`POST /api/unfollow` resets to solo.

If bob disappears (memberlist declares him dead), every follower reverts to
its default — master of a group of 1 — via the self-heal rule above.

### 5.2 Master takeover (“make master”)

If alice (a follower) wants to play local media, the mastership must move to
her first. `POST /api/group/master {"node":"<aliceId>"}` on any group member:

1. The receiving node forwards the request to the current master (proxy).
2. The master stops any running playback session.
3. The master copies the group's current **settings** record to the new master's
   key (D44: settings are master-keyed, so they would otherwise reset to defaults
   — one extra `SetGroupSettings` carries codec/transport/bufferMs over). Playback
   does NOT carry (step 2 stopped it).
4. The master instructs every member (including itself) over HTTP:
   `POST /api/follow {"target":"<aliceId>"}` — and alice: `POST /api/unfollow`.
5. Group membership is unchanged, so the member-set XOR — hence the **name
   override** — is unchanged; only the master moved, so the **group ID changes**
   to the new master's id (D44) and settings carry over (step 3).

Members that miss the command end up following alice late or self-heal to
solo; no further coordination. The UI exposes this as a **“make master”**
button; *playing local media from a follower in the UI does takeover + play
in one action*.

## 6. Media sources

What a master plays is a **URI**; the scheme selects an interchangeable media
source implementation (§6.1):

- `file:<relative path>` (or a bare path) — a file under the node's local
  media directory (`MEDIA_DIR`). `GET /api/media` lists playable files
  (recursive scan, extensions `.wav .mp3 .flac`, rescanned on request):
  `[{path, name, sizeBytes, modTime}]`. Paths are relative to `MEDIA_DIR`;
  path traversal outside it is rejected.
- `http://…` / `https://…` — a remote stream or file (e.g. internet radio),
  decoded by Content-Type / URL extension. No duration, no seek.
- `input:` — the node's local capture input (line-in/mic), recorded via an
  exec-capture backend (`pw-record`/`arecord` pipe, mirroring §8.5 playback).

Any node's media can be browsed cluster-wide through the proxy (§9.3).

### 6.1 Source abstraction

All media sources implement one contract: produce canonical PCM (§8.1) frames
via `ReadFrame(dst)` until `io.EOF`, plus `Close`. A scheme-keyed factory
(`file`, `http`, `input`) creates them; a node's available schemes are
reported in its capabilities (§1). Two pacing classes:

- **Pull-paced** (`file`): decode runs ahead of the release ticker; EOF is the
  natural end of the session.
- **Live-paced** (`http`, `input`): data arrives in real time and never EOFs;
  the session ends only on `stop`. If the source momentarily underflows
  (network stall, capture hiccup), the release ticker emits silence frames —
  the stream's seq/pts cadence **never stalls**.

Adding a new kind of source (e.g. Spotify Connect, snapcast pipe) means
implementing this one interface and registering a scheme.

## 7. Clock sync

Master-anchored, NTP-style, over **STREAM_PORT UDP** (multiplexed with stream
reception by packet type, §8.4).

- The **master** answers clock requests: echoes the client's t1, stamps
  receive time t2 and send time t3 (monotonic-derived nanoseconds).
- Every member — **including the master itself, against localhost** — runs a
  clock **follower**: one request per second, computes offset
  `((t2−t1)+(t3−t4))/2`, keeps the 5 best-RTT samples of the last 30 and uses
  their median offset. Until the first sample arrives the member is *unsynced*
  and must not start playout.

The translated master clock is what playout (§8.5) schedules against. When
mastership changes, followers discard samples and resync (generation counter,
§8.4).

## 8. Audio pipeline

### 8.1 Canonical format

Everything streams as **48 kHz, stereo, s16le** PCM, in **20 ms frames**
(960 samples/ch, 3840 bytes). Decoders convert: mono → dup to stereo, other
rates → linear-interpolation resample. Good enough; simple.

Every frame on the wire is both **counted and timestamped**: `seq` (a frame
counter — ordering, loss detection, FEC block identity) and `pts` (a
presentation timestamp in master-clock nanoseconds — playout scheduling).
They serve different layers and both are needed (§8.5).

### 8.2 Source (master side)

On `play`, the master starts an **audio source server** on its `SOURCE_PORT`:

- It opens the media source for the URI (§6.1) and releases canonical frames
  in real time (20 ms ticker), each stamped
  `pts = sessionStart + frameIndex·20ms` (master-clock ns), where
  `sessionStart = now + leadMs`.
- It maintains a **subscriber registry**: every group member — *including the
  master's own sink, which subscribes over loopback exactly like everyone
  else, no special handling* — subscribes via the stream control protocol
  (§8.7). Each released frame is sent to every live subscriber.
- It keeps a **ring buffer of recently released frames** sized
  `max(2 × bufferMs, 1 s)`. When a subscriber joins (or rejoins after getting
  lost), the source **burst-primes** it: it replays every ring frame whose
  playout deadline (`pts + bufferMs`) is still in the future — older frames
  are useless to the newcomer and are skipped. Over UDP the burst is paced at
  ~4× realtime (one frame per ~5 ms) so it outruns the stream without
  flooding the network; over TCP it is written back-to-back (flow control
  paces it). After the burst, the subscriber receives live frames like
  everyone else.
- It counts and surfaces **source stats**: current subscriber count, total
  connects, restarts (re-prime requests), and primes served (§9.1).

### 8.3 Codec — group setting `codec: pcm | opus` (default `opus`, D42)

- `opus` (**default**): 20 ms Opus at 128 kbps (~320 B/frame, so a stream
  datagram ≈ 344 B stays **under one MTU** and never IP-fragments). No cgo, no
  build variant: **libopus is loaded at runtime** (`dlopen` via purego,
  `libopus.so.0`); if it isn't loadable on a host, that node reports no `opus`
  capability. The **master encodes** (after `ReadFrame`, before fan-out — one
  encode for all subscribers); **every member decodes** (between receive and the
  sink, which always consumes canonical PCM).
- `pcm`: raw frame payload (3840 B). One datagram is `24 + 3840 = 3864 B`, which
  **IP-fragments into ~3 packets** — on lossy Wi-Fi, losing any fragment drops
  the whole frame and the per-frame XOR FEC cannot recover it. Use pcm only on
  reliable links.
- **Codec negotiation (D33/D42)**: the master picks the **effective codec** —
  the wanted `settings.codec` iff every CURRENT member's effective capabilities
  (probed minus operator-disabled, §1) include it AND the master can encode it,
  else **pcm** (always universal). This is computed at every session start (play,
  resume, settings change), so `play` **never fails** for lack of opus — opus is
  the default, pcm is the universal fallback. A downgrade is logged
  (`codec negotiated wanted=opus got=pcm lacking=[names]`); the replicated
  playback record carries the EFFECTIVE codec.
- **Mid-session renegotiation (D33)**: if a member disables opus, or a non-opus
  node joins, while an opus session is playing, the master's reconcile detects
  the running codec is no longer supported by all members and **downgrades the
  live session to pcm in place** — bump gen, drop the encoder, re-arm the source,
  broadcast RECONFIG (members reconnect), resume from the current position. Only
  downgrades happen automatically mid-session; an upgrade (opus became newly
  possible) applies on the next play/settings change. An *explicit* `codec: opus`
  group setting is still validated against the master's own capability at
  `POST /api/group/settings`.

### 8.4 Transport — group setting `transport: udp | tcp` (default `udp`)

Common frame header (before payload):
`magic(1) | type(1) | gen(4) | seq(8) | pts(8) | payloadLen(2)`.
`gen` is the session generation (bumped on every new play / master change /
settings change); receivers drop frames from stale generations.

Members **subscribe** to the master's source (§8.7); the source streams back
to the address each subscription came from:

- **udp**: the subscriber HELLOs from its own STREAM_PORT UDP socket to the
  master's SOURCE_PORT; audio then flows source → that observed addr:port,
  one frame per datagram, plus **XOR FEC**: after every 4 audio frames, one
  parity datagram (XOR of the 4 payloads, padded) — any single loss per block
  of 5 is recovered. Receiver keeps a small reorder/recovery window. Packet
  types multiplex the member's STREAM_PORT UDP socket: `0x01` audio frame,
  `0x02` FEC parity, `0x10` clock request, `0x11` clock reply; control types
  (§8.7) ride the SOURCE_PORT.
- **tcp**: the subscriber dials the master's SOURCE_PORT TCP; the persistent
  connection carries control frames (§8.7) and length-prefixed audio frames
  master→member. No FEC (TCP retransmits). The member-side STREAM_PORT TCP
  listener plays no role in audio.

**Protocol version / compatibility (v1, D46).** The **magic byte `0xE5` IS the
version marker**: every framed packet starts with it. Receivers **MUST ignore
unknown packet `type` values** (forward-compatible: new optional types may be
added within v1 without breaking older receivers). A **future incompatible
revision changes the magic byte**, not the header layout — so a single leading-
byte check is both the framing sanity check and the version gate. This is what
lets a *protocol-minimal* receiver (e.g. ESP32-S3 firmware, see
`docs/DUMB-CLIENT.md`) interoperate without tracking the cluster: it implements
only the packet types it needs and drops the rest.

**Settings are editable per group in the UI and persist with the master** (D47).
The group card exposes inline `codec` / `transport` / `bufferMs` controls; a
change POSTs the full trio to `POST /api/group/settings` addressed to the group's
master (proxied, so it works from any node) and applies live via RECONFIG (§8.7).
Because the group id == the master id (D44), the master persists its OWN settings
record (codec/transport/bufferMs, full LWW record) to `DATA_DIR/cluster.json`
(§4); on restart it re-forms its group with those settings instead of the
cluster defaults.

### 8.5 Sink & playout (every member)

Every member (incl. master) runs: subscriber → jitter buffer → **rate-adaptive
resampler** → **volume gain** → output backend.

**Three clocks** are in play and the design keeps them separate:

1. the **master clock** — the shared reference; clock sync (§7) gives every
   member a translation `MasterToLocal(t)`;
2. the **local OS clock** — what the member sleeps and schedules against,
   corrected by that translation;
3. the **DAC crystal** — the output device consumes samples at *its own* idea
   of 48 kHz, typically ±20–100 ppm off, and never looks at clocks 1 or 2.

**Coarse scheduling is timestamp-driven** (clocks 1→2): the playout loop
translates each frame's `pts` and writes it to the backend so audio for `pts`
hits the device at `pts + bufferMs − outputDelayMs` (`bufferMs`: group
setting, default **150 ms**; `outputDelayMs`: the node's hardware calibration,
§1). Missing frames after FEC/reorder → play silence (gap), never block.
Frames arriving too late are dropped (counted). This anchors session start,
mid-song joins, and recovery. A live `outputDelayMs` change re-anchors via
RESTART (§8.6).

**Volume** is applied last before the backend: per-sample software gain from
the node's replicated volume, read atomically each frame and linearly ramped
over one 20 ms frame on change (no zipper noise) — fully live, the UI drives
it directly.

**Fine cadence is servo-driven** (clock 3): once the device pipeline is full,
writes are paced by the DAC, not by the scheduler — and crystal skew of
±50 ppm drifts ~3 ms/min, which would slowly drain/fill the jitter buffer and
pull rooms audibly apart. So every sink runs a **continuous rate servo**: a
skew estimator (cumulative samples consumed vs master-clock elapsed time,
averaged over ~3 s) feeds a small PI controller whose output — clamped to
±300 ppm and slewed — drives a **4-tap (Catmull-Rom) fractional
resampler** between jitter buffer and backend. The correction magnitudes are
far below audibility. This is *not* an underrun reaction; it runs at all
times to prevent drift. Real underruns are handled by silence insertion and,
if starvation persists, the RESTART path (§8.6).

**Output backends** are named, interchangeable implementations behind one
interface (`Write(frame)`, `Close`), selected by `ENSEMBLE_OUTPUT`
(`auto` picks the best available: **alsa → exec → null**):

- `alsa` — raw device access via **runtime-loaded** libasound
  (`dlopen("libasound.so.2")` via purego — no cgo, no build variant, no dev
  headers; gracefully absent on hosts without the library). Uses the simple
  ALSA API (`snd_pcm_open/set_params/writei/recover/close`).
- `exec` — the basic fallback: pipe raw s16le 48k stereo into the first of
  `pw-play`, `pw-cat -p`, `aplay`, `paplay` found on `$PATH`. Zero cgo.
- `null` — timed discard; tests and playback-less nodes.
- `file:<path>` — raw PCM append; debugging.

A backend **may** additionally implement `DeviceDelay() (nanos, ok)` — the
exact amount of audio queued between a `Write` and the speaker. The servo
type-asserts for it: alsa implements it (`snd_pcm_delay`) and skew
measurement is exact; without it (exec pipes) the servo falls back to
backpressure inference, and per-device constant offsets limit absolute
inter-room accuracy to roughly ±10–20 ms. Available backends are reported in
capabilities (§1); `playback` is true when a real (non-null) backend is
usable.

**ALSA device selection** (D37). The alsa backend opens a *named* device, not
always `default`. Each node enumerates its playback-capable PCMs once at startup
by parsing `/proc/asound/pcm` (zero extra deps; empty when libasound is not
loadable or the file is absent) and reports them as
`outputDevices: [{id, desc}]` on its `/api/cluster` record, alongside the
currently-selected `outputDevice` (default `"default"`, persisted in
`node.json`). `PATCH /api/node {outputDevice}` validates the id against the
node's own enumerated list (or `"default"`), persists it, replicates it, and —
when the active backend is alsa — reopens that backend for the new device and
hot-swaps it into the live sink (a brief audio blip; the session is *not*
restarted). The exec backend ignores the device selection in v1 (it plays to its
player tool's own default).

### 8.6 Stop / end / getting lost

`stop` (or natural end of a pull-paced source) bumps the generation,
broadcasts a RECONFIG/stop control (§8.7), and clears playback status.
A subscriber whose frames cease for **> 2 s** (watchdog) first sends
**RESTART** to the source — "I got lost, re-prime me" — and resumes from the
burst; if the source stays silent (master died), the subscriber gives up,
unsubscribes locally, and normal group self-healing (§5) takes over.

### 8.7 Stream control (SOURCE_PORT)

A minimal signaling protocol between subscribers and the source, using the
same header framing (§8.4) with control packet types. Over UDP, control
datagrams go subscriber → master's SOURCE_PORT (sent from the subscriber's
STREAM_PORT socket, so the reply/stream path is observed-by-construction);
over TCP they are frames on the subscription connection.

| Type | Name | Direction | Meaning |
|---|---|---|---|
| `0x20` | HELLO | sub → src | subscribe (or keepalive); flag: prime-me |
| `0x21` | BYE | sub → src | "I am leaving, stop sending" |
| `0x22` | RESTART | sub → src | "I got lost, re-prime and resume" |
| `0x23` | RECONFIG | src → sub | "settings/session changed: re-fetch group settings, resubscribe under the new generation" (also doubles as the stop notice with gen 0… payload flag) |

HELLO repeats every **5 s** as keepalive; the source expires subscribers
unseen for **15 s**. When group settings change mid-session (codec,
transport, bufferMs), the master bumps the generation, broadcasts RECONFIG,
and subscribers reconnect with the new settings read from the replicated
group settings — settings changes therefore apply **live**, not at next play.

## 9. HTTP API (Echo)

All under `/api`. JSON. No auth (trusted LAN, v1).

### 9.1 REST

| Method/path | Action |
|---|---|
| `GET  /api/status` | this node: id, name, ports, role, group, sink/clock stats, and — when this node runs a source — `source: {clients, connects, restarts, primes}` |
| `PATCH /api/node` | `{name?, volume?, outputDelayMs?, outputDevice?, disabled?}` → rename / set live volume (§8.5) / set output-delay calibration (re-anchors playout) / select the ALSA output device (§8.5, live backend swap) / toggle operator-disabled features (D40: subset of `playback`/`opus`/`input`) on this node |
| `GET  /api/cluster` | replicated state, resolved: nodes (alive/dead, addrs, observed), derived groups (id, name, master, members, playback, source stats) |
| `GET  /api/media` | list this node's local media |
| `POST /api/follow` | `{target}` → this node follows target (§5.1) |
| `POST /api/unfollow` | this node becomes solo master |
| `POST /api/group/name` | `{group, name}` → set a group's name override (any node may write); the override is stored under the group's current member-set XOR (D44). An **empty `name` clears** the override, reverting to the derived label (§5) |
| `POST /api/group/master` | `{node}` → takeover (§5.2) |
| `POST /api/play` | `{uri}` (back-compat: `{file}` ≡ `file:` URI) → master only: serve that source to the group. Non-masters reject with a hint to use takeover. |
| `POST /api/stop` | master only: stop group playback |
| `POST /api/pause` | master only: freeze the running session (D39); 409 when nothing is playing |
| `POST /api/resume` | master only: resume a paused session (D39); 409 when not paused |
| `GET  /api/group/settings` / `POST` | `{codec, transport, bufferMs}` for this node's group (master only for POST; applies live via RECONFIG §8.7) |

Sink stats in `/api/status` include `played, silence, lateDrop, staleGen,
synced, ratePPM, buffered` (the servo's current correction and jitter-buffer
depth).

### 9.2 WebSocket

`GET /api/ws` — pushes `{type:"cluster", data:<same as GET /api/cluster>}` on
every state change (debounced ≥ 250 ms) plus periodic (5 s) heartbeat with
playback position. The SPA renders exclusively from these events after load.

### 9.3 Node proxy

`/api/:nodeId/*` — when the first path segment after `/api/` is a 32-hex node
ID, the request is transparently proxied to that node's HTTP port (address
chosen per §3.1), e.g. `GET http://node1:8080/api/<aliceId>/media` lists
alice's media via node1. One hop only: proxied requests carry
`X-Ensemble-Proxied: 1` and a request with that header is never re-proxied.
`:nodeId` may also be a node *name* if unique.

## 10. Web UI (Svelte SPA)

Svelte 5 + Vite, plain JavaScript, no component library, minimal hand-written
CSS. Built to static files, embedded in the binary via `go:embed`, served at
`/` by every node. Connects to `/api/ws`, renders from cluster events.

Screens (single page, sections):
- **Groups** — cards per derived group: name (click to rename; a derived label
  renders muted/italic and clearing the field resets to derived, D44), members with
  master badge and a live **volume slider** each (PATCH via proxy, §9.1),
  playback status + position, source stats (listeners / reconnects) on the
  master, stop button, per-member **“make master”** button, drag-free “join”
  via a member's *Join group…* dropdown (issues follow), *Leave* button
  (unfollow).
- **Nodes** — all known nodes: name (click to rename — works for remote nodes
  via proxy), id, addresses, capabilities, alive/last-seen, stale indicator,
  volume slider, and an **output delay (ms)** calibration field.
- **Media** — node picker (any node, via proxy) → file list → **Play here**:
  plays that file on the selected node's group (does takeover first if the
  target node isn't its group's master). A URL field allows playing an
  `http(s)://` stream; an *Input* button plays the node's capture input.

## 11. Out of scope (v1)

Seek, auth/TLS, internet-facing operation, playlists, album
art/metadata, multiple simultaneous streams per group, hardware mixer volume
(volume is software gain in v1). (Per-group play/pause IS supported, D39.)

## 12. Repository layout

```
cmd/ensemble/        main: flag parsing, wiring, lifecycle
internal/id/         node/group IDs (gen, parse, XOR)
internal/config/     flags/env, data dir, node.json persistence
internal/dl/         runtime shared-library loading (purego dlopen/dlsym
                     probe; soft-fails to "capability off")
internal/netx/       bind-or-increment listeners, CIDR interface scan
internal/discovery/  mDNS register + browse
internal/cluster/    memberlist wrapper, replicated state (LWW), observed IPs
internal/group/      group derivation, follow/takeover, playback orchestration
internal/clock/      clock server + follower (UDP, via stream mux)
internal/source/     audio source server: subscriber registry, ring buffer,
                     burst prime, stream control listener, source stats
internal/stream/     frame wire codec, member-side UDP mux, subscriber client
                     (HELLO/keepalive/RESTART), UDP+FEC / TCP reception
internal/audio/      media sources: scheme registry, decoders (wav/mp3/flac),
                     http stream source, exec-capture input, resampler,
                     opus encoder/decoder (runtime-loaded libopus)
internal/sink/       jitter buffer, playout scheduler, rate servo + 4-tap
                     resampler, output backend registry (exec/null/file[/alsa])
internal/api/        Echo server: REST, WebSocket, proxy, SPA embed
web/                 Svelte SPA (vite project; `npm run build` → web/dist)
docs/                this spec, architecture notes (docs/arch/*.md)
```
