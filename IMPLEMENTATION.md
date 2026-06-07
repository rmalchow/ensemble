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
  fec `0x02`, clock req `0x10`, clock resp `0x11`), canonical PCM constants
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
`internal/config/*`. Flags + env fallbacks per spec §2, `node.json`
load/create (id, name) with atomic rewrite on rename, `MEDIA_DIR`/`DATA_DIR`
resolution. Pure, unit-tested.

### B — discovery (mDNS)
`internal/discovery/*`. zeroconf register (TXT: id/gossip/http/stream) +
continuous browse; emits `Peer{ID, Addr, GossipPort, HTTPPort, StreamPort}`
on a channel, dedup/throttled. No memberlist dependency: the cluster piece
consumes the channel.

### C — cluster state (gossip)
`internal/cluster/*`. memberlist wrapper (delegate: broadcasts + push/pull
TCP), replicated LWW doc per spec §4 (node records, group names, playback),
observed-IP tracking (`Observe(peerID, ip)` fed by gossip + HTTP), address
candidate resolution per §3.1 (CIDR ∩ observations), liveness events,
30-day purge, change notifications (`Subscribe() <-chan struct{}`).
In-memory only; this node's own record fields set via setters
(`SetName`, `SetFollowing`, `SetPlayback`, …) that bump version + broadcast.

### D — audio source
`internal/audio/*`. Decoders wav/mp3/flac → canonical PCM stream
(`Open(path) (FrameReader, error)`; `ReadFrame() ([3840]byte-ish, error)`),
mono→stereo, linear resampler to 48k. Unit tests with tiny generated fixtures
(WAV written by test code; mp3/flac decode tested when fixtures exist —
generate a wav fixture programmatically, skip codec-specific tests if no
fixture).

### E — sink & playout
`internal/sink/*`. Output backends: `pw-play`/`pw-cat -p`/`aplay`/`paplay`
exec-pipe (auto-pick, raw s16le 48k stereo stdin) + `null` (timed discard) +
`file` (debug). Jitter buffer keyed by seq, playout loop translating pts via
a `Clock` interface (`Now() int64` master-time), silence insertion, late-drop
counters, 2 s starvation watchdog, generation gating. Testable with the null
backend and a fake clock.

### F — clock
`internal/clock/*`. Server (registers type 0x10 on the UDP mux, answers with
0x11) and follower (1 Hz request loop, 5-best-of-30 median offset, resync on
generation/master change). Exposes `Follower.MasterNow() (int64, bool)`
(synced flag). Uses only the mux contract from S.

### G — stream transport
`internal/stream/{sender,receiver,tcp,fec}*.go` (wire.go + mux.go are S,
read-only here). Sender: fan-out to member endpoints, UDP datagrams + XOR
FEC parity every 4 frames, or persistent TCP conns (length-prefixed,
reconnect). Receiver: UDP path (mux type 0x01/0x02, reorder + FEC recovery
window) and TCP listener path; both deliver `(header, payload)` to a
callback. Loss/recovery counters. Unit tests over loopback.

### H — group engine
`internal/group/*`. Group derivation from cluster state (§5), follow/unfollow
with validation, self-heal (10 s grace), takeover orchestration (§5.2, calls
members over HTTP via a small client func injected from API piece),
playback orchestration: on `Play` → audio source (D) → ticker release →
sender (G) to all members incl. self; manages generation, playback status
record (C), group settings (codec/transport/bufferMs) stored in the group-name
record's sibling map… **settings live in the replicated doc keyed by group ID,
LWW, written by master**. End-of-file/stop handling.

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
`cmd/ensemble/main.go` wiring (S→A→B/C→F/G→E→H→I lifecycle, graceful
shutdown), `scripts/dev2.sh` (two nodes, tmp data dirs, null sink env var
`ENSEMBLE_OUTPUT=null`), e2e smoke test script asserting: discovery, cluster
doc convergence, follow, derived group id = xor, takeover, play→both sinks
receiving frames in sync (null backend stats endpoint via /api/status).

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
