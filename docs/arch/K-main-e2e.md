# K — main wiring & end-to-end

Source of truth: [docs/README.md](../README.md) (§2 four ports, §6 sources, §8
audio pipeline, §9 API). Contracts: [S-skeleton.md](S-skeleton.md). Integrator
decisions: [DECISIONS.md](DECISIONS.md) — this piece is governed by **D2, D3,
D6, D8, D16, D19, D20, D22, D23, D24, D25, D26, D27, D28, D32, D33, D34**.

This piece owns process lifecycle (`cmd/ensemble/main.go`) and the two scripts
(`scripts/dev2.sh`, `scripts/e2e.sh`). It writes **no** package code under
`internal/` — it only constructs the components other pieces export, in
dependency order, and tears them down in reverse. K is the only place that knows
the full graph, so it is the only place allowed to know every concrete
constructor. If a constructor signature here disagrees with what
A/B/C/F/G/E/H/I actually ship, K is the integrator's fix-loop; mismatches are
recorded as contract concerns (see §9), **not** worked around with new
abstractions.

The audio path is **subscribe-based** (D22): the group master runs an audio
**source server** on its `SOURCE_PORT`; every member — including the master
itself, over loopback — runs a **subscriber client** that HELLOs to the current
master's source and feeds frames into its local sink. There is **no**
master-dials-members push, no `Resolver`, no `SetEndpoints`: subscribers dial,
the source streams back to the address each subscription came from. K wires both
ends on every node (any node can become master).

---

## 1. Package / file layout

Files K creates and owns:

```
cmd/ensemble/main.go      flag/env parse → build component graph → run → signal → graceful shutdown
cmd/ensemble/main_test.go pure-helper unit tests (option parse, caps, follow client, LIFO unwind)
scripts/dev2.sh           launch two nodes, tmp DATA_DIRs, ENSEMBLE_OUTPUT=null, four disjoint ports, --join seed
scripts/e2e.sh            curl/jq smoke test against the nodes' REST API (the assertions of §7)
scripts/fixtures.sh       (thin) regenerate testdata/media/tone.wav via a go run helper, if absent
```

`main.go` is deliberately a single file: flag parsing, a `run(ctx) error`
builder, and `main()` that wires signals to a context and maps the error to an
exit code. No `internal/app` package, no DI container — the graph is ~14 nodes,
built top-to-bottom once. Target ≤ ~340 lines.

One non-obvious helper stays local to `main.go`: `followClient`, a tiny
`contracts.FollowClient` implementation (HTTP POST to a peer's `/api/follow` /
`/api/unfollow`, address resolved via the cluster per §3.1). Per **D16** the API
piece (I) owns the canonical `FollowClient` as a plain cluster-backed HTTP
client; K prefers I's (`apiSrv.FollowClient()`) and provides this fallback only
so the graph closes if I has not shipped it yet. The takeover e2e needs it to
work either way.

---

## 2. Concrete Go API

`main.go` exports nothing (package `main`). Internal shape:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ensemble/internal/api"
	"ensemble/internal/audio"
	"ensemble/internal/clock"
	"ensemble/internal/cluster"
	"ensemble/internal/config"
	"ensemble/internal/contracts"
	"ensemble/internal/discovery"
	"ensemble/internal/group"
	"ensemble/internal/id"
	"ensemble/internal/netx"
	"ensemble/internal/sink"
	"ensemble/internal/source"
	"ensemble/internal/stream"
)

// options is the fully-resolved configuration after flags+env. A owns the
// parse semantics (flag > env > default); K declares the flags, reads env
// fallbacks, and hands the values to config.Load.
type options struct {
	HTTPPort   int      // --http-port   / ENSEMBLE_HTTP_PORT   (default 8080)
	StreamPort int      // --stream-port / ENSEMBLE_STREAM_PORT (default 9090)
	SourcePort int      // --source-port / ENSEMBLE_SOURCE_PORT (default 9200)  ← NEW, D22/§2
	GossipPort int      // --gossip-port / ENSEMBLE_GOSSIP_PORT (default 7946)
	DataDir    string   // --data        / ENSEMBLE_DATA_DIR    (default ./data)
	MediaDir   string   // --media       / ENSEMBLE_MEDIA_DIR   (default DATA_DIR/media)
	Name       string   // --name        (initial node name; applied on first start only)
	Output     string   // ENSEMBLE_OUTPUT (env only, D2): "" → "auto" | "null" | "file:<path>" | backend name
	Join       []string // --join / ENSEMBLE_JOIN (D20): comma-sep host:gossipPort seeds (dev e2e)
	Host       string   // --host bind address, default "" (all ifaces); "127.0.0.1" in dev2/e2e
	LogLevel   string   // ENSEMBLE_LOG (debug|info|warn|error), default info
}

func main()                                            // parse → run(ctx) → exit code
func parseOptions(args []string, env func(string) string) (options, error)
func capabilities(opt options) contracts.Capabilities  // D3/D32: PATH probe + internal/dl dlopen probes
func run(ctx context.Context, opt options) error

// shutdownStack is a LIFO of teardown closures (§3.3). Pushed as resources are
// acquired; unwound in reverse on shutdown. Extracted so it is unit-testable.
type shutdownStack struct {
	fns []func(context.Context) error
}
func (s *shutdownStack) push(name string, fn func(context.Context) error)
func (s *shutdownStack) unwind(ctx context.Context, log *slog.Logger) error // reverse; logs each; returns first err

// followClient is K's fallback contracts.FollowClient (used only if I exports
// none): HTTP POST /api/follow|/unfollow to a peer, dialing an http addr
// resolved from the cluster (§3.1), carrying the X-Ensemble-Proxied guard.
type followClient struct {
	store  contracts.StateStore
	client *http.Client
}
func (f *followClient) Follow(ctx context.Context, peer, target id.ID) error
func (f *followClient) Unfollow(ctx context.Context, peer id.ID) error
func (f *followClient) httpAddr(peer id.ID) (netip.AddrPort, error) // DialCandidates ∩ httpPort
```

### 2.1 Assumed constructor surface (per piece)

The constructors K calls, following the spec + S + D22–D28 **subscribe model**.
Exact spellings are the integrator's to confirm in the fix-loop; any divergence
is a one-line edit in `run`. G and H are regenerated to the subscribe model
(source server + subscriber client; URI-scheme media factory), so the surface
below matches them; the remaining cross-piece naming mismatches to settle at
integration are recorded as contract concerns (§9).

```go
// ── A: config (§2, D2, D20) ─────────────────────────────────────────────────
config.Load(o config.Options) (*config.Config, error) // node.json (id,name) under DataDir; resolves MediaDir
cfg.NodeID   id.ID
cfg.Name     string
cfg.DataDir  string
cfg.MediaDir string
cfg.SetName(name string) error                          // atomic node.json rewrite on rename

