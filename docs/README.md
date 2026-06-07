# ensemble — self-organizing multiroom audio

A multiroom audio system. Every node runs the same single binary. Nodes find each
other automatically (mDNS + gossip), organize into **groups**, and play audio in
sync: the group master decodes local media and streams timestamped PCM (or Opus)
to every member — including itself — while a master-anchored clock keeps playout
aligned.

Design goal: **simple and basic**. One binary, three ports, no external services,
no database, no PKI. State is replicated via gossip; everything heals itself.

---

## 1. Node identity

- **Node ID**: 16 random bytes, lowercase hex (32 chars). Generated on first
  start, persisted to `DATA_DIR/node.json`, **immutable** forever after.
- **Node name**: display name, initially the first 8 chars of the node ID.
  Changeable at runtime via API/UI; persisted in `node.json` and replicated.
- **Capabilities**: reported in the replicated node record:
  - `playback`: whether a PCM output backend was found (`pw-cat`, `pw-play`,
    `aplay`, or `paplay` on `$PATH`)
  - `codecs`: codecs this build can encode/decode for streaming (`["pcm"]`;
    `"opus"` only if built with opus support — see §8.3)
  - `formats`: local media formats it can decode (`["wav","mp3","flac"]`)

## 2. Ports

Three configurable base ports. **Bind-or-increment**: try the base port; if it
is taken, increment by 1 and retry, up to 64 attempts, then fail. The *actually
bound* ports are what the node advertises (mDNS TXT + gossiped node record).

| Name          | Default | Protocols   | Purpose                                  |
|---------------|---------|-------------|------------------------------------------|
| `HTTP_PORT`   | 8080    | TCP         | REST API, WebSocket, SPA, node proxy      |
| `STREAM_PORT` | 9090    | TCP **and** UDP | audio stream (TCP or UDP+FEC) **and** clock sync (UDP, multiplexed by packet type) |
| `GOSSIP_PORT` | 7946    | TCP **and** UDP | memberlist gossip                         |

TCP and UDP for a given name must end up on the **same** port number: a
candidate port is accepted only if *all* its sockets bind; otherwise both are
closed and the next port is tried.

mDNS additionally uses standard multicast UDP 5353 (via the zeroconf library);
this is not a configurable ensemble port.

Configuration is via flags with env-var fallbacks:
`--http-port` / `ENSEMBLE_HTTP_PORT`, `--stream-port` / `ENSEMBLE_STREAM_PORT`,
`--gossip-port` / `ENSEMBLE_GOSSIP_PORT`, `--data` / `ENSEMBLE_DATA_DIR`
(default `./data`), `--media` / `ENSEMBLE_MEDIA_DIR` (default `DATA_DIR/media`),
`--name` (initial node name, only applied on first start).
Additionally: `ENSEMBLE_OUTPUT` (env only) overrides the PCM output backend
(`auto` default | `null` | `file:<path>`), and `--join` / `ENSEMBLE_JOIN`
(dev only) seeds gossip with a comma-separated `host:gossipPort` list for
multicast-less environments (tests).

## 3. Discovery

Two mechanisms, both always on:

1. **mDNS** (`grandcat/zeroconf`): every node registers service
   `_ensemble._tcp` with TXT records `id=<nodeId>`, `gossip=<port>`,
   `http=<port>`, `stream=<port>`. Every node browses continuously; any peer
   not yet in the gossip cluster is joined via its discovered address.
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

When choosing an address to dial a peer (proxy, clock, stream, post-boot
gossip), candidates are taken from the peer's self-reported CIDR list
**intersected with the cluster's observations**: an IP that no node has ever
observed is ignored. Observed addresses are preferred in order of most recent
observation. Two bootstrap exceptions: the *initial* gossip join dials the
address mDNS actually answered from, and a peer with no observations yet
falls back to its self-reported list (it tightens to observed-only as soon as
any traffic flows).

## 4. Replicated cluster state

