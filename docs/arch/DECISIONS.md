# Architecture decisions

The living record of load-bearing design decisions for ensemble. Each is numbered
`Dn`; **those numbers are stable anchors cited from code comments** (e.g.
`// D64 telemetry`) — they are never reused or renumbered. Decisions are grouped by
topic below; superseded ones live only in the [index](#index--d1d65) with a pointer
to what replaced them. Where a piece/spec doc disagrees with this file, this file
wins.

---

## 1. Identity, persistence & config

**D1 — `node.json` is the only persisted per-node state.** It holds
`{id, name, volume, outputDelayMs, outputDevice, disabled, following}`. Everything
else on a node record (ports, addrs, caps, observations) is runtime/replicated,
in-memory, rebuilt on start. Fields decode presence-aware (an absent field takes its
default; a malformed one warns and defaults — never fatal). `following` is the boot
seed + last-known follow target (`""` = solo); its live value rides the replicated
record. Atomic temp+fsync+rename writes. (Fields added over time: volume D35,
outputDelayMs D36, outputDevice D37, disabled D40, following D45.)

**D2 — `ENSEMBLE_OUTPUT` is env-only**, no flag: `auto` (default) | `null` |
`file:<path>` | an explicit backend name. `auto` picks alsa → exec → null (D27).

**D41 / D47 — `cluster.json` persists only the long-lived lookup tables.** Two
things, each a FULL LWW record (version + writer, so the gossip merge applies):
the group override-**names** map (XOR-keyed — an override names a specific
COMBINATION of rooms, so it survives master changes) and this node's **own**
group-settings record (self-keyed, since group id == master id, D44). NOT node
records, NOT playback, NOT peers' settings (all live replicated state owned
elsewhere). Loaded at `cluster.New` before the memberlist join; once gossip starts
the normal LWW rule reconciles loaded-vs-gossiped. Saved debounced (~2 s, coalescing
change storms) plus a final save on `Close`; atomic like `node.json`. Missing file =
clean empty start; corrupt = warn + empty (never fatal). Empty `StatePath` (tests)
disables persistence. A restart-version guard (`reconcileOwnSettingsVersion`, the D7
rule for settings) jumps the local counter above any peer copy with version ≥ ours so
the next local write wins. Group names + settings are exempt from the 30-day purge
(kept forever); node records + playback age out at 30 days. Effect: a master that
restarts against the same data dir re-forms its solo group with its last
codec/transport/bufferMs instead of cluster defaults.

---

## 2. Cluster, discovery & grouping

**D4 — discovery `Peer` = `{ID; Addr; GossipPort, HTTPPort, StreamPort}`** with a
`GossipAddrPort()` helper; the discovery channel is `<-chan Peer`, closed on
shutdown. zeroconf's SRV port is informational — **TXT records are authoritative**
for all ports.

**D5 — group derivation is pure and centralized** in `cluster.DeriveGroups`
(exported). `Snapshot.Groups` arrives pre-derived, joined with
names/playback/settings; consumers do not re-derive. (Group keying redefined by D44:
group id = master id, not the member-set XOR.)

**D6 — `DialCandidates` falls back to self-reported CIDRs** when the
observed-intersection is empty (cold peers must be dialable), tightening to
observed-only once any observation exists. The initial memberlist join uses the
discovery `Peer.Addr` directly.

**D7 — own-record version reconciliation on restart.** After the first push/pull, if
a peer holds our own record with version ≥ ours, jump our counter above it. The same
rule applies to the self-keyed settings record (D47) and to discovered playback-node
records across masters (D62).

**D8 — gossip port handoff.** K probes a free TCP+UDP pair for the gossip port,
closes both, and passes the bare number to memberlist (which binds it). The tiny
rebind race is accepted. STREAM stays bound-and-handed-over (the mux keeps its UDP
socket).

**D14 — cluster write-side is concrete methods on `cluster.Cluster`** (not in
`contracts`; consumers declare small Go-style consumer interfaces): `SetName`,
`SetVolume`, `SetOutputDelayMs`, `SetOutputDevice`, `SetDisabled`, `SetFollowing`
(Zero = solo), `SetPlayback(group, p)`, `SetGroupSettings(group, s)`,
`SetGroupName(group, name)`, `Observe(peer, ip)`, `DialCandidates(peer)` (best-first),
`Join(addrs)`.

**D20 — `--join` / `ENSEMBLE_JOIN`** (comma-separated `host:gossipPort` seeds) is a
dev flag for hermetic loopback e2e tests; mDNS remains the production path.