// ── netx (S): bind-or-increment, all-or-nothing per port number ─────────────
netx.BindTCPUDP(host string, base, tries int) (*net.TCPListener, *net.UDPConn, int, error)
netx.BindTCP(host string, base, tries int) (*net.TCPListener, int, error)
netx.InterfaceCIDRs() []string

// ── stream (S): UDP mux over the bound STREAM_PORT UDP socket ───────────────
stream.NewMux(conn *net.UDPConn, log *slog.Logger) *stream.Mux
mux.LocalAddr() netip.AddrPort

// ── C: cluster (memberlist on the probed gossip port, D8; impls StateStore) ─
cluster.New(p cluster.Params) (*cluster.Cluster, error)
//   Params{ Self id.ID, Name string, HTTPPort, StreamPort, SourcePort, GossipPort int,
//           Addrs []string, Capabilities contracts.Capabilities, Log *slog.Logger }
cl.Observe(peer id.ID, ip string)
cl.Join(addrs []string) (int, error)                    // seed list (D20) + discovery peers
cl.DialCandidates(peer id.ID) []netip.Addr              // §3.1, best-first (used by followClient + source dial)
cl.Subscribe() <-chan struct{}
cl.Snapshot() contracts.Snapshot
cl.Self() id.ID
cl.SetFollowing(target id.ID)
cl.Shutdown() error                                     // memberlist Leave broadcast, then close

// ── B: discovery (mDNS register/browse; emits Peer, D4) ─────────────────────
discovery.New(p discovery.Params) (*discovery.Discovery, error)
//   Params{ Self id.ID, HTTPPort, StreamPort, SourcePort, GossipPort int, Log *slog.Logger }
d.Peers() <-chan discovery.Peer                          // {ID, Addr, GossipPort, HTTPPort, StreamPort, SourcePort}
d.Run(ctx context.Context) error
d.Close() error

// ── F: clock (server 0x10→0x11 on mux; follower 1 Hz, impls contracts.Clock) ─
clock.NewServer(mux *stream.Mux, log *slog.Logger) *clock.Server
clock.NewFollower(mux *stream.Mux, log *slog.Logger) *clock.Follower
fol.SetMaster(addr netip.AddrPort, gen uint32)           // H re-points on master/gen change (D17 clock half stands)
fol.Run(ctx context.Context)
fol.Close() error
// *clock.Follower satisfies contracts.Clock (MasterNow/MasterToLocal/LocalToMaster)

// ── E: sink + playout + rate servo (D25, D27) ───────────────────────────────
sink.PickBackend(name string, log *slog.Logger) (contracts.Backend, error) // "auto"|"null"|"file:<p>"|name (D27)
sink.New(cfg sink.Config) *sink.Playout                  // impls contracts.Sink
//   Config{ Backend contracts.Backend, Clock contracts.Clock, BufferMs int, Log *slog.Logger }
sk.Push(gen uint32, seq uint64, pts int64, payload []byte)
sk.Reset(gen uint32)
sk.Stats() contracts.SinkStats                           // incl. RatePPM, Buffered (D25)
sk.Close() error

// ── G: subscriber client (member side, internal/stream) + source server
//      (master side, internal/source), D22–D24 ─
// Subscriber: HELLO/keepalive/BYE/RESTART; UDP receive via mux 0x01/0x02 (+FEC),
// or TCP subscription; delivers (Header, payload) to a DeliverFunc callback.
// (G-stream.md names it stream.NewClient/*stream.Client — see §9 concern 1.)
stream.NewClient(cfg stream.ClientConfig) *stream.Client
//   ClientConfig{ Mux *stream.Mux, Deliver stream.DeliverFunc, Log *slog.Logger }
//   DeliverFunc = func(h stream.Header, payload []byte)
sub.Subscribe(sourceAddr netip.AddrPort, gen uint32, t stream.Transport) error // (re)point at master's SOURCE_PORT; prime-me
sub.Unsubscribe()                                        // BYE + stop
sub.Counters() stream.Counters                           // lifetime transport-health counters
sub.Close() error
// (The 2 s-starvation watchdog → RESTART lives inside the Client, §8.6/D25 —
// K does not call a Restart() hook.)
// Source server (only active while this node is master; bound on every node, D22):
source.NewServer(cfg source.Config) *source.Server
//   Config{ Self id.ID, UDP *net.UDPConn, TCP *net.TCPListener, Log *slog.Logger }
srcSrv.Stats() contracts.SourceStats                     // Clients/Connects/Restarts/Primes (D28)
srcSrv.Run()                                             // non-blocking; starts read/accept/sweeper loops
srcSrv.Close() error
// H drives the source server per session (open media → ticker release → fan-out
// to subscribers); K just constructs and starts/stops it.

// ── D: media-source factory (scheme-keyed: file/http/input, D26) ────────────
// D-audio.md exports audio.Open(ctx, uri, mediaDir) (audio.Source, error) +
// audio.Schemes(); H consumes it behind a MediaFactory seam. K wires whichever
// constructor D ships (a thin Opener wrapper or Open directly) into group.Deps.
// audio.Source: ReadFrame(dst []byte) error (D9 EOF); Live() bool; Close() error

// ── H: group engine (derivation consume + follow/takeover + playback) ───────
group.New(d group.Deps) *group.Engine
//   Deps{ Cluster <cluster write+read view>, Clock contracts.Clock, Follow contracts.FollowClient,
//         Opener <media opener>, Source *source.Server, Subscriber *source.Subscriber,
//         ClockFollower <SetMaster-er>, Sink contracts.Sink, MediaDir string, Log *slog.Logger }
g.Start()
g.Close() error
// On Play (master): open media → run release ticker → server fans out to subs;
// own sink subscribes over loopback like everyone else. On any master/gen change
// (derivation): repoints Subscriber + clock Follower at the current master's
// SOURCE_PORT / STREAM_PORT (resolved via cluster.DialCandidates). On settings
// change: bumps gen, broadcasts RECONFIG, subscribers resubscribe (D23).

