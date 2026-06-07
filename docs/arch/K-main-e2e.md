# K — main wiring & end-to-end

Source of truth: [docs/README.md](../README.md). Contracts: [S-skeleton.md](S-skeleton.md).
This piece owns process lifecycle (`cmd/ensemble/main.go`) and the two scripts
(`scripts/dev2.sh`, `scripts/e2e.sh`). It writes **no** package code under
`internal/` — it only constructs the components other pieces export, in
dependency order, and tears them down in reverse. If a constructor signature
here disagrees with what A/B/C/F/G/E/H/I actually ship, K is the integrator's
fix-loop; mismatches are recorded as contract concerns, not worked around with
new abstractions.

K is the only place that knows the full graph, so it is the only place allowed
to know every concrete constructor. Everything K calls already exists by wave 4.

---

## 1. Package / file layout

Files K creates and owns:

```
cmd/ensemble/main.go     flag/env parse → build component graph → run → signal → graceful shutdown
scripts/dev2.sh          launch two nodes, tmp DATA_DIRs, ENSEMBLE_OUTPUT=null, distinct ports
scripts/e2e.sh           curl/jq smoke test against the two nodes' REST API (the assertions §K)
```

`main.go` is deliberately a single file: flag parsing, a `run(ctx) error`
builder, and `main()` that wires signals to a context and maps the error to an
exit code. No `internal/app` package, no DI container — the graph is ~12 nodes,
built top-to-bottom once. Keep it under ~300 lines.

There is **one** non-obvious helper kept local to `main.go`: `followClient`, a
tiny `contracts.FollowClient` implementation (HTTP POST to a peer's
`/api/follow` / `/api/unfollow`, address resolved via the cluster). S says the
API piece (I) "injects a concrete implementation" of `FollowClient`; if I ships
it, K uses I's. K provides the fallback so the graph closes even if I does not
(see contract concerns). The e2e takeover path needs it to work either way.

---

## 2. Concrete Go API

`main.go` exports nothing (package `main`). The internal shape:

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
	"os"
	"os/signal"
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
	"ensemble/internal/netx"
	"ensemble/internal/sink"
	"ensemble/internal/stream"
)

// options is the fully-resolved configuration after flags+env (A owns the
// parse; K only declares the flags and hands the values to config.Load).
type options struct {
	HTTPPort   int    // --http-port  / ENSEMBLE_HTTP_PORT   (default 8080)
	StreamPort int    // --stream-port/ ENSEMBLE_STREAM_PORT (default 9090)
	GossipPort int    // --gossip-port/ ENSEMBLE_GOSSIP_PORT (default 7946)
	DataDir    string // --data       / ENSEMBLE_DATA_DIR    (default ./data)
	MediaDir   string // --media      / ENSEMBLE_MEDIA_DIR   (default DATA_DIR/media)
	Name       string // --name       (initial node name; applied on first start only)
	Output     string // ENSEMBLE_OUTPUT: "", "null", "auto" (default auto) — selects sink backend
	Host       string // bind host, default "" (all interfaces); "127.0.0.1" in dev2/e2e
	LogLevel   string // ENSEMBLE_LOG (debug|info|warn|error), default info
}

func main()                                   // parse → run(ctx) → exit code
func parseOptions(args []string) (options, error)
func run(ctx context.Context, opt options) error

// followClient is K's local contracts.FollowClient: HTTP POST /api/follow and
// /api/unfollow to a peer, dialing an address resolved from the cluster. Used
// only if the API piece does not export an equivalent (it should — §I).
type followClient struct {
	store  contracts.StateStore   // resolve peer http addr (§3.1)
	client *http.Client
	self   id.ID                  // for the X-Ensemble-Proxied guard
}

func (f *followClient) Follow(ctx context.Context, peer, target id.ID) error
func (f *followClient) Unfollow(ctx context.Context, peer id.ID) error
```

### 2.1 Assumed constructor surface (per piece)

These are the constructors K calls. They follow the spec and S directly; each
is named in IMPLEMENTATION.md as that piece's job. Exact spellings are the
integrator's to confirm — any divergence is a one-line edit in `run`.

```go
// A — config (§2). Loads/creates node.json under DataDir; applies Name only on
// first start; resolves MediaDir. Pure.
config.Load(opt config.Options) (*config.Config, error)
cfg.NodeID  id.ID
cfg.Name    string
cfg.DataDir string
cfg.MediaDir string
cfg.SetName(name string) error   // atomic rewrite of node.json on rename