**D44 — the group id IS the master's node id; hybrid naming; settings carry over on
takeover.** Keying the group (and its master-written playback + settings records) by
the master id means membership churn (a member joining/leaving, master unchanged)
never changes the id, so those records are never orphaned; the id changes only on a
master move (which stops the session first). Solo group id = the node's own id.
`GroupView.ID = master`. **Hybrid naming:** the explicit override-names map stays
member-set-XOR-keyed (survives master changes); with no override, `DeriveGroups`
computes a DERIVED label from member names (sorted, `" + "`-joined, first 3 then
`" +N more"`; solo = the node's name; missing member → 8-char short id).
`GroupView.NameIsDerived` (json `nameDerived`) reports which; `POST /api/group/name`
writes the override at the current member set's XOR, an empty name CLEARS it. On
takeover, `group.MakeMaster` copies the settings record to the new master's key
(playback does not carry — takeover stops the session). This superseded the
XOR-of-members group id (D5/§5) and removed the D43 churn re-point.

**D45 — a node persists `following` and rejoins its previous group on return;
self-heal clears a dangling follow; grace is the decision window.** `node.json` holds
`following` (D1); `cluster.New` seeds this node's own record's `Following` from it
(gossiped from version 1, as if `SetFollowing` had been called), and the EXISTING
machinery does the rest — if the old master is alive and a master, `DeriveGroups`
re-forms the group; if it is dead/unknown, the §5 self-heal grace fires and resets
the node to solo. No new rejoin logic. The engine persists on every follow change
(`Follow`/`Unfollow`/takeover-directed/self-heal reset) via one `setFollowing`
helper. The self-heal timer arms when the engine first OBSERVES the dangling follow
(first stale reconcile), not at process start, so slow gossip convergence cannot
insta-clear a follow still propagating. (For non-gossiping playback nodes the
mechanism differs — see D63a.)

---

## 3. Clock & sync

**D10 — `contracts.Clock` carries both directions:** `MasterToLocal` and
`LocalToMaster(localNanos) (int64, bool)` — the source stamps PTS in master time and
needs the forward conversion.

**D11 — clock generation rides `Header.Gen`.** The 24-byte `t1|t2|t3` payload stands;
the follower trusts its locally-recorded t1 keyed by `Header.Seq` (echoed payload t1
is advisory).

**D53 — cold-start convergence uses a startup burst, not 1 Hz.** To meet the <500 ms
play-to-sound budget while withholding playout until synced, a joining node bursts
clock probes (~10–20 over the first ~200 ms) then settles to 1 Hz steady state. The
best-RTT-of-window offset filter is unchanged — only the probe schedule changes.

---

## 4. Audio source, streaming & sink

**D9 — audio EOF semantics.** `ReadFrame(dst []byte) error` fills exactly
`stream.FrameBytes` into caller-owned `dst`; the final partial frame is zero-padded
and returned with `nil`; the *next* call returns `io.EOF`.

**D12 — no `Mux.Unregister` in v1.** Handlers are one-per-node and long-lived;
receiver/follower keep a `closed` guard so late dispatch is a no-op.

**D13 — TCP stream framing is a `uint32` big-endian length prefix** before each
`header+payload` chunk. FEC parity IS flushed for a partial tail block on stop/EOF.

**D22 — subscribe model on SOURCE_PORT (default 9200, TCP+UDP, bind-or-increment).**
The source listens; members subscribe via stream control (§8.7:
HELLO/BYE/RESTART/RECONFIG, packet types 0x20–0x23, 1-byte flag payload). UDP
subscribers HELLO **from their STREAM_PORT mux socket**, so audio flows back to the
observed source addr:port and the member-side receive path (mux types 0x01/0x02) is
unchanged; TCP subscribers dial SOURCE_PORT and share control + length-prefixed audio
on the connection. HELLO keepalive every 5 s; subscriber expiry 15 s. Subscribers
resolve the master via `cluster.DialCandidates(master)` (this removed the old
`Resolver`/`SetEndpoints` seam, D18). The master's own sink subscribes over loopback
like any client. Every node binds SOURCE_PORT (any node can become master) though it
only matters on masters.

**D23 — live settings changes.** The master bumps gen, broadcasts RECONFIG, refreshes
the replicated group-settings record; subscribers re-read settings and resubscribe
under the new gen. RECONFIG with the stop flag is the explicit end-of-session notice.
(Replaced the "bufferMs fixed per session" assumption, D21.)

**D24 — source ring & burst prime.** A ring of released frames sized
`max(2 × bufferMs, 1 s)`. Prime = replay ring frames whose `pts + bufferMs` deadline
is still future (older frames are skipped — useless to the newcomer). UDP burst
pacing ~4× realtime (one frame per ~5 ms); TCP back-to-back. A priming subscriber is
**excluded from live fan-out** until its burst catches up to the live edge — else an
interleaved live frame would anchor its reorder window ahead of the burst and the
whole prime would drop as late; the >realtime rate guarantees catch-up terminates.

**D25 — the rate servo prevents DAC drift continuously.** The drift signal is the
**output device queue depth** (ALSA `snd_pcm_delay` via the backend `DelayReporter`),
NOT the playout scheduler — the scheduler is master-clock-locked, so the DAC's true
crystal rate only shows up in queue depth. A proportional controller (gentle gain,
slew-limited, ±300 ppm clamp — see D64) drives a 4-tap Catmull-Rom fractional
resampler between the jitter buffer and the backend. Underruns stay silence +
watchdog → RESTART (starved >2 s → RESTART to the source; still starved →
unsubscribe, group self-heal takes over). `SinkStats` carries `RatePPM`, `Buffered`.

**D26 — media-source abstraction.** A scheme-keyed factory (`file` / `http` / `input`)
→ one `Source` contract (canonical-PCM `ReadFrame(dst)`, `Close`,
D9 EOF). Pull-paced (`file`: decode-ahead, EOF ends session) vs live-paced
(`http`/`input`: never EOF, underflow → the release ticker emits silence, cadence
never stalls). `input` is exec-capture (`pw-record`/`arecord`). Available schemes are
reported in `capabilities.sources`. Live-source underflow is the source's problem
(silence internally, `ReadFrame` returns `nil`) — there is no `ErrUnderflow`
sentinel.

**D27 — sink-backend registry.** Named backends in the single build: `alsa`
(runtime-loaded libasound, implements `DelayReporter`, v1), `exec` (fallback pipe),
`null`, `file`. `alsa` registers itself only when the dlopen probe succeeds (D32).
`ENSEMBLE_OUTPUT` selects by name; `auto` picks alsa → exec → null. Available names
are reported in `capabilities.backends`; `playback` = a real (non-null) backend is
usable.

**D28 — source stats surfacing.** `SourceStats{Clients, Connects, Restarts, Primes}`
in `/api/status` (D19) and riding the master-written replicated playback record
(`Playback.Source`), refreshed with the periodic position update so the UI reads it
from the cluster snapshot.

**D29 — seam names follow the concrete exports.** The source server is
`source.NewServer(source.Config)` with `StartSession / ReleaseFrame / Reconfig /
StopSession / Stats`; the subscriber is `stream.NewClient` with `Subscribe(sourceAddr,
gen, transport) / Unsubscribe / Counters`.

**D31 — no `api.SetGroup`; no construction cycle.** The group engine depends on the
API only via `contracts.FollowClient` (a leaf), so K builds standalone
`api.NewFollowClient(cluster)` → `group.New(...)` → `api.New(...)` last.

**D34 — alsa backend (v1).** Simple-API binding via `internal/dl`: `snd_pcm_open(…,
"default", PLAYBACK)`, `snd_pcm_set_params(S16_LE, RW_INTERLEAVED, 2, 48000, 1,
latencyUs)`, `snd_pcm_writei` per frame with `snd_pcm_recover` on `-EPIPE`/`-ESTRPIPE`,
`snd_pcm_delay` implementing `DelayReporter`, `snd_pcm_close`. Registers only when the
probe succeeds; first in `auto` order.

---

## 5. Codecs & runtime library loading

There is exactly **one build**, no cgo, no build tags. Optional native support is
probed at runtime and degrades gracefully.

**D3 — capabilities are assembled by K (main) at startup:** PATH probes for exec
tools, runtime dlopen probes (`libopus.so.0` → `opus`; `libasound.so.2` → `alsa`),
and static format/scheme lists — handed to cluster via its config/setter. A node with
`ENSEMBLE_OUTPUT=null` reports `playback:false` but still "plays" to the null sink;
capability never gates group membership or fan-out.

**D32 — runtime loading via purego (`internal/dl`).** Optional shared libraries load
with `github.com/ebitengine/purego` (dlopen/dlsym FFI from pure Go, CGO_ENABLED=0).
`dl.Open(sonames, symbols)` tries sonames in order and **dlsym-verifies every required
symbol before any `RegisterLibFunc`** — a missing library/version/symbol yields
`dl.ErrUnavailable` (soft, never a panic) and the capability is reported off. Call
rate ~50/s, so FFI overhead is irrelevant.

**D33 — opus: placement, negotiation, late-join.** The codec lives in
`internal/audio` (`NewOpusEncoder/NewOpusDecoder`, returning `dl.ErrUnavailable` when
libopus isn't loadable; ~7 bound functions, bitrate 128k). **Master encodes once**
(wired between source `ReadFrame` and fan-out); **each member decodes** (wired between
the subscriber deliver callback and `Sink.Push` — the sink always consumes canonical
PCM). No decoder PLC — a lost opus frame is silence, same as pcm.
- **Negotiation:** the master never rejects `play` for a missing codec — it
  negotiates the EFFECTIVE codec at every session start AND on mid-session membership
  change. Effective = wanted `settings.codec` iff every current member's effective
  caps include it and this master can encode it, else `pcm` (universal). Downgrades
  log and are reflected in the replicated playback record. Mid-session, a running opus
  session that becomes unsupported (a member disabled opus, or a non-opus node joined)
  **downgrades in place** like a live settings change (bump gen, swap session encoder
  to nil, `StartSession`, `Reconfig`, re-point, resume from position). Only downgrades
  auto-apply mid-session; an upgrade waits for the next play/settings change. A genuine
  encoder-build failure still fails the play with `ErrNoOpus`.
- **Late-join stale-gen:** the member-side deliver consults the sink's ACTUAL armed
  gen (`*sink.Playout.ArmedGen()`) and re-arms whenever the sink is not armed at the
  incoming frame's gen — fixing a joiner that starved when the master's real gen
  happened to equal the deliver closure's cached gen.

**D42 — opus is the default codec, with transparent pcm downgrade.**
`contracts.DefaultCodec = "opus"`. Rationale: a raw-PCM datagram is `24 + 3840 B` and
IP-fragments into ~3 packets; on lossy Wi-Fi a lost fragment drops the whole frame and
per-frame XOR FEC can't recover it (observed: Pi members on WLAN got clock packets but
no audio). A 20 ms opus packet is ~320 B — one MTU. `group.Play` downgrades to pcm
when the group can't do opus (per D33) rather than rejecting; an explicit
`codec: opus` is still validated against the master's own capability.

---

## 6. Per-node controls & features

**D35 — per-node volume (live software gain).** `volume` float `0.0–1.0` (default
`1.0`), in `node.json` + the replicated record; `PATCH /api/node {volume}`. Applied as
the last sink stage before the backend: per-sample int16 multiply, target read
atomically each frame, linear ramp over one 20 ms frame — no restart, every backend.
`0.0` is a real (muted) value: absent-field defaulting to `1.0` happens ONLY in A's
presence-aware decode; every layer downstream treats the resolved value as
authoritative.

**D36 — per-node output-delay calibration.** `outputDelayMs` int (default 0, clamped
±500) compensates fixed downstream latency invisible to the servo (pipe internals,
DAC/amp/BT chains). Deadline contribution: `… − outputDelayMs`. Sign: **positive =
device chain is late → write earlier**. `Sink.SetDelayOffset` takes ns; a live change
drops the buffer and fires the restart hook (RESTART → burst re-prime) — a sub-second
blip on that node only. Covers ONLY the acoustic/room offset; the device-buffer
component is handled separately (D65).

**D37 — output-device selection.** A node may pick which ALSA device the alsa backend
opens. Enumeration is from `/proc/asound/pcm` (zero deps, pure/testable
`parseProcPCM`): playback-capable `CC-DD` lines → `{ID:"hw:C,D"}`, prepended with
`default`; empty when libasound isn't loadable or the file is missing. Enumerated once
at startup, reported on the node record + `NodeView`. `node.json` gains `outputDevice`
(default `"default"`); `PATCH /api/node {outputDevice}` validates against the node's
own list, then persist → replicate → apply: only when the active backend is alsa, K
reopens the backend and calls `Playout.SwapBackend(b)` (close old, set new, re-assert
`DelayReporter`). A brief blip is accepted; the session is NOT restarted. The exec
backend ignores the device in v1.

**D38 — bring-up aids.** `POST /api/tone` plays a 1 s 440 Hz tone through the local
backend (`Sink.TestTone`; 409 while a session/tone is active; respects live volume),
surfaced as a per-node button. And: the initial UDP HELLO may be lost, so until the
first frame arrives the subscriber re-HELLOs (prime-me) 3× at 500 ms before falling
back to the 5 s keepalive.

**D39 — per-group play/pause.** A `paused` state joins `idle`/`playing`, written only
by the master. **Pause** (`POST /api/pause`, 409 `not_playing`): the master stops
releasing frames but KEEPS the source open and the session/gen alive with the position
frozen; writing `state="paused"` cleanly unsubscribes every member through the
existing `state=="playing"` gating. **Resume** (`POST /api/resume`, 409 `not_paused`):
bump gen, re-anchor `sessionStart`, frame index → 0, source continues from where it
stopped — audio is contiguous though pts restart with the new gen. Live sources resume
at the live edge. The UI bar is play/pause aware.

**D40 — operator-disabled features (effective capabilities).** A node may locally turn
off `playback`, `opus`, or `input`. `node.json` gains `disabled:[…]` (normalized
subset); the replicated record carries both the operator list AND the probed caps.
**Effective caps = probed − disabled**, computed in one place (`cluster.effectiveCaps`
in `nodeView`): disabling `playback` → `Playback:false`; `opus` → out of `Codecs`;
`input` → out of `Sources`. Probed caps are never mutated, so re-enabling restores
them; `NodeView` exposes the raw `Disabled` for the UI. `PATCH /api/node {disabled}`
validates a subset of `{playback,opus,input}`, then persist → replicate → apply (the
live apply swaps the sink to **null** when `playback` is newly disabled, reopens the
device when re-enabled; `opus`/`input` need no swap). Local constructors also refuse
(belt-and-suspenders), but the primary gate is the effective-caps subtraction. The UI
renders tri-state chips (ON solid green ●, OFF amber outline ○, UNAVAILABLE dimmed
struck ✕, not clickable).

---

## 7. Device latency, drift & cross-room sync

The playout deadline is `target = MasterToLocal(pts + bufferMs − outputDelayMs +
equalize)`. The three terms below pin down how heterogeneous DACs are kept in phase.
`DefaultBufferMs` is 300 ms (jitter window ≈ `bufferMs − deviceLatency`; play-to-sound
stays under 500 ms).

**D63 — device-latency telemetry + `LatencyReporter` (constant-subtraction
reverted).** D63 originally subtracted the backend's CONFIGURED output latency
(`ENSEMBLE_ALSA_LATENCY_MS`, default 200 ms; 0 for pipe/null via the optional
`contracts.LatencyReporter`) from the deadline, to pre-roll the device to its full
buffer and put heterogeneous DACs in phase. **That subtraction was reverted by D64**
(it jolted the schedule when a calibrated constant switched mid-session). What
survives and is current: the `LatencyReporter` interface, and the device-latency
**telemetry** — `SinkStats.DeviceDelayNs` (the live `snd_pcm_delay`) surfaced over the
wire (still tagged `D63 telemetry` in code). The orphaned `deviceLatencyNs` sink field
is dead and may be removed.

**D64 — gentle anti-drift servo.** The audible "stumbling" (nodes drift apart then
snap back over seconds) was the rate servo HUNTING: at `Kq=1.5 ppm/sample` the Pi's
±10 ms `snd_pcm_delay` jitter mapped to >300 ppm, railing the loop at ±ClampPPM and
swinging the playout phase. Fix: retune gentle (`Kq=0.08`, `SlewPPM=20`) so ±10 ms maps
to tens of ppm — small, smooth, never railed. The servo's job is unchanged (hold the
device queue near its calibrated setpoint); only the gain changed. Subtracting the
LIVE device delay from the deadline was rejected as a positive-feedback loop (write
earlier → queue grows → write earlier…; reproduced as a test failure). To drive the
proper fix, STATUS gained `DeviceDelayNs` + `PhaseErrNs`, the master logs a 1 s
`room sync skewMs` line, and every piece emits a 1 s `msg=stats` line.