A single eventually-consistent document replicated to all nodes through
memberlist broadcasts + push/pull sync. No leader, no consensus; merging is
**last-writer-wins per record** using a per-record monotonic version
(lamport-ish counter, tie-broken by node ID).

Records:

- **Node records** — owned and only ever written by the node itself:
  `id, name, addrs (CIDR), httpPort, streamPort, gossipPort, capabilities,
  following (nodeId or empty), observed (peerId → {ip, lastSeenUnix}),
  version, updatedAt`
- **Group names** — map `groupId → {name, version, updatedAt}`; written by
  whichever node renames the group (LWW).
- **Playback status** — per group, written only by that group's master:
  `groupId → {state: idle|playing, file, startedAt, positionSec, codec,
  transport, version, updatedAt}`.

Liveness (`lastSeen`) comes from memberlist itself; alive/dead is not part of
the replicated doc. Node records, group names, and playback entries not
updated for **30 days** are purged (checked hourly).

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

**Group ID** = XOR of all member node IDs (16-byte XOR, hex-encoded). Order
doesn't matter (XOR is commutative); a solo node's group ID equals its node
ID. Because the ID is derived purely from the member set, a *specific
combination of nodes* keeps its name (§4 group names) whenever it reforms.

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
3. The master instructs every member (including itself) over HTTP:
   `POST /api/follow {"target":"<aliceId>"}` — and alice: `POST /api/unfollow`.
4. Group membership is unchanged, so the group ID and name are unchanged;
   only the master moved.

Members that miss the command end up following alice late or self-heal to
solo; no further coordination. The UI exposes this as a **“make master”**
button; *playing local media from a follower in the UI does takeover + play
in one action*.

## 6. Media

Each node has a local media directory (`MEDIA_DIR`). `GET /api/media` lists
playable files (recursive scan, extensions `.wav .mp3 .flac`, rescanned on
request): `[{path, name, sizeBytes, modTime}]`. Paths are relative to
`MEDIA_DIR`; path traversal outside it is rejected.

Any node's media can be browsed cluster-wide through the proxy (§9.3).

## 7. Clock sync

Master-anchored, NTP-style, over **STREAM_PORT UDP** (multiplexed with audio
by packet type, §8.4).

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

### 8.2 Source (master side)

On `play`, the master opens the file (wav: hand-rolled/`go-audio/wav`; mp3:
`hajimehoshi/go-mp3`; flac: `mewkiz/flac`), decodes → canonical PCM →
20 ms frames. Each frame gets a presentation timestamp
`pts = sessionStart + frameIndex·20ms` in **master clock nanoseconds**, where
`sessionStart = now + leadMs`. Frames are released in real time (ticker),
slightly ahead of the clock. The master sends every frame to **all group
members, including itself** (its own sink consumes via the same network path
on localhost — one code path, no special cases).

### 8.3 Codec — group setting `codec: pcm | opus` (default `pcm`)

- `pcm`: raw frame payload (3840 B).
- `opus`: 20 ms Opus at 128 kbps. Requires cgo + libopus (`hraban/opus`),
  behind build tag `opus`. A node without opus support rejects
  `play` with codec `opus` (clear API error) and reports it in capabilities.
  **The default build has no opus**; the setting exists, the wire format
  reserves it, and a `pcm` cluster works everywhere.

### 8.4 Transport — group setting `transport: udp | tcp` (default `udp`)

Common frame header (before payload):
`magic(1) | type(1) | gen(4) | seq(8) | pts(8) | payloadLen(2)`.
`gen` is the session generation (bumped on every new play/master change);
receivers drop frames from stale generations. Packet `type` on the UDP socket
multiplexes: `0x01` audio frame, `0x02` FEC parity, `0x10` clock request,
`0x11` clock reply.

- **udp**: one frame per datagram + **XOR FEC**: after every 4 audio frames,
  one parity datagram (XOR of the 4 payloads, padded) — any single loss per
  block of 5 is recovered. Receiver keeps a small reorder/recovery window.