// ── I: HTTP API (Echo: REST + WS + proxy + SPA embed; feeds Observe) ────────
api.New(p api.Params) *api.Server
//   Params{ Store *cluster.Cluster, Group *group.Engine, Config *config.Config,
//           MediaDir string, Observe func(peer id.ID, ip string), DistFS fs.FS, Log *slog.Logger }
srv.FollowClient() contracts.FollowClient                // D16 — preferred over K's fallback
srv.Serve(ln net.Listener) error                         // blocks until Shutdown
srv.Shutdown(ctx context.Context) error
```

K invents no type beyond `options`, `shutdownStack`, and the `followClient`
fallback. Everything else is constructed-and-wired.

---

## 3. Control flow

### 3.1 Startup — strict dependency order (four port binds)

`run(ctx, opt)` builds the graph bottom-up. Every step that allocates an OS
resource or starts a goroutine pushes a closer onto the `shutdownStack` so
teardown is exactly reverse order. **Four** ports are bound (§2):

```
 0. logger   := slog at opt.LogLevel, .With("comp","main")
 1. cfg      := config.Load{DataDir,MediaDir,Name}        // node.json (A). FATAL on error (no fresh id over corrupt file, §4)
 2. PORT BINDS — bind-or-increment, capture the ACTUAL bound port for each:
      streamTCP, streamUDP, streamPort := netx.BindTCPUDP(host, opt.StreamPort, 64)   // §2; UDP handed to mux
      srcTCP,    srcUDP,    sourcePort  := netx.BindTCPUDP(host, opt.SourcePort, 64)   // §2 D22 — NEW fourth path
      httpLn,               httpPort    := netx.BindTCP   (host, opt.HTTPPort,   64)
      gossipPort                        := probeGossipPort(host, opt.GossipPort, 64)   // D8: probe TCP+UDP pair, CLOSE both, hand bare number to memberlist
    Each FATAL on error (port exhaustion §2). Nothing to unwind on a bind error
    except already-bound listeners (closed in the deferred cleanup before return).
 3. addrs    := netx.InterfaceCIDRs()                      // §3.1 (empty on loopback — fine)
 4. caps     := capabilities(opt)                          // D3/D32: PATH probe (exec backends/playback) + internal/dl dlopen probes
                                                          //   (libopus → codecs gains "opus"; libasound → backends gains "alsa"),
                                                          //   sources=[file,http,input], formats. Probe results feed BOTH this
                                                          //   record AND the sink-backend registry / audio package (see note below).
 5. mux      := stream.NewMux(streamUDP, log)              // owns STREAM_PORT UDP; not yet Run
 6. cluster  := cluster.New{Self:cfg.NodeID, Name:cfg.Name,
                  HTTPPort, StreamPort:streamPort, SourcePort:sourcePort, GossipPort:gossipPort,
                  Addrs:addrs, Capabilities:caps, log}      // advertises ACTUAL ports incl. SourcePort
 7. clockSrv := clock.NewServer(mux, log)                  // Register(0x10) on mux  (answers 0x11)
    clockFol := clock.NewFollower(mux, log)                // contracts.Clock; receives 0x11
 8. backend  := sink.PickBackend(opt.Output, log)          // "null" if ENSEMBLE_OUTPUT=null; auto-probes pw-play/… (D27)
    theSink  := sink.New{backend, clockFol, contracts.DefaultBufferMs, log}   // contracts.Sink
 9. srcSrv   := source.NewServer{Self:cfg.NodeID, UDP:srcUDP, TCP:srcTCP, log}  // master-side source server (D22), idle until a session runs
10. subClient:= stream.NewClient{Mux:mux,
                  Deliver:func(h stream.Header,pay []byte){ theSink.Push(h.Gen,h.Seq,h.PTS,pay) }, log}  // member-side; mux 0x01/0x02 (+FEC) + TCP