// netx (S) — bind-or-increment, all-or-nothing per port.
netx.BindTCPUDP(host string, base, tries int) (*net.TCPListener, *net.UDPConn, int, error)
netx.BindTCP(host string, base, tries int) (*net.TCPListener, int, error)
netx.InterfaceCIDRs() []string

// stream (S) — UDP mux over the bound STREAM_PORT socket.
stream.NewMux(conn *net.UDPConn, log *slog.Logger) *stream.Mux

// C — cluster. Wraps memberlist on the bound gossip TCP+UDP; implements
// contracts.StateStore. K passes the resolved self-record fields.
cluster.New(p cluster.Params) (*cluster.Cluster, error)
//   Params{ Self id.ID, Name string, HTTPPort, StreamPort, GossipPort int,
//           Addrs []string, Capabilities contracts.Capabilities,
//           GossipTCP *net.TCPListener, GossipUDP *net.UDPConn, Log *slog.Logger }
cl.Observe(peer id.ID, ip string)          // fed by discovery + API (§3.1)
cl.Join(addrs []string) (int, error)       // join discovered peers
cl.Subscribe() <-chan struct{}
cl.Snapshot() contracts.Snapshot
cl.Self() id.ID
cl.SetFollowing(target id.ID)              // §5.1 setter (used by API/group)
cl.Shutdown() error                        // leave gracefully, then close

// B — discovery. Registers _ensemble._tcp, browses; emits peers on a channel.
discovery.New(p discovery.Params) (*discovery.Discovery, error)
//   Params{ Self id.ID, HTTPPort, StreamPort, GossipPort int, Log *slog.Logger }
d.Peers() <-chan discovery.Peer            // {ID, Addr, GossipPort, HTTPPort, StreamPort}
d.Run(ctx context.Context) error           // register + browse loop
d.Close() error

// F — clock. Server answers 0x10→0x11 on the mux; follower runs 1 Hz against
// the master endpoint, implements contracts.Clock.
clock.NewServer(mux *stream.Mux, log *slog.Logger) *clock.Server   // Register on mux
clock.NewFollower(mux *stream.Mux, log *slog.Logger) *clock.Follower
fol.SetMaster(addr netip.AddrPort, gen uint32)  // re-point on master/gen change
fol.MasterNow() (int64, bool)                    // contracts.Clock
fol.Run(ctx context.Context)
fol.Close() error

// E — sink. Backend auto-pick or null; jitter buffer + playout against a Clock.
sink.NewBackend(kind string, log *slog.Logger) (contracts.Backend, error) // "auto"|"null"
sink.New(b contracts.Backend, clk contracts.Clock, bufferMs int, log *slog.Logger) *sink.Sink
//   *sink.Sink implements contracts.Sink (Push/Reset/Stats/Close)

// G — transport. Receiver delivers (header,payload) to the sink; sender fans out.
stream.NewReceiver(mux *stream.Mux, tcpLn *net.TCPListener, log *slog.Logger) *stream.Receiver
rx.OnFrame(func(h stream.Header, payload []byte))   // wired to sink.Push
rx.Run(ctx context.Context)
rx.Close() error
stream.NewSender(mux *stream.Mux, log *slog.Logger) *stream.Sender  // group H drives it

// H — group engine. Derivation + follow/takeover + playback orchestration.
group.New(p group.Params) *group.Engine
//   Params{ Store *cluster.Cluster, Source audio.Opener, Sink contracts.Sink,
//           Sender *stream.Sender, Clock contracts.Clock, Follow contracts.FollowClient,
//           MediaDir string, Log *slog.Logger }
g.Run(ctx context.Context)                  // derivation loop + self-heal + watchdog
g.Close() error

// audio (D) — opener for the media source.
audio.NewOpener() audio.Opener              // Open(path) (FrameReader, error)

