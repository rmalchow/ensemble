# Contract reconciliation — integrator decisions

The eleven piece architects raised 43 contract concerns against
[S-skeleton.md](S-skeleton.md) and the [spec](../README.md). This file
resolves every one that needed a decision; trivially-confirmed items are
grouped at the end. **These decisions amend the arch docs** — where a piece
doc disagrees with this file, this file wins. (Surgical fixes already applied
to S-skeleton.md are marked ✎S.)

## Decisions

**D1 — node.json holds `{id, name, volume, outputDelayMs, outputDevice, disabled,
following}`** *(amended by D35/D36/D37/D40/D45)*. (A) The remaining node-record
fields (ports, addrs, caps, observations) are runtime/replicated, in-memory only,
rebuilt on start. `following` (D45) is persisted as the boot seed + last-known
follow target (`""` == solo); its live value still lives in the replicated record.

**D2 — `ENSEMBLE_OUTPUT` is env-only** (`auto` default | `null` | `file:<path>`
| explicit backend name). No flag. Added to spec §2. (A/E/K)

**D3 — capabilities are assembled by K (main) at startup** — PATH probe for
the exec tools **plus runtime dlopen probes** (D32: `libopus.so.0` →
`codecs:["pcm","opus"]`, `libasound.so.2` → `backends` includes `"alsa"`),
static format/scheme lists — and handed to cluster via its config/setter.
A stays pure, D stays decode-only. A node with `ENSEMBLE_OUTPUT=null` reports
`playback:false` but still receives and "plays" to the null sink; playback
capability never gates group membership or stream fan-out. (A/D/E/K)

**D4 — discovery `Peer` is `{ID id.ID; Addr netip.Addr; GossipPort, HTTPPort,
StreamPort int}`** with a `GossipAddrPort()` helper; B's channel is
`<-chan Peer`, closed on shutdown. zeroconf's SRV port carries HTTP_PORT but
is informational — **TXT records are authoritative** for all three ports. (B/C)

**D5 — group derivation is owned by C** (`cluster.DeriveGroups`, pure,
exported); `Snapshot.Groups` arrives pre-derived and joined with
names/playback/settings. H consumes `Snapshot.Groups` and does **not**
re-derive; H's own copy of the algorithm in H-group.md is dropped. (C/H)

**D6 — DialCandidates falls back to self-reported CIDRs** when the
observed-intersection is empty (cold peers must be dialable); it tightens to
observed-only as soon as any observation exists. Initial memberlist join uses
the discovery `Peer.Addr` directly — §3.1 resolution governs post-boot dials,
not cold bootstrap. Spec §3.1 wording adjusted. (C/K)

**D7 — own-record version reconciliation on restart**: after first push/pull,
if a peer holds our own record with version ≥ ours, jump our counter above it.
(C)

**D8 — gossip port handoff**: K uses netx to *probe* a free TCP+UDP pair for
the gossip port, closes both, and passes the bare number to memberlist (which
binds it itself). The tiny rebind race is accepted for v1. STREAM stays
bound-and-handed-over (mux keeps the UDP socket). (C/K)

**D9 — audio EOF semantics** (pinned for D and H): `ReadFrame(dst []byte)
error` fills exactly `stream.FrameBytes` into caller-owned `dst`; the final
partial frame is zero-padded and returned with `nil`; the *next* call returns
`io.EOF`. H's `Source` seam adopts this signature (H-group.md's
`ReadFrame() ([]byte, error)` is superseded). (D/H)

**D10 — `contracts.Clock` gains `LocalToMaster(localNanos int64) (int64, bool)`**
— H stamps PTS in master time and needs the forward conversion. ✎S (F/H)

**D11 — clock generation rides `Header.Gen`**; the 24-byte t1|t2|t3 payload
stands; the follower trusts its locally-recorded t1 keyed by `Header.Seq`
(echoed payload t1 is advisory). (F)

**D12 — no `Mux.Unregister` in v1.** Handlers are one-per-node and long-lived;
Receiver/Follower keep a `closed` guard so late dispatch is a no-op. (F/G)

**D13 — TCP stream framing is `uint32` big-endian length prefix** before each
`header+payload` chunk. Both ends live in G; pinned here so nobody invents a
second framing. FEC parity **is** flushed for a partial tail block on
stop/EOF. (G)

**D14 — cluster write-side method set** (concrete methods on `cluster.Cluster`,
not in `contracts`; H and I declare small consumer-side interfaces, Go-style):