11. opener   := audio media factory over cfg.MediaDir      // scheme factory file/http/input (D26)
12. apiSrv   := api.New{Store:cluster, Group:<set after>, Config:cfg, MediaDir:cfg.MediaDir,
                  Observe:cluster.Observe, DistFS:web.DistFS, log}            // engine wired in step 13
    follow   := apiSrv.FollowClient()  (or K's &followClient{cluster,client}) // D16 prefer I's
13. engine   := group.New{Cluster:cluster, Clock:clockFol, Follow:follow,
                  Opener:opener, Source:srcSrv, Subscriber:subClient,
                  ClockFollower:clockFol, Sink:theSink, MediaDir:cfg.MediaDir, log}
    apiSrv.SetGroup(engine)                                // close the API↔group cycle (see note)
14. disc     := discovery.New{cfg.NodeID, HTTPPort, StreamPort:streamPort,
                  SourcePort:sourcePort, GossipPort:gossipPort, log}
15. SEED + ACTIVATE:
      cluster.Join(opt.Join)                               // D20 dev seeds (e2e); errors logged, non-fatal
      mux.Run()                                            // packets now flow on STREAM_PORT UDP
16. start goroutines (each pushes its Close on the stack BEFORE starting):
      clockFol.Run(ctx)        srcSrv.Run()        // subClient starts its own
      engine.Start()           disc.Run(ctx)       // loops inside Subscribe (H drives it)
      go discoveryJoinLoop(ctx, disc.Peers(), cluster)
      go apiSrv.Serve(httpLn)                             // blocks in its own goroutine; error → cancel ctx
    (clockSrv is passive — registered on the mux, no goroutine of its own;
     srcSrv.Run() is non-blocking and starts the read/accept/sweeper loops.)
17. log "ready": id, name, ports{http,stream,source,gossip}, output backend, playback cap
18. <-ctx.Done()  → graceful shutdown (§3.3)
```

**Capability assembly & probe wiring (step 4, D3/D32).** `capabilities(opt)`
assembles the node's `contracts.Capabilities` from three sources: the `$PATH`
scan for exec tools (playback backends, the `input` capture scheme), the static
format/scheme lists, and the `internal/dl` dlopen probes (D32). `dl.Open` is
tried once per optional library at startup — `libopus.so.0`/`libopus.so` and
`libasound.so.2`/`libasound.so` — dlsym-verifying every required symbol before
the capability is reported on; a missing/old library or symbol yields
`dl.ErrUnavailable` (soft, never a panic) and the capability stays off. The
probe results are wired in **two** places, not just the cluster record:

- **Cluster capability setter.** `caps` is handed to `cluster.New{…,
  Capabilities:caps,…}` (step 6), which advertises the record over gossip — so
  `codecs` gains `"opus"` exactly when libopus loaded and `backends` gains
  `"alsa"` exactly when libasound loaded.
- **Sink backend registry / audio package.** The same `dl` probe outcome gates
  what `sink.PickBackend` (step 8) can actually return: the `alsa` backend
  registers itself in the registry only when the libasound probe succeeded
  (D34, first in `auto` order), and the opus codec constructors in
  `internal/audio` (`audio.NewOpusEncoder`/`NewOpusDecoder`, D33) return
  `dl.ErrUnavailable` when libopus did not load. K does **not** re-run the
  probes per call: the registry/audio side and the advertised `caps` derive
  from the **same** startup `dl.Open` result, so a node never advertises a
  backend or codec it cannot construct.

**The API↔group cycle (steps 12–13).** `api.New` needs the engine (REST
`/play`,`/follow`,`/master`,`/stop`,`/group/settings` delegate to it); the
engine needs `FollowClient` from the API (D16). K breaks it exactly as S/D16
intend — `FollowClient` is a leaf interface in `contracts`:

- Preferred: build the API first **without** the engine; take
  `apiSrv.FollowClient()`; build the engine with it; `apiSrv.SetGroup(engine)`
  before `Serve`.
- Fallback (I exposes neither `SetGroup` nor `FollowClient`): inject K's
  `&followClient{store: cluster}` into the engine, build the engine first, then
  `api.New` with the engine. K's `followClient` depends only on the cluster, so
  no cycle. (§9 contract concern: I and H settle on one.)

**`discoveryJoinLoop`** (the one K-owned goroutine). Ranges `disc.Peers()`; for
each `Peer` it calls `cluster.Observe(peer.ID, peer.Addr.String())` (seeds the
§3.1 observation from the mDNS source IP — critical on loopback where
`InterfaceCIDRs` is empty) and `cluster.Join([peer.GossipAddrPort().String()])`.
memberlist dedups already-joined members; join errors log at debug and drop.

**Source/clock re-pointing is H's job, not K's.** K wires the subscriber and the
clock follower **into the engine's Deps**; the engine watches `cluster.Subscribe()`
derivation changes and calls `subClient.Subscribe(masterSourceAddr, gen, settings)`
and `clockFol.SetMaster(masterStreamAddr, gen)` whenever the elected master
endpoint or generation changes (D17 clock half; D22 subscribe half), resolving
addresses via `cluster.DialCandidates(master)`. K never drives master changes.

### 3.2 Steady state — goroutines & channels

| Goroutine                 | Owner | Blocks on / loops                                                   |
|---------------------------|-------|---------------------------------------------------------------------|
| mux read loop             | S/mux | `ReadFromUDPAddrPort`; dispatch by type (0x01/0x02 audio+FEC, 0x10/0x11 clock) |
| clock follower            | F     | 1 Hz ticker; UDP req/reply via mux; 5-best-of-30 median offset      |
| source server: UDP fan-out| G     | per released frame → datagram to each sub addr + XOR FEC every 4     |
| source server: TCP accept | G     | accept subscriber TCP conns on SOURCE_PORT; length-framed audio out  |
| source server: control    | G     | HELLO/BYE/RESTART on SOURCE_PORT (UDP datagrams / TCP frames); registry keepalive(5 s)/expiry(15 s); burst prime |
| subscriber receive (UDP)  | G     | mux 0x01/0x02 → reorder/FEC window → `Deliver` → `theSink.Push`      |
| subscriber receive (TCP)  | G     | dial master SOURCE_PORT; length-framed reads → `Deliver`             |
| subscriber control        | G     | HELLO keepalive (5 s); 2 s-starvation watchdog → RESTART; RECONFIG → resubscribe |
| sink playout + rate servo | E     | frame-cadence ticker; jitter pop; Catmull-Rom resample; backend write |
| group engine              | H     | `cluster.Subscribe()` → re-derive; self-heal/watchdog tickers; per-session release ticker (master); repoints subscriber+clock |
| discovery register/browse | B     | zeroconf; emits Peer                                                 |
| discoveryJoinLoop         | K     | ranges `Peers()` → `Observe` + `Join`                               |
| api Serve                 | I     | Echo http.Server; per-request handlers; WS pump (debounced cluster) |

The master's own sink is **not special**: the engine points `subClient` at the
master's own `SOURCE_PORT` over loopback, so the master subscribes and plays via
the identical path as remote members (D22). K wires exactly one subscriber and
one source server per node; which one is "active" is derived state owned by H.

Channels K introduces: the `disc.Peers()` consumer loop and the
`shutdownStack`. K holds **no mutex** — it builds, starts, waits. Each component
owns its own single mutex (S §3 convention).

### 3.3 Shutdown — reverse order, bounded

`main` installs `signal.NotifyContext(parent, SIGINT, SIGTERM)`. On the first
signal `ctx` cancels; `run` returns from `<-ctx.Done()` and unwinds the
`shutdownStack` with a **fresh** `shutdownCtx` (5 s deadline, not the cancelled
parent). A second signal makes `main` exit hard. LIFO order (reverse of
acquisition):

```
 1. disc.Close()           // stop advertising/browsing first (deregister mDNS)
 2. apiSrv.Shutdown(sc)    // stop accepting HTTP; drain in-flight; close WS
 3. engine.Close()         // stop any session: bump gen, BYE the local sub, RECONFIG/stop to subs, clear playback status
 4. subClient.Close()      // BYE to master source; stop receive loops
 5. srcSrv.Close()         // expire subscribers, close SOURCE_PORT TCP conns + listeners
 6. theSink.Close()        // stop playout loop + rate servo; Close() backend (exec: kill on hang, D21)
 7. clockFol.Close()       // stop 1 Hz loop   (clockSrv is passive; dies with the mux)
 8. cluster.Shutdown(sc)   // memberlist Leave (broadcast) then close — peers see us depart
 9. mux.Close()            // close STREAM_PORT UDP socket, join read goroutine
10. close remaining listeners K still owns: srcTCP, srcUDP, streamTCP, httpLn
```

Ordering rationale: **engine before subClient/srcSrv** so the master's stop
control (RECONFIG/stop, §8.6 D23) is sent while the source server still runs and
subscribers are still listening. **engine before cluster** so the master writes
`state:idle` (clears the playback record) *before* we Leave; followers then see
both the stop and our departure. **cluster Leave before mux.Close** so the
gossip socket is alive for the leave broadcast. Every `Close` error is logged,
never aborts the unwind. `run` returns the first non-nil shutdown error (or the
original cause from `ctx`).

### 3.4 Locking

None in K. The graph is built on the main goroutine before any worker starts
(`mux.Run()` and the `*.Run(ctx)` calls are step 15–16, after all
`Register`/`Deliver`/`SetGroup` wiring), so there is no concurrent access during
construction; workers only start after the graph is closed.

---

## 4. Edge cases & failure handling

- **Four-port exhaustion (§2, S §4, D8).** Any of `BindTCPUDP(stream)`,
  `BindTCPUDP(source)`, `BindTCP(http)`, or `probeGossipPort` returning an error
  is **fatal**: log the base+range, close any listeners already bound this call,
  exit non-zero before starting any goroutine. SOURCE_PORT (9200) is bound on
  **every** node, master or not (D22) — a node that never becomes master still
  holds an idle source listener; that is correct and cheap.
- **Gossip port handoff (D8).** K does **not** hand memberlist a live socket for
  gossip: `probeGossipPort` uses `netx.BindTCPUDP` to find a free TCP+UDP pair,
  **closes both**, and passes the bare number to `cluster.New` (memberlist binds
  it itself). The tiny rebind race is accepted for v1. STREAM and SOURCE stay
  bound-and-handed-over (mux keeps the STREAM UDP socket; the source server
  keeps both SOURCE sockets).
- **node.json unreadable / corrupt (§1, A).** `config.Load` error is fatal; do
  **not** generate a fresh ID over a corrupt file — that would duplicate
  identity on the network. Fail loudly.
- **`ENSEMBLE_OUTPUT=null` (K mandate, §8.5, D27).** `opt.Output=="null"` selects
  the null backend regardless of `$PATH` **and regardless of what the dlopen
  probe found** — forcing null keeps the e2e backends honest even on a host
  whose libasound probe succeeded (`alsa` may legitimately appear in
  `capabilities.backends`, but the sink is still null for tests). `capabilities(opt)`
  still probes `$PATH` and the optional libraries truthfully: with forced
  `null`, `playback=false`, but the receiver→sink path is independent of the
  backend, so a two-null cluster still **subscribes and "plays"** (frames
  written to the null sink advance `Stats().Played`). `auto` (default) picks
  alsa → exec → null (D27/D34); none usable → null + `playback=false`.
  `backends` in capabilities lists every name this host actually offers —
  `"alsa"` exactly when the libasound dlopen probe succeeded (D32/D34).
- **Source schemes in capabilities (D26).** `capabilities(opt).Sources` is the
  static `["file","http","input"]` (`input` only if an exec-capture tool is on
  `$PATH`, mirroring the playback probe). `play` of an `http://` URI needs no
  local file; `file:` URIs resolve under `MEDIA_DIR` with traversal rejected by
  the opener (D26).
- **No network interfaces (§3.1, S §4).** `InterfaceCIDRs()` empty is fine. On
  loopback dev2/e2e, CIDRs are empty (loopback skipped), so the two nodes find
  each other purely via (a) the `--join` seed (D20) and (b)
  `cluster.Observe(peer, "127.0.0.1")` fed from the discovery source address and
  from inbound HTTP/gossip traffic — `DialCandidates` then resolves
  `127.0.0.1:<port>` for proxy, clock, and source subscription. The stream path
  needs **no** resolution at all (the source streams back to the observed
  subscription addr, §3.1/§8.7).
- **mDNS unavailable (container, no multicast).** `discovery.New`/`Run` error or
  silence is **non-fatal**: warn and continue. e2e seeds an explicit `--join` so
  the smoke test never depends on multicast.
- **Subscriber re-point on master change (§7/§8.7, D17/D22).** Owned by H, not K.
  When this node is solo master, master == self, so H points `subClient` at the
  node's own SOURCE_PORT (loopback) and `clockFol.SetMaster(mux.LocalAddr(), gen)`.
  K only supplies the subscriber, the source server, the clock follower, and
  `cluster.DialCandidates` into the engine's Deps. (§9 contract concern: confirm
  H owns the repoint; else K runs a small loop off `cluster.Subscribe()`.)