**D65 — master-driven cross-room device-buffer equalization.** Rooms with different
output latencies (measured: pipewire-pulse ~250 ms vs a Pi `hw` device ~180 ms) play
~70 ms apart even though each is clock-locked — the audible inter-room desync the
`room sync skewMs` line reports. The **master** equalizes it: the stable per-room
constant is the servo's calibrated setpoint, recovered from STATUS as
`DeviceDelayNs − PhaseErrNs` (a `StatusFlagCalibrated` bit marks it frozen — the live
`DeviceDelayNs` swings ±10 ms and must not be used). The driver finds the slowest
room's setpoint and tells every faster room to DELAY by `max − own` via a new control
type `SETEQ` (0x35, unsigned ms). The sink sums it into the deadline as a SEPARATE
component from the D36 acoustic offset (`… + equalize`), so the master never clobbers
the node-owned acoustic calibration. It only ever ADDS delay, so it never pre-rolls
harder than the device buffer allows (the feedback failure mode D63/D64 avoided).
Soft-state: re-asserted every tick, the listener dedups the re-anchor, so a steady
target re-anchors once and a lost datagram self-heals. Withheld until EVERY room has a
settled setpoint, so the reference `max` is final. The `room sync` line gains `cal=`;
the driver logs `room equalized` on change. **Limitation:** v1 equalizes the remote
rooms a master drives; a master that is also a local player is not yet folded into the
`max` (follow-up).