```go
SetName(string)
SetVolume(float64)                                   // D35
SetOutputDelayMs(int)                                // D36
SetFollowing(id.ID)                                  // Zero = solo
SetPlayback(group id.ID, p contracts.Playback)
SetGroupSettings(group id.ID, s contracts.GroupSettings)
SetGroupName(group id.ID, name string)
Observe(peer id.ID, ip string)
DialCandidates(peer id.ID) []netip.Addr              // best-first
Join(addrs []string) error                           // seed list / discovery
```
(C/H/I)

**D15 — go:embed lives in `web/embed.go`** (`package web`,
`//go:embed all:dist`, exports `DistFS`), because `go:embed` cannot reference
parent dirs from `internal/api`. The API piece takes the FS via its config.
✎S (I)

**D16 — `FollowClient` is implemented in `internal/api` as a plain
cluster-backed HTTP client** (no dependency on the Echo server), so K builds:
cluster → followClient → group engine → api server. No construction cycle.
(H/I/K)

**D17 — takeover forwarding is I's job** (proxy hop to current master);
`group.MakeMaster` assumes it executes on the master and errors with
`ErrNotMaster` otherwise. H owns re-pointing the clock follower
(`SetMaster(addr, gen)`) whenever the elected master endpoint or generation
changes. *(Endpoint-management half superseded by D22 — the subscribe model
removes per-member stream endpoints; clock re-pointing stands.)* (H/K)

**D18 — ~~stream endpoints~~ SUPERSEDED by D22**: there is no `Resolver` /
`SetEndpoints` seam. Subscribers dial the master's `SOURCE_PORT` (resolved
via `cluster.DialCandidates(master)`); the source streams back to the address
each subscription actually came from. (H/K)

**D19 — `/api/status` JSON envelope** (pinned for I, J, and the e2e):

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
(I/K; `role:"solo"` = master of a group of 1; `source` present only while
this node runs an active audio source.)

**D20 — `--join` / `ENSEMBLE_JOIN`** (comma-separated `host:gossipPort` seed
list) is added as a dev flag in A, passed to `cluster.Join`. It exists for
hermetic loopback e2e tests; mDNS remains the production path. Added to spec
§2 as dev-only. (K)

**D21 — ~~bufferMs is fixed per session~~ PARTIALLY SUPERSEDED by D23**:
settings changes now apply live — the master bumps the generation and
broadcasts RECONFIG; subscribers re-fetch replicated group settings and
resubscribe (spec §8.7). Still true: sink `Stats().Synced` is computed live
from the Clock at call time; `Backend.Write` may block; the exec backend gets
a write deadline via process kill on Close — accepted v1 limitation. (E)

---

## Audio source/sink restructure (second review round)

Spec §6/§8 were reworked after user review: subscribe-based streaming on a
dedicated SOURCE_PORT, source ring + burst priming, live settings changes,
a continuous DAC rate servo, and interchangeable source/backend
implementations. Decisions D22+ pin the parameters; arch docs D, E, G, H, K
were regenerated against them.

**D22 — subscribe model on SOURCE_PORT (default 9200, TCP+UDP,
bind-or-increment)**: the source listens; members subscribe via stream
control (§8.7: HELLO/BYE/RESTART/RECONFIG, packet types 0x20–0x23, 1-byte
flag payload). UDP subscribers HELLO **from their STREAM_PORT mux socket**,
so audio flows back to the observed source addr:port and the member-side
receive path (mux types 0x01/0x02) is unchanged. TCP subscribers dial
SOURCE_PORT; control + length-prefixed audio share the connection. HELLO
keepalive every 5 s; subscriber expiry 15 s. The master's own sink subscribes
over loopback like any client. Inbound SOURCE_PORT only matters on masters,
but every node binds it (any node can become master). (G/H/K)

**D23 — live settings changes**: master bumps gen, broadcasts RECONFIG,
refreshes the replicated group-settings record; subscribers re-read settings
and resubscribe under the new gen. RECONFIG with the stop flag is the
explicit end-of-session notice (§8.6). (G/H)

**D24 — source ring & burst prime**: ring of released frames sized
`max(2 × bufferMs, 1 s)`. Prime = replay ring frames whose `pts + bufferMs`
deadline is still future (older frames are skipped — useless to the
newcomer). UDP burst pacing ~4× realtime (one frame per ~5 ms); TCP
back-to-back. Primes are counted in SourceStats (at burst initiation).
*Implementation refinement:* a priming subscriber is **excluded from live
fan-out** until its burst has caught up to the live edge via the ring
(`framesAfter` loop) — otherwise an interleaved live frame would anchor the
newcomer's reorder window/sink ahead of the burst and the entire prime would
be dropped as late. The >‑realtime burst rate guarantees catch-up
terminates. (G)