// I — HTTP API. Echo server over the bound HTTP listener; serves SPA + REST + WS.
api.New(p api.Params) *api.Server
//   Params{ Store *cluster.Cluster, Group *group.Engine, Config *config.Config,
//           MediaDir string, Observe func(id.ID, string), Log *slog.Logger }
srv.Handler() http.Handler                  // for httptest in unit tests
srv.FollowClient() contracts.FollowClient   // I's injected impl (preferred over K's)
srv.Serve(ln net.Listener) error            // blocks; returns on Shutdown
srv.Shutdown(ctx context.Context) error
```

K does not invent any type beyond `options` and the fallback `followClient`.
Everything else is constructed-and-wired.

---

## 3. Control flow

### 3.1 Startup — strict dependency order

`run(ctx, opt)` builds the graph bottom-up. Each step that allocates an OS
resource or starts a goroutine is registered on a LIFO `shutdown` stack
(a `[]func(context.Context) error`) so teardown is exactly reverse order.

```
 0. logger      := slog.New(handler at opt.LogLevel).With("comp","main")
 1. cfg         := config.Load(...)                 // node.json (A); FATAL on error
 2. bind ports  := netx.BindTCPUDP(stream) ; netx.BindTCPUDP(gossip) ; netx.BindTCP(http)
                   // each FATAL on error (port exhaustion §2). Capture ACTUAL ports.
 3. addrs       := netx.InterfaceCIDRs()
 4. caps        := capabilities(opt.Output)         // playback? codecs=[pcm] formats=[wav,mp3,flac]
 5. mux         := stream.NewMux(streamUDP, log)     // not yet Run
 6. cluster     := cluster.New(Params{... gossip sockets, actual ports, addrs, caps})
 7. clockSrv    := clock.NewServer(mux, log)         // Register(0x10) on mux
    clockFol    := clock.NewFollower(mux, log)
 8. backend     := sink.NewBackend(kind, log)        // "null" if ENSEMBLE_OUTPUT=null
    theSink     := sink.New(backend, clockFol, DefaultBufferMs, log)
 9. receiver    := stream.NewReceiver(mux, streamTCP, log)
    receiver.OnFrame(func(h, p){ theSink.Push(h.Gen,h.Seq,h.PTS,p) })