---

## 8. Acoustic calibration

**D48 — acoustic auto-calibration — REMOVED.** A mic-driven auto-measurement of every
member's output delay (matched-filter log-sweep, relative §5 solve, `calibrate:`
source scheme, `POST/GET /api/calibrate`, the `internal/calibrate` package and a
group-card wizard) was implemented and then removed: it was a heavyweight,
hardware-coupled wizard that earned its place in every room card poorly, and the
cross-room sync it chased is now handled by the master-driven equalization of D65.
Per-node latency is still tunable by hand via `outputDelayMs` (D36). The number is
retained as an anchor; the feature, its API, and `docs/calibrate.md` are gone.

---

## 9. Playback role split (master / playback nodes)

Informed by a study of competing multiroom protocols (archived in `../external/`:
Snapcast, SlimProto/Squeezelite, AirPlay 2 / NQPTP, Roc/FECFRAME, Google Cast). The
cross-cutting finding: every synced-audio system converges on *shared clock + buffer
lead + per-receiver rate correction*, which ensemble already does. **Pre-release
latitude:** these decisions may renumber wire `type`s and reassign ports freely —
there is no external client to keep compatible.

**D46 — wire protocol v1: magic `0xE5` is the version marker.** Receivers ignore
unknown packet types (new optional types are additive); an incompatible revision
changes the magic. This lets a protocol-minimal receive-only client
(`docs/DUMB-CLIENT.md`, `cmd/dumbclient`) interoperate without cluster membership.