**D25 — rate servo (E)**: skew estimator — cumulative samples consumed vs
master-clock elapsed, ~3 s averaging window; backend `DelayReporter`
(`DeviceDelay()`) used when implemented, backpressure inference otherwise —
feeding a PI controller, output clamped ±2000 ppm and slew-limited (tuned aggressive per user testing), driving a
4-tap Catmull-Rom fractional resampler between jitter buffer and backend.
Runs continuously (drift *prevention*); underruns stay silence + watchdog →
RESTART (§8.6: starved > 2 s → RESTART to the source; still starved →
unsubscribe, group self-heal takes over). Target buffer level = bufferMs.
`SinkStats` gains `RatePPM`, `Buffered`. (E/G)

**D26 — media-source abstraction (D)**: scheme-keyed factory `file` / `http` /
`input` → one `Source` contract (canonical-PCM `ReadFrame(dst)`, `Close`,
D9 EOF semantics). Pull-paced (`file`: decode-ahead, EOF ends session) vs
live-paced (`http`/`input`: never EOF, underflow → the release ticker emits
silence, cadence never stalls). `input` is exec-capture (`pw-record`/
`arecord`), mirroring the exec playback backend. Available schemes reported
in `capabilities.sources`. (D/H)

**D27 — sink-backend registry (E)** *(amended by D32 — no build tags)*:
named backends `alsa` (runtime-loaded libasound, implements `DelayReporter`,
**v1**), `exec` (fallback pipe), `null`, `file` — all in the one and only
build; `alsa` registers itself only when the dlopen probe succeeds.
`ENSEMBLE_OUTPUT` selects by name; `auto` picks **alsa → exec → null**.
Available names reported in `capabilities.backends`; `playback` = a real
(non-null) backend is usable. (E/K)

**D28 — source stats surfacing**: `SourceStats{Clients, Connects, Restarts,
Primes}` — in `/api/status` (D19) and riding the master-written replicated
playback record (`Playback.Source`), refreshed with the periodic position
update, so the UI reads it from the cluster snapshot. (G/C/I/J)

**D29 — seam names follow G's concrete exports** (consistency-sweep
resolution): the source server is `source.NewServer(source.Config)` with
`StartSession / ReleaseFrame / Reconfig / StopSession / Stats`; the
subscriber is `stream.NewClient` (package `internal/stream`) with
`Subscribe(sourceAddr, gen, transport)` / `Unsubscribe` / `Counters`. H's
consumer-side interfaces adopt these method names (its `Publish` /
`SubscribeTo` / `EndSession` spellings in H-group.md are superseded). (G/H/K)

**D30 — live-source underflow is D's problem, not H's**: live sources
(`http`, `input`) emit silence internally on momentary underflow and
`ReadFrame` returns `nil` — there is **no** `audio.ErrUnderflow` sentinel;
H-group.md's references to it are superseded. The release ticker always gets
a frame. D's `Open(ctx, uri, mediaDir)` signature stands; H bridges with a
closure. (D/H)

**D31 — no `api.SetGroup`**: H depends on I only via `contracts.FollowClient`
(leaf package, no cycle), so K builds the API server **last** with the group
engine in `api.Config` and obtains the follow client via
`apiSrv.FollowClient()` — actually constructed standalone before the engine:
`api.NewFollowClient(cluster)` → `group.New(...)` → `api.New(Config{Group:
engine, ...})`. K-main-e2e.md's `SetGroup` fallback is unused. (H/I/K)

---

## Runtime library loading (third review round)

User question: can the binary ALWAYS carry libopus/libalsa support and probe
loadability at runtime, degrading gracefully? Yes — and it kills the
build-tag convention entirely. There is exactly **one build**, no cgo.

**D32 — runtime loading via purego (`internal/dl`, S-owned)**: optional
shared libraries are loaded with `github.com/ebitengine/purego`
(dlopen/dlsym FFI from pure Go, works with CGO_ENABLED=0).
`dl.Open(sonames, symbols)` tries sonames in order (`libopus.so.0` then
`libopus.so`; `libasound.so.2` then `libasound.so`), and **dlsym-verifies
every required symbol before any `RegisterLibFunc`** — a missing library,
wrong version, or missing symbol yields `dl.ErrUnavailable` (soft), never a
panic, and the corresponding capability is simply reported off (D3).
Call-rate is ~50/s (per 20 ms frame) so FFI overhead is irrelevant. The
CGO_ENABLED=0 static-build interaction is purego's documented supported mode;
verified at implementation time. Build tags for `opus`/`alsa` are abolished
everywhere. (S/D/E/K)

