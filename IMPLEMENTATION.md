# Implementation plan

Source of truth: [docs/README.md](docs/README.md). This file breaks the system
into pieces small enough for one agent each. Every piece owns a **disjoint set
of files**; cross-piece contracts live in the skeleton (piece S) and may only
be changed by the integrator.

Architecture notes per piece: `docs/arch/<piece>.md` (written by architect
agents before implementation).

## Pieces

### S — skeleton *(integrator, before everything)*
`go.mod`, `Makefile`, `.gitignore`, directory tree, and the shared contracts:
- `internal/id`: `ID [16]byte`, `New()`, `Parse(string)`, `String()` (hex),
  `XOR(ids ...ID) ID`, JSON marshalling.
- `internal/stream/wire.go`: frame header struct + encode/decode
  (`magic|type|gen|seq|pts|len`), packet type constants (audio `0x01`,
  fec `0x02`, clock req `0x10`, clock resp `0x11`, stream control
  hello/bye/restart/reconfig `0x20–0x23`), canonical PCM constants
  (48000 Hz, 2 ch, s16le, 20 ms, 3840 B).
- `internal/stream/mux.go`: UDP mux owning the STREAM_PORT UDP socket;
  `Register(type byte, h func(pkt []byte, from netip.AddrPort))`,
  `WriteTo(pkt, addr)`.
- `internal/netx`: `BindTCPUDP(base int, tries int) (tcpLn, udpConn, port, err)`
  bind-or-increment (all-or-nothing per port), `BindTCP` for HTTP,
  `InterfaceCIDRs() []string`.
- Interface types consumed across pieces (defined where they're consumed,
  Go-style, but pinned in arch docs): output backend, frame sink, state store.

### A — identity & config
`internal/config/*`. Flags + env fallbacks per spec §2 (incl. SOURCE_PORT and
the dev `--join` seed list), `node.json` load/create (id, name) with atomic
rewrite on rename, `MEDIA_DIR`/`DATA_DIR` resolution. Pure, unit-tested.

### B — discovery (mDNS)
`internal/discovery/*`. zeroconf register (TXT: id/gossip/http/stream/source)
+ continuous browse; emits `Peer{ID, Addr, GossipPort, HTTPPort, StreamPort,
SourcePort}` on a channel, dedup/throttled. No memberlist dependency: the cluster piece
consumes the channel.

### C — cluster state (gossip)
`internal/cluster/*`. memberlist wrapper (delegate: broadcasts + push/pull
TCP), replicated LWW doc per spec §4 (node records, group names, playback),
observed-IP tracking (`Observe(peerID, ip)` fed by gossip + HTTP), address
candidate resolution per §3.1 (CIDR ∩ observations), liveness events,
30-day purge, change notifications (`Subscribe() <-chan struct{}`).
In-memory only; this node's own record fields set via setters
(`SetName`, `SetFollowing`, `SetPlayback`, …) that bump version + broadcast.

### D — media sources
`internal/audio/*`. Interchangeable media sources behind one contract
(`ReadFrame(dst []byte) error` filling canonical 20 ms PCM frames, D9 EOF
semantics), created by a scheme-keyed factory (spec §6.1, D26): `file`
(decoders wav/mp3/flac, mono→stereo, linear resample to 48k), `http(s)`
(same decoders over a response body; live-paced, never EOF), `input`
(exec-capture via pw-record/arecord, mirroring E's exec playback). Unit tests
with generated WAV fixtures, an httptest server, and a fake capture command.

### E — sink & playout
`internal/sink/*`. Output backends as a **named registry** (D27): `exec`
(`pw-play`/`pw-cat -p`/`aplay`/`paplay` pipe, auto-pick), `null` (timed
discard), `file` (debug); `alsa` slot reserved behind a build tag (v1.1) —
v1 ships the registry and the optional `DelayReporter` seam. Jitter buffer
keyed by seq, playout loop translating pts via the `Clock` contract, silence
insertion, late-drop counters, generation gating, and the **continuous rate
servo** (D25): skew estimator → PI controller (±500 ppm clamp, slewed) →
4-tap Catmull-Rom fractional resampler between buffer and backend. 2 s
starvation watchdog triggers the subscriber's RESTART hook (callback injected
by G/H). Testable with the null backend, a fake clock, and a skewed fake
DAC.