**D49 — two independently-enableable roles, `master` and `playback`** (default both).
- **`master`** participates in memberlist gossip, owns/replicates cluster state,
  serves the REST/WS API + SPA, and sources/streams audio. It is the only external
  control surface (D56).
- **`playback`** is the receive-and-play role (wire behavior =
  [DUMB-CLIENT.md](../DUMB-CLIENT.md)): subscribe, clock-follow, play in sync. It
  announces via mDNS, **never gossips, holds no cluster state**, and is **driven by a
  master**, idle until told to join. The protocol is identical for Go and MCU; only
  capabilities differ (D51). A combined node gossips *because* it is a master.

**D50 — masters discover playback nodes via mDNS and represent them in gossiped state
as non-gossiping members.** The TXT carries node id, `role=playback`, control/clock/
audio ports, and the minimal cap set (D51). Each is assigned to a group via the
existing model; assignment is a single cluster-owned record per node, so multiple
masters arbitrate through the existing gossip CRDT rather than racing for a speaker.

**D51 — capabilities are announced, not assumed.** A playback node advertises a small
flat cap set (mDNS TXT, echoed in the control handshake): decodable `codecs`,
`maxRate`, `hwVolume`, `fixedLatencyMs`, `canReportQueue` (drives D52), `input`. This
generalizes D3 to the non-gossiping role. Caps still never gate membership or fan-out.