- **Takeover needs HTTP to peers (§5.2).** `followClient` dials peers' HTTP ports
  via `cluster.DialCandidates(peer)` ∩ `httpPort` (§3.1). On loopback that is
  `127.0.0.1:<httpPort>`. No address resolves → `Follow` returns an error; the
  engine logs and the missing member self-heals (§5). Proxied follow requests
  carry `X-Ensemble-Proxied: 1` and are never re-proxied (§9.3).
- **Graceful-shutdown deadline.** 5 s. If `cluster.Shutdown`/`apiSrv.Shutdown`
  exceed it, `shutdownCtx` cancels them and `run` proceeds to `mux.Close()` and
  returns. A second SIGINT bypasses the wait entirely.
- **`run` returns error → exit 1.** `main` prints to stderr, `os.Exit(1)`. Clean
  shutdown → 0. Port-exhaustion / config errors → 1.

---

## 5. scripts/dev2.sh

`bash`, `set -euo pipefail`. Builds once, launches two nodes on **loopback** with
four disjoint port blocks and temp data dirs, both forced to the null backend,
prints their base URLs. Used by `make dev` and sourced by `e2e.sh`.

```sh
#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/ensemble"
go build -o "$BIN" ./cmd/ensemble            # web/dist placeholder is fine

# A tiny canonical WAV (48k stereo s16le tone) committed/generated for `play`.
[ -f "$ROOT/testdata/media/tone.wav" ] || "$ROOT/scripts/fixtures.sh"

DATA1="$(mktemp -d)"; DATA2="$(mktemp -d)"; DATA3="$(mktemp -d)"
LOG1="$(mktemp)"; LOG2="$(mktemp)"; LOG3="$(mktemp)"
export ENSEMBLE_OUTPUT=null                  # all nodes: null sink (§8.5, D27)
export ENSEMBLE_LOG="${ENSEMBLE_LOG:-info}"

# Four ports per node, distinct blocks:
#   N1: http 18080 stream 19090 source 19200 gossip 17946
#   N2: http 28080 stream 29090 source 29200 gossip 27946
#   N3: http 38080 stream 39090 source 39200 gossip 37946  (started later by e2e for late-join)
"$BIN" --data "$DATA1" --media "$ROOT/testdata/media" --name n1 --host 127.0.0.1 \
       --http-port 18080 --stream-port 19090 --source-port 19200 --gossip-port 17946 \
       >"$LOG1" 2>&1 &  PID1=$!
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" --name n2 --host 127.0.0.1 \
       --http-port 28080 --stream-port 29090 --source-port 29200 --gossip-port 27946 \
       --join 127.0.0.1:17946 >"$LOG2" 2>&1 &  PID2=$!

trap 'kill $PID1 $PID2 ${PID3:-} 2>/dev/null || true' EXIT INT TERM
echo "N1=http://127.0.0.1:18080  N2=http://127.0.0.1:28080"
echo "DATA1=$DATA1 DATA2=$DATA2 DATA3=$DATA3 LOG1=$LOG1 LOG2=$LOG2 LOG3=$LOG3"
echo "ROOT=$ROOT BIN=$BIN"
# Direct run: wait. Sourced by e2e (DEV2_WAIT=0): caller controls lifetime + starts N3.
[ "${DEV2_WAIT:-1}" = 1 ] && wait
```

Notes:
- Each node now binds **four** ports (`--source-port` added, D22/§2). N3's block
  is reserved here but **started by e2e.sh mid-play** (late-join test, §7).
- `--host 127.0.0.1` keeps everything on loopback. `--join` (D20) is an explicit
  gossip seed so the test never depends on mDNS multicast; discovery still runs.