### F — clock
`internal/clock/*`. Server (registers type 0x10 on the UDP mux, answers with
0x11) and follower (1 Hz request loop, 5-best-of-30 median offset, resync on
generation/master change). Exposes `Follower.MasterNow() (int64, bool)`
(synced flag). Uses only the mux contract from S.

### G — audio source server & subscriber client
`internal/source/*` + `internal/stream/*` (wire.go + mux.go are S, read-only
here). **Source** (master side, spec §8.2/§8.7, D22–D24): SOURCE_PORT
TCP+UDP listeners, subscriber registry with keepalive/expiry, ring buffer of
released frames, burst prime, RECONFIG broadcast, per-frame fan-out (UDP
datagrams + XOR FEC parity every 4 frames, or length-prefixed frames down
subscriber TCP conns), SourceStats. **Subscriber** (member side): HELLO/
keepalive/BYE/RESTART client, UDP receive path (mux types 0x01/0x02, reorder
+ FEC recovery window) and TCP subscription path; both deliver
`(header, payload)` to a callback. Loss/recovery counters. Unit tests over
loopback.

### H — group engine
`internal/group/*`. Group derivation consumption (C owns the algorithm, D5),
follow/unfollow with validation, self-heal (10 s grace), takeover
orchestration (§5.2, calls members over HTTP via a small client func injected
from API piece), playback orchestration: on `Play` → media source (D) for the
URI → ticker release → source server (G) which subscribers join; the local
sink subscribes to whichever master's source is current (incl. self over
loopback); manages generation, RECONFIG on settings change (D23), playback
status record incl. SourceStats (C, D28), group settings replicated LWW,
written by master. End-of-source/stop handling.

### I — HTTP API
`internal/api/*`. Echo server: all REST routes (§9.1), WebSocket (§9.2,
debounced cluster pushes + 5 s heartbeat), node proxy middleware (§9.3,
one-hop guard, id-or-unique-name), SPA serving from `web/dist` via go:embed
(with graceful fallback page when dist is the placeholder), Observe() feed of
client IPs for §3.1. Thin: delegates to cluster (C) and group (H).

### J — web UI
`web/*`. Svelte 5 + Vite, JS, hand-written CSS, three sections per §10,
WebSocket store with auto-reconnect, fetch wrappers, proxy-aware media
browser, join/leave/make-master/play/stop/rename actions. `npm run build` →
`web/dist` (gitignored; placeholder index.html committed so go:embed works).

### K — main & e2e
`cmd/ensemble/main.go` wiring (S→A→B/C→F/G→E→H→I lifecycle, four port binds,
graceful shutdown), `scripts/dev2.sh` (two nodes, tmp data dirs, null sink env
var `ENSEMBLE_OUTPUT=null`), e2e smoke test script asserting: discovery,
cluster doc convergence, follow, derived group id = xor, takeover,
play→both sinks subscribe and receive frames in sync (/api/status sink
stats), late join gets burst-primed, RESTART recovers a lost subscriber,
source stats (clients/connects/restarts) surface on the master, stop works.

## Dependency waves

| Wave | Pieces | Notes |
|---|---|---|
| 0 | S | integrator writes contracts; `go build ./...` green |
| 1 | A, B, D, E, J | independent of each other; J only needs §9 API shapes |
| 2 | C, F, G | C needs B's Peer type; F/G need S's mux/wire |
| 3 | H, I | H needs C/D/E/F/G; I needs C/H; J finishes against real API |
| 4 | K | integration + e2e; fix-loop |

After each wave: `go build ./... && go vet ./... && go test ./...` must pass.

## Ground rules for agents

- Keep it **simple and basic** — no speculative abstraction, no feature not
  in the spec. Prefer 200 lines that obviously work over 600 that might.
- Don't touch files outside your piece. Contracts (S) are read-only; if a
  contract is wrong, report it back instead of editing it.
- Every piece ships unit tests that run without network root, audio
  hardware, or external files (loopback sockets, null backend, generated
  fixtures, fake clocks).
- Standard library first; allowed deps: echo v4, memberlist,
  grandcat/zeroconf, gorilla/websocket, hajimehoshi/go-mp3, mewkiz/flac,
  go-audio/wav (or hand-rolled wav).
- Log with `log/slog`, component-scoped (`slog.With("comp", "cluster")`).