**D52 — the rate servo + resampler is canonical drift correction, for Go AND MCU.** An
MCU with `canReportQueue` (observable I2S/DMA fill; an S3-class FPU runs Catmull-Rom at
48 k stereo) SHOULD run the identical loop (D25). Without it, DAC drift is unobservable
and accepted — the skip-late/silence-gap path is the floor, not the target. This is
what satisfies the inaudible-drift non-negotiable.

**D54 — the control plane is a small set of wire packets, master↔playback.** Verbs:
`ATTACH <stream/gen>`, `DETACH`, `SETVOL <pct,mute>`, `SETDELAY <ms>` (D36 acoustic
offset), `SETEQ <ms>` (D65 equalization), `SETCAP <bit,on>`, and `STATUS`
(playback→master, D55). `SETVOL`/`SETDELAY` drive the existing per-node fields over the
wire, so an MCU needs no HTTP server.

**D55 — playback nodes emit STATUS telemetry to their master** (~1 Hz / on change):
the existing `SinkStats` (jitter-buffer fullness, last `seq`, clock `offset`/`rtt`,
`ratePPM`, underrun/skip counters) plus the device-latency fields (D63/D64). The master
never depends on it for correctness — observability + adaptation (e.g. D65), not
control. It feeds the SPA's per-room health view.