- **tcp**: same header, length-prefixed on a persistent connection from
  master to each member's STREAM_PORT TCP listener. No FEC (TCP retransmits).

### 8.5 Sink & playout (every member)

Every member (incl. master) runs: receiver → jitter buffer → playout. The
playout loop translates each frame's `pts` to local time via the clock
follower offset and writes it to the output backend so that audio for `pts`
hits the device at `pts + bufferMs` (group setting, default **150 ms**).
Missing frames after FEC/reorder → play silence (gap), never block. Frames
arriving too late are dropped (counted).

**Output backends**: exec pipe to the first of `pw-play`, `pw-cat -p`,
`aplay`, `paplay` found (raw s16le 48k stereo via stdin) — zero cgo; plus a
`null` backend (timed discard) used in tests and on playback-less nodes.

### 8.6 Stop / end

`stop` (or natural end of file) bumps the generation, sends a `stop` control
through the same channel, clears playback status. Followers also stop when
frames cease for > 2 s (watchdog).

## 9. HTTP API (Echo)

All under `/api`. JSON. No auth (trusted LAN, v1).

### 9.1 REST

| Method/path | Action |
|---|---|
| `GET  /api/status` | this node: id, name, ports, role, group, sync/playout stats |
| `PATCH /api/node` | `{name}` → rename this node |
| `GET  /api/cluster` | replicated state, resolved: nodes (alive/dead, addrs, observed), derived groups (id, name, master, members, playback) |
| `GET  /api/media` | list this node's local media |
| `POST /api/follow` | `{target}` → this node follows target (§5.1) |
| `POST /api/unfollow` | this node becomes solo master |
| `POST /api/group/name` | `{group, name}` → name a group (any node may write) |
| `POST /api/group/master` | `{node}` → takeover (§5.2) |
| `POST /api/play` | `{file, node?}` → master only: play `file` from local media to the group. Non-masters reject with a hint to use takeover. |
| `POST /api/stop` | master only: stop group playback |
| `GET  /api/group/settings` / `POST` | `{codec, transport, bufferMs}` for this node's group (master only for POST) |

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
- **Groups** — cards per derived group: name (click to rename), members with
  master badge, playback status + position, stop button, per-member
  **“make master”** button, drag-free “join” via a member's *Join group…*
  dropdown (issues follow), *Leave* button (unfollow).
- **Nodes** — all known nodes: name (click to rename — works for remote nodes
  via proxy), id, addresses, capabilities, alive/last-seen, stale indicator.
- **Media** — node picker (any node, via proxy) → file list → **Play here**:
  plays that file on the selected node's group (does takeover first if the
  target node isn't its group's master).

## 11. Out of scope (v1)

Volume control, pause/seek, auth/TLS, internet-facing operation, playlists,
album art/metadata, multiple simultaneous streams per group, Opus in the
default build.

## 12. Repository layout

```
cmd/ensemble/        main: flag parsing, wiring, lifecycle
internal/id/         node/group IDs (gen, parse, XOR)
internal/config/     flags/env, data dir, node.json persistence
internal/netx/       bind-or-increment listeners, CIDR interface scan
internal/discovery/  mDNS register + browse
internal/cluster/    memberlist wrapper, replicated state (LWW), observed IPs
internal/group/      group derivation, follow/takeover, playback orchestration
internal/clock/      clock server + follower (UDP, via stream mux)
internal/stream/     frame codec, TCP/UDP+FEC sender & receiver, UDP mux
internal/audio/      decoders (wav/mp3/flac), resampler, frame source
internal/sink/       jitter buffer, playout scheduler, output backends
internal/api/        Echo server: REST, WebSocket, proxy, SPA embed
web/                 Svelte SPA (vite project; `npm run build` → web/dist)
docs/                this spec, architecture notes (docs/arch/*.md)
```