10. sender      := stream.NewSender(mux, log)
11. opener      := audio.NewOpener()
12. apiSrv      := api.New(Params{cluster, <group set later>, cfg, mediaDir, cluster.Observe, log})
    follow      := apiSrv.FollowClient() (or K's followClient fallback)
13. engine      := group.New(Params{cluster, opener, theSink, sender, clockFol, follow, mediaDir, log})
    apiSrv.SetGroup(engine)  // or pass engine into api.New if order allows (see note)
14. discovery   := discovery.New(Params{cfg.NodeID, httpPort, streamPort, gossipPort, log})
15. mux.Run()                                        // now packets flow
16. start goroutines under ctx:
      go clockSrv (passive; registered)              clockFol.Run(ctx)
      go receiver.Run(ctx)   go engine.Run(ctx)      go discovery.Run(ctx)
      go discoveryJoinLoop(ctx, discovery.Peers(), cluster)   // mDNS peer → cluster.Join + Observe
      go apiSrv.Serve(httpLn)                         // blocks in its own goroutine
17. log "ready" with id, name, http/stream/gossip ports
18. <-ctx.Done()  → graceful shutdown (§3.3)
```

**The API↔group cycle (steps 12–13).** `api.New` needs the group engine
(REST `/play`,`/follow`,`/master` delegate to it) and the group engine needs
`FollowClient` from the API. K breaks this the way S intends — `FollowClient`
is a leaf interface in `contracts`, so:

- Preferred: `api.New` takes the engine; engine takes `apiSrv.FollowClient()`.
  Construct API first **without** the engine (pass nil), build the engine with
  `apiSrv.FollowClient()`, then `apiSrv.SetGroup(engine)` before `Serve`.
- Fallback (if I exposes no `SetGroup` and no `FollowClient`): K's own
  `followClient{store: cluster}` is injected into the engine, and the engine is
  passed to `api.New`. This needs the engine built before the API, which is
  fine because K's `followClient` depends only on the cluster, not the API.

K codes the fallback (self-contained, always works) and uses I's injected
client when present. Recorded as a contract concern so I and H settle on one.

**`discoveryJoinLoop`.** One goroutine ranges `discovery.Peers()`; for each
`Peer` it calls `cluster.Observe(peer.ID, peer.Addr.IP)` (seeds §3.1
observations from the mDNS source address) and
`cluster.Join([peer.Addr:GossipPort])`. memberlist dedups already-joined
members; join errors are logged at debug and dropped (the peer may reappear).

### 3.2 Steady state — goroutines & channels

| Goroutine            | Owner | Blocks on / loops                                   |
|----------------------|-------|-----------------------------------------------------|
| mux read loop        | S/mux | `ReadFromUDPAddrPort`; dispatch by type             |
| clock follower       | F     | 1 Hz ticker; UDP req/reply via mux; offset median   |
| receiver UDP         | G     | mux delivers 0x01/0x02 → reorder/FEC → `OnFrame`    |
| receiver TCP         | G     | accept on streamTCP; length-framed reads → `OnFrame`|
| sink playout         | E     | ticker at frame cadence; pop jitter buf; backend    |
| group engine         | H     | cluster.Subscribe() events → re-derive; self-heal/watchdog tickers; playback ticker when master playing |
| discovery register/browse | B | zeroconf; emits Peer                            |
| discoveryJoinLoop    | K     | ranges Peers() → Observe + Join                      |
| api Serve            | I     | Echo http.Server; per-request handlers; WS pump     |

Channels K introduces: only the `discovery.Peers()` consumer loop and the
`shutdown` LIFO. K holds **no mutex** — it builds, starts, and waits. Each
component owns its own single mutex (S §3 locking convention).

### 3.3 Shutdown — reverse order, bounded

`main` installs `signal.NotifyContext(ctx, SIGINT, SIGTERM)`. On the first
signal, `ctx` cancels; `run` returns from `<-ctx.Done()` and unwinds the LIFO
`shutdown` stack with a fresh `shutdownCtx` (5 s deadline, **not** the cancelled
parent). On a second signal `main` exits hard (force-quit). Order:

```
1. discovery.Close()      // stop advertising/browsing first (deregister mDNS)
2. apiSrv.Shutdown(sc)    // stop accepting HTTP; drain in-flight; closes WS
3. engine.Close()         // stop any playback session: bump gen, send stop, clear status
4. sender.Close()         // close TCP conns to members
5. receiver.Close()       // stop accept + UDP delivery
6. theSink.Close()        // stop playout loop, Close() backend
7. clockFol.Close()       // stop 1 Hz loop  (clockSrv is passive, dies with mux)
8. cluster.Shutdown(sc)   // memberlist Leave (broadcast) then close — peers see us go
9. mux.Close()            // close UDP socket, join read goroutine
10. httpLn/streamTCP/gossip listeners closed by their owners; any K still holds → Close
```

Engine before cluster so the master broadcasts `state:idle` (clears playback
record) **before** we Leave; followers then see both the stop and our
departure. Cluster Leave before mux close so the gossip socket is still live
for the leave broadcast. Every `Close` error is logged, never aborts the
unwind. `run` returns the first non-nil shutdown error (or the original cause).

### 3.4 Locking

None in K. The graph is built on the main goroutine before any worker starts
(`mux.Run()` is step 15, after all `Register`/`OnFrame` wiring), so there is no
concurrent access during construction; workers only start in step 16.

---

## 4. Edge cases & failure handling

- **Port exhaustion (§2, S §4).** `netx.BindTCPUDP`/`BindTCP` returning an error
  is fatal: log the base+range, exit non-zero **before** starting any goroutine.
  Nothing to unwind (binds are step 2, first resource on the stack).
- **node.json unreadable / corrupt (§1, A).** `config.Load` error is fatal; the
  node has no identity. Do not generate a fresh ID over a corrupt file (would
  duplicate identity on the network) — fail loudly.
- **`ENSEMBLE_OUTPUT=null` (K mandate, §8.5).** `kind="null"` selects the null
  backend regardless of `$PATH`; capability `playback=false` is still reported
  *truthfully* by probing `$PATH` — but the running sink is null. Spec §1 ties
  `playback` to a backend being *found*; with forced null we report
  `playback=false` so the e2e two-null-node cluster shows no false playback
  capability yet both still receive frames (the receiver→sink path is
  independent of the backend kind). `kind="auto"` (default) probes pw-play/
  pw-cat/aplay/paplay; none found → fall back to null + `playback=false`.
- **No network interfaces (§3.1, S §4).** `InterfaceCIDRs()` empty is fine: the
  node advertises no self CIDRs and is reachable only via observed IPs. In
  dev2/e2e on loopback, CIDRs are empty (loopback skipped), so the two nodes
  **must** still find each other — they do, because mDNS on 127.0.0.1 +
  `cluster.Observe(peer, "127.0.0.1")` from the discovery source address seeds
  the observation, and the gossip join dials `127.0.0.1:gossipPort` directly
  from the discovered `Peer.Addr`. (Contract concern: discovery must surface the
  loopback address it saw; mDNS may not advertise 127.0.0.1 — see §6.)
- **mDNS unavailable (container, no multicast).** `discovery.New`/`Run` error or
  silence is **non-fatal**: log a warning and continue. The cluster still works
  if peers are joined some other way; in e2e we additionally seed an explicit
  join (see §5, `ENSEMBLE_JOIN`) so the smoke test does not depend on multicast.
- **Follower self-clock (§7).** The clock follower targets the *master's*
  STREAM_PORT UDP. When this node is solo master, master == self, so the
  follower dials `mux.LocalAddr()` (localhost). K passes the resolved master
  endpoint to `clockFol.SetMaster` — but K does **not** drive master changes;
  the group engine (H) owns "who is master" and calls `SetMaster` on the
  follower (and bumps gen). K only wires the follower into the engine's params.
  (Contract concern: confirm H owns `SetMaster`, else K runs a small re-point
  loop off `cluster.Subscribe()`.)
- **Takeover needs HTTP to peers (§5.2).** The `FollowClient` dials peers' HTTP
  ports using `cluster.Snapshot()` addresses ∩ observations (§3.1). On loopback
  e2e that is `127.0.0.1:httpPort`. If no address resolves, `Follow` returns an
  error; the engine logs and the missing member self-heals (§5).
- **Graceful-shutdown deadline.** 5 s. If `cluster.Shutdown`/`apiSrv.Shutdown`
  exceed it, the deadline `shutdownCtx` cancels them and `run` proceeds to
  `mux.Close()` and returns. A second SIGINT bypasses the wait entirely.
- **`run` returns error → exit 1.** `main` prints it to stderr and
  `os.Exit(1)`. Clean shutdown → exit 0. Port-exhaustion/config errors map to 1.

---

## 5. scripts/dev2.sh

POSIX `sh`, `set -eu`. Builds once, launches two nodes on **loopback** with
disjoint ports and temp data dirs, both forced to the null backend, and prints
their base URLs. Used by `make dev` and as the harness `e2e.sh` sources.

Behavior:

```sh
#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/ensemble"
go build -o "$BIN" ./cmd/ensemble          # build (web/dist placeholder is fine)

DATA1="$(mktemp -d)/n1"; DATA2="$(mktemp -d)/n2"
export ENSEMBLE_OUTPUT=null                # both nodes: null sink (§8.5)
export ENSEMBLE_LOG="${ENSEMBLE_LOG:-info}"

# Node 1: http 18080 / stream 19090 / gossip 17946
# Node 2: http 28080 / stream 29090 / gossip 27946
"$BIN" --data "$DATA1" --media "$ROOT/testdata/media" \
       --http-port 18080 --stream-port 19090 --gossip-port 17946 \
       --host 127.0.0.1 --name n1 &  PID1=$!
"$BIN" --data "$DATA2" --media "$ROOT/testdata/media" \
       --http-port 28080 --stream-port 29090 --gossip-port 27946 \
       --host 127.0.0.1 --name n2 \
       --join 127.0.0.1:17946 &        PID2=$!   # explicit join — no multicast reliance

trap 'kill "$PID1" "$PID2" 2>/dev/null || true' EXIT INT TERM
echo "N1=http://127.0.0.1:18080  N2=http://127.0.0.1:28080  (pids $PID1 $PID2)"
# When run directly: wait. When sourced by e2e.sh: caller controls lifetime.
[ "${DEV2_WAIT:-1}" = 1 ] && wait
```

Notes:
- `--host 127.0.0.1` keeps everything on loopback (no LAN noise; reproducible).
- `--join` / `ENSEMBLE_JOIN` is an explicit gossip seed so the test never
  depends on mDNS multicast (which is flaky in CI/containers). Discovery still
  runs; the seed is belt-and-suspenders. **Contract concern:** A/K need a
  `--join` flag (a comma-list of `host:gossipPort` passed to `cluster.Join` at
  startup). It is not in the spec's flag list (§2) but is required for a
  hermetic e2e on loopback. K adds it as a hidden/dev flag.
- `testdata/media` ships a tiny generated WAV (a few hundred ms of canonical
  48k stereo s16le tone) committed under the repo so `play` has a real file.
  Generated by a `go test`/`go run` helper in D's fixtures or a `make fixtures`
  target; K's script only references the path.

## 6. scripts/e2e.sh — the smoke test

POSIX `bash`, `set -euo pipefail`. Needs `curl` and `jq`. Sources `dev2.sh`
with `DEV2_WAIT=0` so it controls the node lifetime, polls readiness, runs the
assertions, and tears the nodes down. Each assertion exits non-zero with a
clear message on failure; success prints `e2e OK`.

Helpers:
```sh
api()   { curl -fsS -H 'Accept: application/json' "$@"; }
post()  { curl -fsS -X POST -H 'Content-Type: application/json' -d "$2" "$1"; }
wait_for() { # url jqfilter expected timeout — poll until jq filter == expected
  local url=$1 f=$2 want=$3 t=${4:-15} got
  for _ in $(seq "$((t*4))"); do
    got=$(api "$url" | jq -r "$f" 2>/dev/null || true)
    [ "$got" = "$want" ] && return 0
    sleep 0.25
  done
  echo "TIMEOUT: $url $f != $want (last=$got)" >&2; return 1
}
```

The assertions (mirrors the K mandate exactly):

1. **Both up.** `wait_for $N1/api/status '.id|length' 32` and same for `$N2`;
   capture `ID1=$(api $N1/api/status|jq -r .id)`, `ID2` likewise.

2. **They see each other.** `wait_for $N1/api/cluster '[.nodes[]|select(.alive)]|length' 2`
   and the symmetric check on `$N2`. Asserts gossip converged via the explicit
   join (and, when multicast works, via mDNS too).

3. **Follow forms a 2-node group with XOR id.**
   `post $N2/api/follow "{\"target\":\"$ID1\"}"`.
   Wait until N1's cluster shows one group of two members mastered by `ID1`:
   `wait_for $N1/api/cluster '[.groups[]|select(.master=="'$ID1'")|.members|length]|max' 2`.
   Then assert the group ID is the XOR of the two node IDs, computed in the
   shell from the hex (a 16-byte XOR in `awk`/`python3 -c`), and compared to
   `.groups[]|select(.master==ID1).id`. This is the load-bearing derivation
   check (§5).

4. **Takeover moves mastership.**
   `post $N1/api/group/master "{\"node\":\"$ID2\"}"` (issued on N1, the current
   master; §5.2 forwards as needed). Wait until the group with the **same id**
   now reports `master==ID2` and still has 2 members:
   `wait_for $N1/api/cluster '.groups[]|select(.id=="'$GID'").master' "$ID2"`.
   Group id unchanged (§5.2 step 4) is asserted by keying on `$GID`.

5. **Play makes BOTH null sinks receive frames, small playout error.**
   `post $N2/api/play "{\"file\":\"tone.wav\"}"` (N2 is now master).
   For each of N1,N2: `wait_for $N/api/status '.sink.played>0' true` — i.e. poll
   until `played` (from `contracts.SinkStats`, surfaced under
   `status.sink.played`) is non-zero on **both** nodes. Then assert the playout
   is "small": `lateDrop` is a small fraction of `played`
   (`(.sink.lateDrop // 0) <= .sink.played/10`) and `synced==true` on both.
   "Small playout error" = mostly-played, few late drops, clock synced — the
   observable proxy for in-sync playout the spec gives us via §9.1 stats.

6. **Stop works.** `post $N2/api/stop ""`. Wait until the group playback state
   is idle on both nodes: `wait_for $N1/api/cluster '.groups[]|select(.id=="'$GID'").playback.state' idle`.
   Optionally assert the sinks stop advancing: sample `played`, sleep 0.5 s,
   resample, assert unchanged (within the watchdog window, §8.6).

Teardown: the `trap` from `dev2.sh` kills both PIDs; `e2e.sh` adds its own trap
to print captured node logs on failure (`tail` of each node's stderr file) for
debuggability, then exits with the first failing assertion's code.

**Status JSON shape the test depends on** (§9.1 `GET /api/status`): the test
reads `.id`, `.name`, `.role`, `.sink.played`, `.sink.lateDrop`,
`.sink.synced`. S defines `SinkStats{Played,Silence,LateDrop,StaleGen,Synced}`
but not the `/api/status` envelope. **Contract concern:** the API piece (I)
must nest `SinkStats` under a stable key (proposed `"sink"`) in `/api/status`,
with `played`/`lateDrop`/`synced` JSON tags as in `SinkStats`. If I names it
differently, e2e.sh's jq filters are the single place to update.

---

## 7. Test plan

`main.go` is wiring; its logic is tested at two altitudes — Go unit tests for
the pure helpers, and the shell e2e for the whole graph. No mocks of the 12
components: the e2e *is* the integration test, run against real binaries.

Go unit tests (`cmd/ensemble/main_test.go`):
- `TestParseOptionsDefaults` — no args/env → spec defaults (8080/9090/7946, ./data, MediaDir=DataDir/media, output=auto).
- `TestParseOptionsFlagsOverrideEnv` — flag beats env beats default for each port/dir.
- `TestParseOptionsEnvFallback` — `ENSEMBLE_*` honored when flag absent.
- `TestParseOptionsOutputNull` — `ENSEMBLE_OUTPUT=null` → opt.Output=="null".
- `TestParseOptionsBadPort` — non-numeric `--http-port` → parse error (no panic).
- `TestCapabilitiesNullForcesNoPlayback` — output=null → caps.Playback==false, codecs==["pcm"].
- `TestFollowClientPostsFollow` — followClient.Follow hits `/api/follow` with `{target}` JSON and the proxied header, against an httptest server faking a peer (resolved via a fake StateStore).
- `TestFollowClientUnfollow` — Unfollow hits `/api/unfollow` on the resolved peer.
- `TestFollowClientNoAddress` — peer with no resolvable addr → error, no panic.
- `TestShutdownStackLIFO` — push N closers, run unwind, assert reverse order (the `shutdown []func` mechanism extracted to a tiny testable helper).
- `TestRunFatalOnPortExhaustion` — pre-bind the base ports, `run` returns a non-nil error fast and starts no goroutine (assert via a short timeout + goroutine count delta).

Shell e2e (`scripts/e2e.sh`, invoked by `make e2e` / CI):
- the six assertions in §6 are themselves the test; the script exits 0 only if
  all pass. CI runs it on loopback with `ENSEMBLE_OUTPUT=null`, no audio
  hardware, no root, no real multicast (explicit `--join`).
- `make e2e` target: `go build` → `scripts/e2e.sh`. Fast (< ~10 s): node boot +
  gossip convergence + one short tone.

All Go unit tests run without network root, hardware, or multicast: `httptest`
for the follow client, in-process fakes for `StateStore`, no real sockets bound
except the deliberate port-exhaustion test (loopback ephemeral pre-bind).

---

## 8. Why this is the smallest thing that works

- One `main.go`, one builder func, one fallback type. No app framework, no
  lifecycle library, no DI — a LIFO closure stack is the whole "container".
- K introduces **no** new cross-piece interface; it consumes only what S pinned
  and what each piece already exports. The only K-original code is option
  parsing (trivial), the `followClient` fallback (a 30-line HTTP POST), and the
  two scripts.
- The e2e asserts behavior through the **public REST API only** (curl/jq), so it
  is decoupled from internal types and survives refactors of any single piece.
- Everything runs on loopback with null sinks: no hardware, no root, no
  multicast dependency (explicit gossip seed).