**D56 — the master is the only external control surface.** All REST/WS/SPA — and any
future Home Assistant / MQTT / Snapcast-compatible façade — live on masters only,
keeping MCU firmware tiny. *Direction (deferred):* expose the current REST/WS as the
HA surface first; later evaluate a Snapcast-compatible JSON-RPC façade or MQTT bridge.

**D57 — direction (not committed): Spotify & rich media via a process/pipe source.**
Add a process source scheme so a master can run `go-librespot` (Spotify Connect,
zeroconf-credentialed by the user's phone) or consume a Mopidy output, per the Snapcast
pattern — reuse rather than reimplementing auth/search/indexing. No wire/port impact.

**D58 — the control plane is UDP soft-state, not request/response.** The master
re-asserts desired state to each node's CONTROL_PORT (ATTACH + SETVOL/SETDELAY/SETEQ/
SETCAP) on a ~1 Hz heartbeat and on change; the node applies each command
idempotently and acks nothing. A lost datagram self-heals on the next heartbeat
(Snapcast's "server pushes state, client persists nothing"). TCP control was rejected:
more state, no gain (the data plane already offers TCP where reliability matters).

**D59 — playback assignment reuses `Following`, written by a master.** A playback node
is a `NodeRecord` with `Role=playback`, `Gossips=false`, a `ControlPort`, and its
announced `Caps`; its **assignment is its `Following` set to the master's id**, but
since it doesn't gossip the field is written by a master/operator (`PATCH /api/node
{following}` on a master), not the node. `DeriveGroups` already attaches followers to
their master, so an assigned node appears as a group member with minimal new logic.

**D60 — liveness for non-gossiping playback nodes = mDNS freshness OR recent STATUS.**
memberlist liveness doesn't apply. A node is alive if its mDNS advert is fresh OR it
sent a STATUS recently; it expires when both go stale. `DeriveGroups`/`nodeView`
`isAlive` consult this for `Role=playback` records.

**D61 — a combined master+playback node drives its own playback in-process.** The
`Player` verb interface (`Attach`/`Detach`/`SetVolume`/`SetDelay`/`SetEqualize`/
`SetCap`/`Status`) is the single seam for ALL playout. The group engine drives the
**local** `Player` with direct in-process calls; the wire control plane (D58) is used
**only** for remote nodes. Both front-ends hit the identical interface, so "behaves the
same for Go and MCU" holds without a node ever wire-driving itself over loopback. This
collapses the former two playout paths (full member; standalone `cmd/dumbclient`) into
one component.

**D62 — multi-master convergence reuses the D7 reconcile.** The master that first
discovers a playback peer injects its `NodeRecord`; the record (and its `Following`
assignment) replicates among masters by LWW + own-version reconciliation. No new
arbitration — two masters seeing the same speaker converge on one record; the master
named by `Following` runs the control driver for that node.

**D63a — `following` persists across a master's absence (non-gossiping nodes).** A
player whose target master goes offline is simply not grouped (idle) by `DeriveGroups`
while the master is absent; its `following` is NOT reset, and it rejoins automatically
when the master returns or it is reassigned. There is no self-heal/auto-clear for these
nodes (unlike the gossiping-member grace of D45 — the old `heal.go` was removed with
the crosswise model).

---

## 10. API & web

**D15 — go:embed lives in `web/embed.go`** (`package web`, `//go:embed all:dist`,
exports `DistFS`) because `go:embed` can't reference parent dirs from `internal/api`;
the API takes the FS via config.

**D16 — `FollowClient` is a plain cluster-backed HTTP client in `internal/api`** (no
dependency on the Echo server), so the build order is cluster → followClient → group
engine → api server with no cycle.