**D33 — opus placement & validation (supersedes the §8.3 build-tag text)**:
the codec module lives in `internal/audio` (piece D):
`audio.NewOpusEncoder() / NewOpusDecoder()` return `dl.ErrUnavailable` when
libopus isn't loadable. The ~7 functions bound: `opus_encoder_create`,
`opus_encoder_ctl` (bitrate 128k), `opus_encode`, `opus_encoder_destroy`,
`opus_decoder_create`, `opus_decode`, `opus_decoder_destroy`. **Master
encodes once** (H wires it between source `ReadFrame` and `source.Server`
fan-out); **each member decodes** (wired between the subscriber's deliver
callback and `Sink.Push` — the sink always consumes canonical PCM).
No packet-loss concealment on the decoder — a lost opus frame is silence,
same as pcm (§8.5); keep simple. (D/E/G/H)

**D33 amendment — session codec NEGOTIATION (supersedes the reject-behavior)**:
the master no longer rejects `play` when a member lacks the wanted codec — it
**negotiates** the EFFECTIVE codec at every session start (play, resume, settings
change) AND on membership change mid-session. The effective codec is the wanted
`settings.codec` iff every CURRENT member's effective caps
(`NodeView.Capabilities`, already probed-minus-disabled D40) include it AND this
master can encode it, else `"pcm"` (always universal). A downgrade is logged
(`INFO codec negotiated wanted=opus got=pcm lacking=[names]`); the replicated
playback record shows the EFFECTIVE codec. **Mid-session renegotiation**: the
master's reconcile (`group/watch.go renegotiateLocked`) detects that a running
opus session is no longer supported by all members (a member disabled opus, or a
non-opus node joined) and **downgrades in place** exactly like a live settings
change — bump gen, swap the session encoder to nil (atomic, the run goroutine
loads it per tick), `Source.StartSession`, `Reconfig` (members reconnect via the
RECONFIG machinery), re-point, rewrite the record — resuming from the current
position. Only DOWNGRADES auto-apply mid-session; an upgrade (codec became newly
possible) waits for the next play/settings change (keep it simple). The opus
factory returning `dl.ErrUnavailable` at encoder build still fails the play with
`ErrNoOpus` (a genuine runtime failure, distinct from negotiation). (D/E/G/H,
spec §8.3)

**D33 amendment — late-join stale-gen fix (member-side deliver)**: a node that
follows into an ALREADY-PLAYING group (or leaves + rejoins mid-stream) primes
correctly from the source ring, but the member-side `newDeliver` closure cached
its `curGen` across subscriptions while `repointLocked` independently `Sink.Reset`s
the sink to a guessed gen on (re)subscribe. When the master's real gen happened to
equal the closure's cached gen, deliver skipped re-arming and the sink stayed armed
at the guessed gen — every frame dropped as stale-gen and the joiner starved. Fix:
deliver consults the sink's ACTUAL armed gen (`*sink.Playout.ArmedGen()`) and
re-arms whenever the sink is not armed at the incoming frame's gen, regardless of
its own cache. (E/H)

**D34 — alsa backend (E, v1)**: simple-API binding via `internal/dl`:
`snd_pcm_open(&pcm, "default", PLAYBACK, 0)`,
`snd_pcm_set_params(pcm, S16_LE, RW_INTERLEAVED, 2, 48000, 1, latencyUs)`,
`snd_pcm_writei` per frame with `snd_pcm_recover` on `-EPIPE`/`-ESTRPIPE`,
`snd_pcm_delay` implementing `contracts.DelayReporter` (exact servo
measurement), `snd_pcm_close`. Registers in the backend registry only when
the probe succeeds; first in `auto` order (D27). (E)

---

## Per-node volume & output-delay calibration (user addition)

**D35 — per-node volume (live software gain)**: `volume` float `0.0–1.0`,
default `1.0`. Stored in `node.json` (D1 amended: `{id, name, volume,
outputDelayMs}`) and the replicated node record; set via
`PATCH /api/node {volume}` (UI proxies to the node). Applied in the sink as
the last stage before the backend (`Sink.SetGain`): per-sample int16
multiply, target read atomically each frame, linear ramp over one 20 ms
frame on change — continuous, no restart. Gain applies on every backend
(incl. null/file). `0.0` is a real value (muted): absent-field defaulting to
`1.0` happens **only** in A's presence-aware node.json decode; every layer
downstream (K→C→E) treats the resolved value as authoritative — no
zero-means-unset remapping anywhere. Hardware-mixer volume is out of scope
v1. (A/C/E/I/J)