- `testdata/media/tone.wav`: a few hundred ms of canonical 48k stereo s16le tone,
  generated by `scripts/fixtures.sh` (a `go run` helper in D's fixtures) if
  absent; long enough that a third node joining mid-play still has frames whose
  `pts + bufferMs` deadline is future (so the burst prime has something to send,
  D24). For a clean late-join assertion, e2e plays an `http://` live source or
  loops the tone — see §7 note.

## 6. scripts/e2e.sh — the smoke test

`bash`, `set -euo pipefail`. Needs `curl` and `jq`. Sources `dev2.sh` with
`DEV2_WAIT=0`, polls readiness, runs the assertions, tears down. Each assertion
exits non-zero with a clear message; success prints `e2e OK`. On any failure the
trap tails each node's log file for debuggability.

Helpers:

```sh
api()  { curl -fsS -H 'Accept: application/json' "$@"; }
post() { curl -fsS -X POST -H 'Content-Type: application/json' -d "${2:-}" "$1"; }
wait_for() {  # url jqfilter expected [timeout_s] — poll until jq filter == expected
  local url=$1 f=$2 want=$3 t=${4:-15} got
  for _ in $(seq $((t*4))); do
    got=$(api "$url" | jq -r "$f" 2>/dev/null || true)
    [ "$got" = "$want" ] && return 0
    sleep 0.25
  done
  echo "TIMEOUT: $url  $f != $want  (last=$got)" >&2; return 1
}
xor16() { python3 - "$1" "$2" <<'PY'
import sys
a=bytes.fromhex(sys.argv[1]); b=bytes.fromhex(sys.argv[2])
print(bytes(x^y for x,y in zip(a,b)).hex())
PY
}
```

The assertions (mirror the K mandate in IMPLEMENTATION.md exactly):

1. **Both up.** `wait_for $N1/api/status '.id|length' 32`; same for `$N2`.
   Capture `ID1=$(api $N1/api/status|jq -r .id)`, `ID2` likewise. Assert the
   status envelope (D19) exposes `.ports.source` (the new fourth port):
   `wait_for $N1/api/status '.ports.source>0' true`.

2. **Discovery + convergence.** `wait_for $N1/api/cluster
   '[.nodes[]|select(.alive)]|length' 2` and the symmetric check on `$N2`. Both
   nodes converge via the `--join` seed (and mDNS where multicast works). Assert
   each node advertises its SOURCE_PORT in the record:
   `wait_for $N1/api/cluster '[.nodes[]|select(.sourcePort>0)]|length' 2`.

2a. **Capabilities reflect host reality (D3/D32).** The reported codecs/backends
   are assembled from the `internal/dl` dlopen probes, so they must match what
   this host can actually load. Compute the host truth from `ldconfig` and
   assert the cluster record agrees on **both** nodes:
   ```sh
   HAS_OPUS=$(ldconfig -p 2>/dev/null | grep -q 'libopus\.so' && echo true || echo false)
   for N in "$N1" "$N2"; do
     HAS=$(api "$N/api/cluster" | jq -r --arg id "$(api "$N/api/status"|jq -r .id)" \
       '[.nodes[]|select(.id==$id).capabilities.codecs[]]|index("opus")!=null')
     [ "$HAS" = "$HAS_OPUS" ] || { echo "codecs/opus mismatch on $N: got $HAS want $HAS_OPUS"; exit 1; }
   done
   ```
   (Same shape applies to `backends`/`"alsa"` vs `libasound.so`; the opus check
   is the load-bearing one because assertion 11 conditions on it. `ENSEMBLE_OUTPUT=null`
   keeps the *backend* honest — `"alsa"` may still appear in `backends` from the
   probe, but the forced null sink is what actually runs, §8.5.)

3. **Follow forms a 2-node group with XOR id.**
   `post $N2/api/follow "{\"target\":\"$ID1\"}"`. Wait until N1's cluster shows
   one group of two mastered by `ID1`:
   `wait_for $N1/api/cluster '[.groups[]|select(.master=="'$ID1'")|.members|length]|max' 2`.
   Then assert the derived group ID **equals the 16-byte XOR** of the two node
   IDs (§5): `GID=$(xor16 "$ID1" "$ID2")`;
   `wait_for $N1/api/cluster '.groups[]|select(.master=="'$ID1'").id' "$GID"`.
   This is the load-bearing derivation check.

4. **Takeover moves mastership, group id unchanged.**
   `post $N1/api/group/master "{\"node\":\"$ID2\"}"` (issued on N1, the current
   master; §5.2 forwards as needed). Wait until the group with the **same** id
   reports `master==ID2` and still 2 members (§5.2 step 4 — XOR is unchanged):
   `wait_for $N1/api/cluster '.groups[]|select(.id=="'$GID'").master' "$ID2"` and
   `wait_for $N1/api/cluster '.groups[]|select(.id=="'$GID'")|.members|length' 2`.

5. **Play → BOTH sinks subscribe and play.**
   `post $N2/api/play "{\"file\":\"tone.wav\"}"` (N2 is now master; back-compat
   `{file}` ≡ `file:` URI, §9.1). For each of N1,N2:
   `wait_for $N/api/status '.sink.played>0' true` — poll until `played`
   (`contracts.SinkStats` under `.sink`, D19) is non-zero on **both** nodes,
   proving the master's own sink subscribed over loopback **and** the remote
   member subscribed over the network (D22). Then assert in-sync quality:
   `synced==true` on both and `lateDrop` a small fraction of `played`:
   `wait_for $N1/api/status '.sink.synced' true`;
   `api $N1/api/status | jq -e '(.sink.lateDrop // 0) <= (.sink.played/10)'`.
   The rate servo's `.sink.ratePPM` and `.sink.buffered` (D25) are present in the
   envelope; the test asserts `.sink.buffered >= 0` (sanity) but does not pin a
   ppm value (servo settles below audibility).

6. **Source stats on the master (D28).** N2 (master) runs the source; its
   `/api/status` carries `source:{clients,connects,restarts,primes}` (D19). With
   a 2-node group both members subscribe — including N2's **own** loopback
   subscription — so:
   `wait_for $N2/api/status '.source.clients' 2`  (group size, self included),
   `api $N2/api/status | jq -e '.source.connects >= 2'`,
   `api $N2/api/status | jq -e '.source.primes >= 1'`. The same numbers ride the
   replicated playback record (`Playback.Source`), so they are also readable from
   any node's `/api/cluster`:
   `api $N1/api/cluster | jq -e '.groups[]|select(.id=="'$GID'").playback.source.clients == 2'`.

7. **Late-join burst prime (D24).** Start N3 mid-play and make it follow N2:
   ```sh
   "$BIN" --data "$DATA3" --media "$ROOT/testdata/media" --name n3 --host 127.0.0.1 \
          --http-port 38080 --stream-port 39090 --source-port 39200 --gossip-port 37946 \
          --join 127.0.0.1:17946 >"$LOG3" 2>&1 &  PID3=$!
   wait_for http://127.0.0.1:38080/api/status '.id|length' 32
   ID3=$(api http://127.0.0.1:38080/api/status | jq -r .id)
   post http://127.0.0.1:38080/api/follow "{\"target\":\"$ID2\"}"
   ```
   N3 joins the group, its subscriber HELLOs the master's SOURCE_PORT with the
   prime-me flag, and the source **burst-primes** it from the ring. Assert N3
   reports `played>0` **quickly** (within a few seconds — the burst outruns the
   stream, ~4× realtime UDP, D24):
   `wait_for http://127.0.0.1:38080/api/status '.sink.played>0' true 6`.
   Master's source stats move: `wait_for $N2/api/status '.source.clients' 3` and
   `.source.connects >= 3`, `.source.primes >= 2`. Group id is now the XOR of
   three ids: `GID3=$(xor16 "$GID" "$ID3")`;
   `wait_for $N1/api/cluster '.groups[]|select(.master=="'$ID2'").id' "$GID3"`.

8. **RESTART recovery (§8.6, D25).** Simulate a lost subscriber by briefly
   pausing N3 (`kill -STOP $PID3; sleep 3; kill -CONT $PID3`) — frames cease > 2 s,
   N3's watchdog fires `Subscriber.Restart()`, sends RESTART to the source, and
   resumes from a fresh burst. Assert N3 keeps playing after resume (its `played`
   advances) and the master counts the re-prime:
   sample `P=$(api .../status|jq .sink.played)`, `sleep 1`,
   `api .../status | jq -e '.sink.played > '"$P"`; and
   `api $N2/api/status | jq -e '.source.restarts >= 1'`.
   (If `kill -STOP` is unavailable in the CI sandbox, the equivalent is dropping
   N3's UDP via a brief firewall rule; the assertion text is the same. Recorded
   as an environment caveat, not a contract concern.)

9. **Live settings change → resubscribe, playback continues (D23/§8.7).**
   Change the group's jitter buffer live:
   `post $N2/api/group/settings "{\"codec\":\"pcm\",\"transport\":\"udp\",\"bufferMs\":80}"`.
   The master bumps the generation, broadcasts RECONFIG; every subscriber
   re-reads the replicated settings and **resubscribes** under the new gen — no
   `play`/`stop`. Assert (a) the settings record updated everywhere:
   `wait_for $N1/api/cluster '.groups[]|select(.master=="'$ID2'").settings.bufferMs' 80`;
   (b) playback **did not stop**:
   `api $N1/api/cluster | jq -e '.groups[]|select(.master=="'$ID2'").playback.state == "playing"'`;
   (c) sinks keep advancing across the gen change (sample `played`, `sleep 1`,
   resample, assert strictly greater on N1 and N3). A brief `staleGen` bump on
   the sinks (old-gen frames dropped at the boundary) is allowed:
   `api $N1/api/status | jq -e '.sink.staleGen >= 0'`.

9a. **Conditional opus leg (D32/D33) — skipped cleanly when unavailable.** Opus
   is one universal binary's runtime-loaded codec, so the leg runs **only when
   BOTH nodes report it** (matching the host's `libopus.so` via the dlopen
   probe). It rides the live-settings machinery just proven in assertion 9: flip
   the group codec to opus, confirm both sinks still play, then reset.
   ```sh
   o1=$(api "$N1/api/cluster" | jq -r --arg id "$ID1" \
     '[.nodes[]|select(.id==$id).capabilities.codecs[]]|index("opus")!=null')
   o2=$(api "$N2/api/cluster" | jq -r --arg id "$ID2" \
     '[.nodes[]|select(.id==$id).capabilities.codecs[]]|index("opus")!=null')
   if [ "$o1" = true ] && [ "$o2" = true ]; then
     # master must accept opus only when every member can decode (D33).
     P1=$(api "$N1/api/status"|jq .sink.played); P2=$(api "$N2/api/status"|jq .sink.played)
     post $N2/api/group/settings "{\"codec\":\"opus\",\"transport\":\"udp\",\"bufferMs\":80}"
     wait_for $N1/api/cluster '.groups[]|select(.master=="'$ID2'").settings.codec' opus
     # both sinks keep advancing through the opus gen (master encodes once, each member decodes):
     sleep 1
     api "$N1/api/status" | jq -e '.sink.played > '"$P1"
     api "$N2/api/status" | jq -e '.sink.played > '"$P2"
     # reset codec back to pcm for the remaining legs:
     post $N2/api/group/settings "{\"codec\":\"pcm\",\"transport\":\"udp\",\"bufferMs\":80}"
     wait_for $N1/api/cluster '.groups[]|select(.master=="'$ID2'").settings.codec' pcm
   else
     echo "skip: opus leg — codecs opus not present on both nodes (n1=$o1 n2=$o2)"
   fi
   ```
   (Note N3 was paused/resumed in assertion 8 but remains a member; if it
   reports opus too the master fan-out includes it. The leg only asserts on
   N1/N2 to keep the skip condition a simple two-node `AND`. `ENSEMBLE_OUTPUT=null`
   still forces the null sink — opus decodes to canonical PCM that the null sink
   "plays", advancing `played`, §8.5.)

10. **Stop.** `post $N2/api/stop`. Wait until playback is idle on all nodes:
    `wait_for $N1/api/cluster '.groups[]|select(.id=="'$GID3'").playback.state' idle`.
    Then assert sinks stop advancing (within the watchdog window §8.6): sample
    `played`, `sleep 0.6`, resample, assert unchanged on N1/N2/N3. Master's
    `source.clients` drops as subscribers BYE:
    `wait_for $N2/api/status '.source.clients' 0 5`. Print `e2e OK`.

Teardown: `dev2.sh`'s trap kills PID1/PID2; e2e adds N3 (`PID3`) to the trap and,
on failure, tails `$LOG1 $LOG2 $LOG3`, then exits with the first failing
assertion's code.

**Status JSON the test depends on** (D19 envelope, §9.1): `.id`, `.name`,
`.role`, `.ports.{http,stream,source,gossip}`,
`.sink.{played,lateDrop,staleGen,synced,ratePPM,buffered}`, and — on the master
— `.source.{clients,connects,restarts,primes}`. From `/api/cluster`:
`.groups[].{id,master,members,settings.bufferMs,playback.state,playback.source.clients}`.
These are exactly S's `SinkStats`/`SourceStats`/`GroupView`/`Playback`/
`GroupSettings` shapes under the D19 envelope; if I names a key differently, the
jq filters in `e2e.sh` are the single place to update (§9 concern).

---

## 7. Test plan

`main.go` is wiring; its logic is tested at two altitudes — Go unit tests for the
pure helpers, the shell e2e for the whole graph. No mocks of the ~14 components:
the e2e **is** the integration test, run against real binaries on loopback with
null sinks.

Go unit tests (`cmd/ensemble/main_test.go`):
- `TestParseOptionsDefaults` — no args/env → spec defaults: http 8080, stream
  9090, **source 9200**, gossip 7946, `./data`, MediaDir=DataDir/media,
  output="auto".
- `TestParseOptionsSourcePort` — `--source-port`/`ENSEMBLE_SOURCE_PORT` parsed
  into `opt.SourcePort` (the new fourth port).
- `TestParseOptionsFlagsOverrideEnv` — flag beats env beats default for every
  port/dir.
- `TestParseOptionsJoinList` — `--join a:1,b:2` and `ENSEMBLE_JOIN` parse to
  `[]string{"a:1","b:2"}` (D20).
- `TestParseOptionsOutputNull` — `ENSEMBLE_OUTPUT=null` → `opt.Output=="null"`
  (env-only, D2; no flag).
- `TestParseOptionsBadPort` — non-numeric `--http-port` → parse error, no panic.
- `TestCapabilitiesNullForcesNoPlayback` — output=null → `caps.Playback==false`,
  `caps.Sources` contains `"file"` and `"http"`. (`caps.Codecs` is independent
  of the output backend — opus presence tracks the dlopen probe, D32/D33.)
- `TestCapabilitiesBackendsListed` — `caps.Backends` always contains `"null"`
  and `"file"`; `"exec"` only when a pipe tool is on `$PATH`; `"alsa"` exactly
  when the libasound dlopen probe (`internal/dl`) succeeds on this host
  (D27/D32/D34).
- `TestCapabilitiesOpusProbe` — `caps.Codecs` contains `"opus"` iff the
  libopus dlopen probe succeeds (`internal/dl`, D32/D33); `"pcm"` always
  present. One universal binary — asserted against host reality, not a build
  tag.
- `TestFollowClientPostsFollow` — `followClient.Follow` hits `/api/follow` with
  `{target}` JSON and the `X-Ensemble-Proxied` header, against an httptest server
  faking a peer (addr resolved via a fake `StateStore.DialCandidates`).
- `TestFollowClientUnfollow` — `Unfollow` hits `/api/unfollow` on the resolved peer.
- `TestFollowClientNoAddress` — peer with no resolvable addr → error, no panic.
- `TestShutdownStackLIFO` — push N named closers, `unwind`, assert reverse order
  and that all run even when one returns an error (first error is returned).
- `TestProbeGossipPortReleases` — `probeGossipPort` returns a number whose TCP
  **and** UDP are immediately re-bindable (proves D8 probe-release).
- `TestRunFatalOnPortExhaustion` — pre-bind the four base ports, `run` returns a
  non-nil error fast and starts no goroutine (assert via short timeout +
  goroutine-count delta).

Shell e2e (`scripts/e2e.sh`, via `make e2e` / CI):
- the §6 assertions are the test (the ten core legs plus the capability-reality
  check 2a and the conditional opus leg 9a); the script exits 0 only if all
  pass — 9a may *skip* (logged), which still passes. CI runs it on loopback with
  `ENSEMBLE_OUTPUT=null`, no audio hardware, no root, no real multicast
  (explicit `--join`); `null` keeps the backend honest while the dlopen probes
  report `alsa`/`opus` per host reality (D32).
- `make e2e`: `go build` → `scripts/e2e.sh`. Budget ~15 s (boot + convergence +
  follow + takeover + a short looped/live tone + late-join + settings change).

All Go unit tests run without network root, hardware, or multicast: `httptest`
for the follow client, in-process fakes for `StateStore`, real sockets only in
the deliberate `probeGossipPort` / port-exhaustion tests (loopback ephemeral).

---

## 8. Why this is the smallest thing that works

- One `main.go`, one `run` builder, one LIFO closer stack, one fallback type.
  No app framework, no lifecycle library, no DI container.
- K introduces **no** cross-piece interface; it consumes only what S pinned and
  what each piece exports. The only K-original code is option parsing, the
  `capabilities` probe (D3), the `followClient` fallback (~30 lines), the
  `shutdownStack`, and the two scripts.
- The subscribe model removes an entire concern from K: there is **no** endpoint
  resolver, **no** `SetEndpoints`, **no** master-dials-members. K binds the
  fourth port, constructs one source server + one subscriber per node, and hands
  both to the engine; H owns who subscribes to whom. The stream path needs no
  address resolution (source streams back to the observed subscription address,
  §3.1/§8.7).
- The e2e asserts behavior through the **public REST API only** (curl/jq) under
  the D19 envelope, so it is decoupled from internal types and survives refactors
  of any single piece.
- Everything runs on loopback with null sinks: no hardware, no root, no multicast
  dependency (explicit gossip seed, D20).

---

## 9. Contract concerns (for the integrator fix-loop)

G and H are both regenerated to the subscribe model (D22–D28); the remaining
issues are cross-piece **naming/shape** mismatches K must reconcile in §2.1
before wave-4 integration, not a stale push model:

1. **Subscriber-client package & constructor.** G-stream.md puts the member-side
   subscriber in **`internal/stream`** as `stream.NewClient(ClientConfig)` →
   `*stream.Client` (`Subscribe(sourceAddr, gen, Transport)`, `Unsubscribe`,
   `Counters`, `Close`); only the **source server** lives in `internal/source`
   (`source.NewServer`). §2.1 here names the subscriber `source.NewSubscriber`
   with `Subscribe(master, gen, contracts.GroupSettings)` and a
   `Deliver(gen,seq,pts,payload)` callback — G instead uses a `DeliverFunc(h
   Header, payload)` and a `Transport` (not `GroupSettings`) arg. **Action:
   align K's wiring to `stream.NewClient`/`stream.Client` and G's `DeliverFunc`
   signature (the deliver closure unpacks the Header into the sink Push args).**

2. **Source server config field names.** §2.1 uses
   `source.ServerConfig{SourceTCP, SourceUDP}`; G-stream.md uses
   `source.Config{Self id.ID, UDP, TCP, Log}` and requires `Self`. **Action:
   confirm the `source.Config` field set (Self/UDP/TCP) and that K passes the
   node id.**

3. **API↔group construction order (D16).** K prefers `apiSrv.FollowClient()` and
   `apiSrv.SetGroup(engine)`. If I exports neither, K uses its own `followClient`
   and passes the engine into `api.New`. **Action: I confirm it exports
   `FollowClient()` and `SetGroup(*group.Engine)` (or accept the engine in
   `Params` and expose `FollowClient`).**

4. **`/api/status` D19 envelope.** The e2e reads `.ports.source`,
   `.sink.{ratePPM,buffered}`, and `.source.{clients,connects,restarts,primes}`.
   **Action: I confirm the envelope nests `SinkStats` under `.sink`,
   `SourceStats` under `.source` (present only while a source runs), and exposes
   `.ports.source`** — otherwise the jq filters in `e2e.sh` are the single edit
   point.

5. **`--source-port` / `ENSEMBLE_SOURCE_PORT` flag (§2, D22).** A's config must
   parse the fourth port and `config.Options`/`cluster.Params`/`discovery.Params`
   must carry `SourcePort`. **Action: A and the Params structs add SourcePort.**

6. **Clock/subscriber re-point ownership (D17/D22).** K assumes H repoints both
   `subClient.Subscribe(master, gen, settings)` and `clockFol.SetMaster(master,
   gen)` off `cluster.Subscribe()`. **Action: confirm H owns this; else K runs a
   small repoint loop.**