**D17 — takeover forwarding is the API's job** (a proxy hop to the current master);
`group.MakeMaster` assumes it runs on the master and errors `ErrNotMaster` otherwise.
The group engine owns re-pointing the clock follower (`SetMaster(addr, gen)`) when the
elected master endpoint or generation changes. (The per-member stream-endpoint half is
gone — D22's subscribe model removed it.)

**D19 — `/api/status` JSON envelope:**

```json
{
  "id": "<32hex>", "name": "...", "role": "master|follower|solo",
  "groupId": "<32hex>",
  "ports": {"http": 8080, "stream": 9090, "source": 9200, "gossip": 7946},
  "sink":  {"played": 0, "silence": 0, "lateDrop": 0, "staleGen": 0,
            "synced": false, "ratePPM": 0, "buffered": 0},
  "clock": {"synced": false, "offsetNs": 0, "rttNs": 0},
  "source": {"clients": 0, "connects": 0, "restarts": 0, "primes": 0}
}
```

`role:"solo"` = master of a group of 1; `source` present only while this node runs an
active audio source. `/api/status` carries only `groupId`/`role`; the full group
object comes from `/api/cluster`.

---

## Confirmed as designed (no change)

- The cluster two-mutex exception (doc + liveness) with a never-hold-both rule.
- The decoder imports only the PCM constants from `package stream`.
- Sink `Push` is fire-and-forget; no backpressure/close signal upstream.
- Transport `Counters` and `SinkStats` stay separate; `/api/status` surfaces sink
  stats; transport counters may be added later.
- Loopback e2e: nodes on 127.0.0.1 have empty `InterfaceCIDRs`; reachability comes
  from `--join` seeds + observed-IP reporting (memberlist + HTTP traffic both feed
  `Observe`).

---

## Index — D1–D65

Every decision number, with its home section or what superseded it.

| # | Where |
|---|-------|
| D1 | §1 Identity |
| D2 | §1 Identity |
| D3 | §5 Codecs & loading |
| D4 | §2 Cluster & grouping |
| D5 | §2 Cluster & grouping (group-id keying superseded by **D44**) |
| D6 | §2 Cluster & grouping |
| D7 | §2 Cluster & grouping |
| D8 | §2 Cluster & grouping |
| D9 | §4 Audio pipeline |
| D10 | §3 Clock & sync |
| D11 | §3 Clock & sync |
| D12 | §4 Audio pipeline |
| D13 | §4 Audio pipeline |
| D14 | §2 Cluster & grouping |
| D15 | §10 API & web |
| D16 | §10 API & web |
| D17 | §10 API & web (stream-endpoint half superseded by **D22**) |
| D18 | superseded by **D22** — no `Resolver`/`SetEndpoints` seam; subscribers dial SOURCE_PORT |
| D19 | §10 API & web |
| D20 | §2 Cluster & grouping |
| D21 | superseded by **D23** — bufferMs/settings apply live, not fixed per session |
| D22 | §4 Audio pipeline |
| D23 | §4 Audio pipeline |
| D24 | §4 Audio pipeline |
| D25 | §4 Audio pipeline (servo gain retuned by **D64**) |
| D26 | §4 Audio pipeline |
| D27 | §4 Audio pipeline |
| D28 | §4 Audio pipeline |
| D29 | §4 Audio pipeline |
| D30 | §4 Audio pipeline (folded into D26) |
| D31 | §4 Audio pipeline / §10 |
| D32 | §5 Codecs & loading |
| D33 | §5 Codecs & loading (incl. negotiation + late-join amendments) |
| D34 | §4 Audio pipeline |
| D35 | §6 Per-node controls |
| D36 | §6 Per-node controls |
| D37 | §6 Per-node controls |
| D38 | §6 Per-node controls |
| D39 | §6 Per-node controls |
| D40 | §6 Per-node controls |
| D41 | §1 Identity (narrowed by **D44**, amended by **D47**) |
| D42 | §5 Codecs & loading |
| D43 | superseded by **D44** — the playback-record re-point was removed (group id = master id makes it moot) |
| D44 | §2 Cluster & grouping |
| D45 | §2 Cluster & grouping |
| D46 | §9 Playback role split |
| D47 | §1 Identity |
| D48 | §8 — REMOVED (acoustic auto-calibration; superseded for sync by D65) |
| D49 | §9 Playback role split |
| D50 | §9 Playback role split |
| D51 | §9 Playback role split |
| D52 | §9 Playback role split |
| D53 | §3 Clock & sync |
| D54 | §9 Playback role split |
| D55 | §9 Playback role split |
| D56 | §9 Playback role split / §10 |
| D57 | §9 Playback role split |
| D58 | §9 Playback role split |
| D59 | §9 Playback role split |
| D60 | §9 Playback role split |
| D61 | §9 Playback role split |
| D62 | §9 Playback role split |
| D63 | §7 Device latency (constant-subtraction reverted by **D64**; telemetry survives) |
| D63a | §9 Playback role split |
| D64 | §7 Device latency |
| D65 | §7 Device latency |