**D36 — per-node output-delay calibration**: `outputDelayMs` int, default 0,
clamped ±500. Compensates fixed downstream latency invisible to both the
servo and `DeviceDelay()` (pipe player internals, DAC/amp/BT chains).
Playout deadline = `MasterToLocal(pts) + bufferMs − outputDelayMs`. Stored
like D35; set via `PATCH /api/node {outputDelayMs}`. Sign convention:
**positive = the device chain is late → write earlier** (the deadline
subtracts it); `Sink.SetDelayOffset` takes nanoseconds (I converts
`ms · 1e6`). A live change calls `Sink.SetDelayOffset` → the sink drops its
buffer and fires the restart hook (RESTART → burst re-prime, §8.6) — the
user-visible cost is a sub-second blip on that node only. (A/C/E/I/J)

## Output-device selection (user addition)

**D37 — output-device selection**: each node may select which ALSA device the
alsa backend opens, instead of always `default`. *Enumeration source* is
`/proc/asound/pcm`, parsed with zero external deps (`sink.ListOutputDevices` →
`parseProcPCM`, pure/testable): playback-capable `CC-DD` lines become
`{ID:"hw:C,D", Desc:<id>}`, prepended with `{ID:"default", Desc:"system
default"}`. The list is empty when libasound is not loadable (the alsa backend
never registered → selection is meaningless) OR `/proc/asound/pcm` is missing.
It is enumerated **once at startup** and reported on the node record
(`OutputDevices []contracts.OutputDevice`) plus the resolved `NodeView`.
*Persistence*: `node.json` gains `outputDevice` (default `"default"`,
presence-aware decode + clamp/normalize like `volume`); `config.SetOutputDevice`
mirrors `SetVolume`. *Selection semantics*: `sink.OpenDevice(spec, device, log)`
routes the configured device down the alsa path (auto-selected or explicit
`alsa`); every other backend ignores it — the **exec backend ignores the device
in v1** (plays to its tool's own default). *Live apply*: `PATCH /api/node
{outputDevice}` validates against the node's own enumerated list or `"default"`
(≤64 chars, non-empty), then persist (A) → replicate (C, `SetOutputDevice`) →
apply: only when the active backend kind is alsa, K reopens the backend for the
new device and calls `Playout.SwapBackend(b)` (under the sink mutex: close old,
set new, re-assert `DelayReporter`, log `output backend swapped`). A brief audio
blip is accepted; the session is **not** restarted. (A/C/E/I/J/K)

## Per-group play/pause (user addition)

**D39 — per-group play/pause toggle**. A new playback state `paused` joins
`idle`/`playing`, written only by the master into the replicated playback record.
- **Pause** (`POST /api/pause`, master-only; 409 `not_playing` when nothing is
  playing or already paused): the master freezes the session — it stops releasing
  frames (the 20 ms ticker keeps ticking but reads/publishes nothing) yet KEEPS
  the media source open and the session/generation alive, with the position
  frozen. It writes `playback.state="paused"`. The member-side session gating
  (group/watch.go) already treats only `state=="playing"` as active, so flipping
  to `paused` cleanly unsubscribes every member (BYE) and `Disarm()`s their sinks
  through the existing path; the master leaves its own loopback subscription too
  and broadcasts RECONFIG/stop so any still-attached subscriber drops.
- **Resume** (`POST /api/resume`, master-only; 409 `not_paused` when not paused):
  the master bumps the generation and re-anchors `sessionStart =
  LocalToMaster(now)+lead`; the frame index resets to 0 and the source continues
  from where it stopped, so **audio is contiguous though pts restart with the new
  generation** (pts stay monotonic within a generation; the gen bump is the
  re-anchor boundary that receivers already handle). It re-arms the source session
  (`StartSession`), re-points local plumbing, resumes releasing, and writes
  `playback.state="playing"`.
- **Live sources** (`Live()==true`): pause discards whatever arrives meanwhile
  (the live readahead already drops on overflow); resume returns at the live edge.
- The UI playback bar is play/pause aware: ⏸ pause while playing, ▶ resume + a
  paused indicator while paused; stop stays in both. (spec §4 playback record,
  §9.1 routes, §11 updated.)

## Tri-state feature display + per-node disable toggles (user addition)

**D40 — operator-disabled features (effective capabilities)**. A node may turn
off three features locally: `playback` (output), `opus` (codec), `input`
(capture). Persistence + replication mirror D35/D37:
- **node.json** gains `disabled:[…]` (presence-aware decode, default empty;
  `config.Store.SetDisabled([]string)` / `config.Config.SetDisabled`, normalized
  to the valid subset, deduped + sorted, via the same atomic temp+rename).
- **Replicated node record** carries the operator's `disabled` list AND the
  PROBED capabilities (K passes probed caps as today). Cluster setter
  `SetDisabled([]string)` (a D14 extension) bumps version + broadcasts.
- **Effective capabilities** = probed − disabled, computed in ONE place
  (`cluster.effectiveCaps`, applied in `nodeView` when projecting `NodeView`):
  disabling `playback` → `Playback:false`; `opus` → removed from `Codecs`;
  `input` → removed from `Sources`. Backends/formats untouched; probed caps are
  never mutated, so re-enabling restores them. `NodeView` also exposes the raw
  `Disabled []string` so the UI can render tri-state.
- **API**: `PATCH /api/node {disabled}` validates a subset of
  `{playback,opus,input}` (400 `bad_disabled` otherwise), then persist → replicate
  → apply. The live apply (`ApplyDisabled`, K) swaps the sink to the **null**
  backend when `playback` is newly disabled and reopens the configured
  device/backend when re-enabled (mirroring D37's `ApplyOutputDevice`); `opus`/
  `input` need no swap. Belt-and-suspenders, the LOCAL constructors refuse too:
  K's media factory rejects an `input:` URI and its opus factory returns
  `dl.ErrUnavailable` (→ `ErrNoOpus`) while disabled, and the member deliver path
  drops opus frames — but the primary gate is the effective-caps subtraction
  (master-side D33 NEGOTIATION downgrades an opus session to pcm when a member
  lacks the `opus` cap, rather than rejecting — see the D33 negotiation amendment).
- **UI**: capability chips for playback/opus/input are tri-state with three
  UNMISTAKABLE states, grouped under a small "features" label distinct from the
  passive "formats" chips: **ON** (available + enabled) solid green `●`, click to
  disable; **OFF** (available but disabled) amber outline `○`, click to enable;
  **UNAVAILABLE** (not probed on this host) dimmed + struck `✕`, NOT clickable
  (cursor not-allowed), tooltip "not available on this host". Cursor pointer only
  on the togglable (on/off) chips; pcm/wav/mp3/flac stay plain non-toggleable.
  (spec §1, §9.1 PATCH row updated.)

## Persisted cluster config (user addition)

**D41 — persisted cluster lookup tables** *(narrowed by D44, amended by D47)*. The
long-lived lookup state persisted to `DATA_DIR/cluster.json` is the group
override-**NAMES** map (XOR-keyed) PLUS this node's **OWN group-settings record**
(keyed by self id, since group id == master id, D44/D47) — each as a FULL record
(incl. `version` + `writer` so the LWW merge applies). NOT node records, NOT
playback (runtime/replicated), and NOT peers' settings records (master-keyed live
state owned by other nodes).
- **Load** at cluster `New` (before memberlist join) into the doc; once gossip
  starts, the exact existing LWW rule reconciles loaded-vs-gossiped (an older
  loaded version loses to a newer gossiped record, and vice-versa).
- **Save** debounced — a `markDirty` from any names/settings change (setter or
  gossiped merge) (re)arms a ~2 s timer; a change storm coalesces to a bounded
  number of writes — plus a final save on `Close`. Atomic temp+fsync+rename in
  the same dir, like `node.json`.
- A **missing** file is a clean empty start; a **corrupt** file warns and starts
  empty (never fatal). The path is wired via `cluster.Config.StatePath` (K passes
  `filepath.Join(cfg.DataDir, "cluster.json")`); an empty path (tests) disables
  persistence entirely. A `SaveDebounce` config + `saveNotify` test hook (injected
  timing) make the debounce deterministic in tests.
- **Purge horizon**: group names + settings are exempt from the 30-day purge
  entirely — the lookup table is kept indefinitely (spec §4 updated). Node
  records + playback still age out at 30 days.

## Opus-by-default + playback-record follows the group id (user round)

**D42 — opus is the default codec, with transparent pcm downgrade**.
`contracts.DefaultCodec` is now `"opus"` (was `"pcm"`). Rationale: a raw-PCM
stream datagram is `24 + 3840 = 3864 B` and IP-fragments into ~3 packets; on
lossy Wi-Fi losing any fragment drops the whole frame and the per-frame XOR FEC
cannot recover it (observed: Raspberry-Pi members on WLAN received clock packets
— 24 B, unfragmented — but no audio at all). A 20 ms opus packet is ~320 B, so
the datagram (~344 B) stays under one MTU. To keep "opus everywhere" from
breaking heterogeneous clusters, `group.Play` **downgrades the session to pcm**
when the group can't do opus (a member lacks the `opus` cap, or this master has
no opus encoder) instead of rejecting — opus is the default, pcm is the
universal fallback. An *explicit* `codec: opus` via `POST /api/group/settings`
is still validated against the master's own capability (`validateSettings`).
(spec §8.3 updated.)

**D43 — the master's playback record follows the live group id**. Bug fix: the
group ID is the XOR of the member set (§5), so it CHANGES whenever a member
joins/leaves. The session captured its group id at `Play` time and the heartbeat
kept writing the playback record under that stale id; when members left (e.g. the
Pis disconnected), the surviving group's new id had no playback record, so every
remaining member — including a co-located secondary — saw `state="idle"` and
**stopped playout**, and it never self-healed (the master kept writing the wrong
id). Fix: each reconcile, a master with a running session compares
`sess.groupID` to the freshly-derived `mv.group.ID`; on a change it clears the
OLD record to `idle` and immediately writes the live record under the NEW id
(then `sess.groupID` tracks it). Membership churn no longer stalls playout.

## Group id = master id + hybrid naming (user round)

**D44 — the group id is the MASTER's node id; hybrid naming; settings carry over
on takeover**. A redesign of group keying that kills membership-churn orphaning by
construction. **This supersedes the XOR-of-members group-id wording of D5/§5 and
the §4 group-id record keying, and REPLACES the D43 churn re-point** (the re-point
patch from 2a8bab7 is removed). D41 is narrowed to names-only.

- **Group id = master id**. `DeriveGroups` keys the group — and its master-written
  PLAYBACK + SETTINGS records — by the master's node id (solo group id = the
  node's own id). Membership churn (a member joining/leaving, master unchanged)
  no longer changes the id, so those records are never orphaned; the id changes
  only on a master move (a takeover, which stops the session first). `GroupView.ID
  = master`. The former D43 reconcile-time playback re-point is deleted (the bug
  it patched can no longer occur).
- **Hybrid naming**. The explicit name OVERRIDE map stays keyed by the member-set
  **XOR** (an override names a specific COMBINATION of rooms; survives master
  changes + re-forming). When no override exists, `DeriveGroups` computes a
  server-side **DERIVED** label from the member NAMES: sorted, joined with `" + "`,
  capped at the first 3 then `" +N more"`; solo = the node's name; missing member →
  8-char short id. `GroupView.NameIsDerived` (json `nameDerived`) reports which.
  `POST /api/group/name` resolves the group's CURRENT member set, computes its XOR,
  and writes the override there; an **empty name CLEARS** it (back to derived). The
  UI renders derived labels muted/italic and clears on an empty rename.
- **Settings carry over on takeover**. Because settings are now master-keyed,
  `group.MakeMaster` copies the current master's settings record to the new
  master's key during the handoff (one extra `SetGroupSettings`). Playback does
  NOT carry (takeover stops the session, as today).
- **Persistence (amends D41)**. `cluster.json` persists the override-NAMES map
  ONLY (XOR-keyed, purge-exempt, kept forever). Group settings are NO LONGER
  persisted (master-keyed live state); node records + playback stay unpersisted.
  (spec §4/§5/§9.1 updated.)

## Persisted following — rejoin on return (user round)

**D45 — a node persists its `following` and rejoins its previous group on
return; self-heal clears it; grace is the decision window**. A node that
temporarily disappears (reboot, crash, brief drop) should come back into the
group it was in, without operator action.

- **Persist (amends D1)**. `node.json` gains `following` — a 32-hex node id, or
  `""` for solo (default). Presence-aware decode like `volume`; an absent or
  malformed value loads as `""` (warn + treat as empty, never fatal). The config
  `Store` gains `SetFollowing(id.ID)` following the `SetVolume` pattern (`id.Zero`
  persists as `""`); `Config` exposes the loaded value as `id.ID`.
- **Boot restore**. `cluster.Config` gains `InitialFollowing id.ID`; `cluster.New`
  seeds this node's own record's `Following` with it — gossiped from version 1,
  exactly as if `SetFollowing` had been called. K passes `cfg.Following`. The
  EXISTING machinery does the rest: if the old master is alive + a master,
  `DeriveGroups` re-forms the group; if it is dead/unknown, the §5 self-heal grace
  fires and resets the node to solo. There is **no new rejoin logic**.
- **Persist on change**. `group.Params` gains `PersistFollowing func(id.ID)`
  (nil-safe no-op; K wires it to the config store's `SetFollowing`). The engine
  calls it at EVERY site it writes `cluster.SetFollowing`: `Follow`, `Unfollow`,
  takeover-directed follow, and the self-heal reset — via one internal
  `setFollowing` helper that does both. Logged at debug.
- **Grace is the decision window**. The self-heal `healAt` arms when the engine
  first OBSERVES the dangling follow (the first stale reconcile), not at process
  start, so slow gossip convergence cannot insta-clear a follow that is merely
  still propagating. Verified: `heal.go` already keys off `mv.stale` per reconcile
  and arms `now + Grace` only on the first stale tick — no change needed.

**D46 — wire protocol v1: magic `0xE5` is the version marker; receivers ignore
unknown packet types; an incompatible revision changes the magic**. This makes
the wire format forward-compatible (new optional types are additive) and lets a
protocol-minimal, receive-only client (`docs/DUMB-CLIENT.md`, `cmd/dumbclient`)
interoperate without cluster membership. (§8.4)

## Editable + persisted group settings (user round)

**D47 — group settings are editable per group in the UI and persist with the
group's master** (amends D41; re-introduces settings persistence that D44 had
removed, now keyed correctly by self).

- **UI**. The group card replaces the static "codec · transport · buffer" line
  with compact inline controls: `codec` select (`pcm`/`opus`), `transport` select
  (`udp`/`tcp`), and a `bufferMs` slider (50–500, step 25, ~250 ms debounce like
  VolumeSlider). A change POSTs the FULL current trio with the one field applied to
  `POST /api/group/settings`, addressed to the group's master via the proxy
  (`base(group.master)`) — the endpoint is master-only but the proxy makes it work
  from any viewer. Changes apply live mid-session (RECONFIG, D23). `lib/api.js`
  `setGroupSettings(masterId, settings)`.

- **Persistence**. Because the group id == the master id (D44), a master's
  settings record is keyed by its own node id. The cluster persists ONLY the
  self-keyed settings record (full LWW record, incl. version+writer) to
  `cluster.json` alongside the override-names map — loaded at `New` before gossip,
  saved debounced + on `Close`, exactly like names. A peer's settings record is
  master-keyed live state and is NOT persisted. `SetGroupSettings` marks the table
  dirty only when `group == self`; a gossiped settings merge marks dirty only when
  the changed key is `self`.

- **Restart-version guard (D7 for settings)**. On restart the loaded own-settings
  record may sit below a version a peer still holds (broadcast but not yet saved
  before the crash). `reconcileOwnSettingsVersion` (called from the delegate's
  `MergeRemoteState`, mirroring the node-record `reconcileOwnVersion`) jumps our
  local counter above any peer copy with version ≥ ours, re-broadcasts, and
  re-persists — so the next local `SetGroupSettings` wins.

- **Effect**. A master that restarts against the same data dir re-forms its (solo)
  group with its last codec/transport/bufferMs instead of cluster defaults
  (spec §4/§8.4 updated). (C/H/I/J)

## Confirmed as designed (no change)

- C's two-mutex exception (doc + liveness) with a never-hold-both rule. (C)
- D imports only the PCM constants from `package stream`. (D)
- Sink `Push` is fire-and-forget; no backpressure/close signal to G. (E/G)
- G's transport `Counters` and E's `SinkStats` stay separate; `/api/status`
  surfaces sink stats (D19); transport counters may be added later. (G/I)
- `/api/status` carries only `groupId`/`role`; the full group object comes
  from `/api/cluster`. (I)
- Loopback e2e: nodes on 127.0.0.1 have empty `InterfaceCIDRs`; reachability
  comes from `--join` seeds + observed-IP reporting (memberlist + HTTP traffic
  both feed `Observe`). (K)
- K reconciles exact constructor names at integration (the fix-loop). (K)

**D38 — bring-up aids (user round)**: `POST /api/tone` plays a 1 s 440 Hz
tone through the node's local output backend (`Sink.TestTone`; 409 while a
session or tone is active; respects the live volume) — surfaced as a
"test tone" button per node in the UI, proxy-able. And: the initial UDP
HELLO may be lost, so until the FIRST frame arrives the subscriber re-HELLOs
(prime-me) 3× at 500 ms before falling back to the 5 s keepalive cadence.
(E/G/I/J)
