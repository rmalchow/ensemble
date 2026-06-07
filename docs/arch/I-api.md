# I — HTTP API (Echo)

Source of truth: [docs/README.md](../README.md) §9, §10. Shared contracts:
[S-skeleton.md](S-skeleton.md). This piece owns `internal/api/*` and the
`//go:embed web/dist` directive. It is **thin**: every route either reads the
`contracts.Snapshot` (cluster, C) or delegates a mutation to the cluster setters
(C) and the group engine (H). The API holds no domain state of its own beyond
the WebSocket hub and the embedded SPA.

Design rule for I: **no business logic**. Validation that is purely about the
HTTP envelope (JSON shape, path params, "is this node a master") lives here;
anything that mutates replicated state or audio sessions is a one-line call into
C or H. Pin the JSON wire shapes exactly (§9.1) — the SPA (J) codes against
them.

---

## 1. Package / file layout

Files I create and own (replacing the S stub `internal/api/api.go`):

```
internal/api/api.go          Server struct, New, Start/Shutdown, route table, deps wiring
internal/api/handlers.go     REST handlers for every §9.1 route (status, node, cluster, media, follow, unfollow, group/*, play, stop)
internal/api/dto.go          request/response JSON structs pinned to §9.1 (the wire contract)
internal/api/ws.go           WebSocket hub: per-conn writer, debounced cluster push, 5s heartbeat (§9.2)
internal/api/proxy.go        node-proxy middleware (§9.3): id-or-unique-name match, one-hop guard, reverse proxy via DialCandidates
internal/api/observe.go      Echo middleware feeding client remote IP to cluster.Observe (§3.1)
internal/api/follow_client.go FollowClient impl (contracts.FollowClient) the group engine (H) uses for takeover (§5.2)
internal/api/spa.go          //go:embed web/dist; SPA file server with index.html fallback + placeholder detection
internal/api/deps.go         the Cluster / Group dependency interfaces this package consumes (defined here, where consumed)

internal/api/api_test.go       Server construction, route registration, graceful shutdown
internal/api/handlers_test.go  every REST route over httptest: happy path + the pinned error shapes
internal/api/dto_test.go       golden JSON: marshalled DTOs match the spec byte-for-byte
internal/api/ws_test.go        WS upgrade, debounced push coalescing, 5s heartbeat, client disconnect cleanup
internal/api/proxy_test.go     id match, unique-name match, ambiguous-name 404, one-hop X-Ensemble-Proxied guard, dial-candidate failover
internal/api/observe_test.go   middleware calls Observe with the parsed client IP (X-Forwarded-For ignored; RemoteAddr only)
internal/api/spa_test.go       embed serves index for /, 404-less SPA fallback, placeholder detection
internal/api/follow_client_test.go  Follow/Unfollow issue the right POST against a stub peer server
```

`deps.go` is where I define the **consumer-side interfaces** (`Cluster`,
`Group`) — Go style, defined where consumed. C and H implement them with their
concrete `*cluster.Store` / `*group.Engine`; `main` (K) passes those in. This
keeps the API testable with small fakes and avoids importing C/H concretely
into test code.

---

## 2. Concrete Go API

### 2.1 `internal/api/deps.go` — what I consume from C and H

The skeleton's `contracts.StateStore` covers only the **read** side
(`Self`/`Snapshot`/`Subscribe`). The API also needs the cluster's write setters,
the address resolver for the proxy, and the `Observe` feed; and it needs the
group engine for every mutation that touches groups/playback. Those are not in
`internal/contracts` (and should not be — they are C-owned and H-owned surfaces,
not cross-cutting DTOs). I define them here as the minimal consumer interfaces.

```go
package api

import (
	"context"
	"net/netip"

	"ensemble/internal/contracts"
	"ensemble/internal/id"
)

// Cluster is the subset of the cluster store (piece C) the API depends on.
// C's concrete *cluster.Store satisfies this. Reads come from the embedded
// StateStore; the extra methods are C-owned writes + address resolution.
type Cluster interface {
	contracts.StateStore // Self() id.ID, Snapshot() contracts.Snapshot, Subscribe() <-chan struct{}

	// SetName renames THIS node (PATCH /api/node). Bumps version, broadcasts.
	SetName(name string)

	// Observe records that we received traffic from peer at ip (§3.1). Fed by
	// the observe middleware on every inbound HTTP request that carries a
	// known node id (proxied calls) and, more cheaply, by the cluster's own
	// gossip path. Idempotent, cheap, lock-internal.
	Observe(peer id.ID, ip netip.Addr)

	// DialCandidates returns HTTP dial targets ("host:port") for peer, ordered
	// best-first per §3.1 (self-reported CIDR ∩ cluster observations, most
	// recently observed first). Empty if the peer is unknown or undialable.
	// Used by the proxy to reach the target node's HTTP port.
	DialCandidates(peer id.ID) []string
}

// Group is the subset of the group engine (piece H) the API depends on. Every
// method is a mutation that the spec routes to "this node" or "the master".
// H's concrete *group.Engine satisfies this. All return a typed error the
// handler maps to an HTTP status + JSON error body (§2.4).
type Group interface {
	// Follow makes THIS node follow target (§5.1). ErrNotAlive / ErrNotMaster
	// / ErrUnknownNode on validation failure.
	Follow(ctx context.Context, target id.ID) error
	// Unfollow makes THIS node solo master (§5.1).
	Unfollow(ctx context.Context) error
	// MakeMaster runs takeover so node becomes master of its group (§5.2).
	// Must be called on a group member; forwards to the current master.
	MakeMaster(ctx context.Context, node id.ID) error
	// NameGroup sets a group's display name (LWW, any node, §4/§9.1).
	NameGroup(ctx context.Context, group id.ID, name string) error
	// Play starts playback of file on THIS node's group; master only.
	// ErrNotMaster (with takeover hint) if this node isn't its group's master.
	// ErrNoCodec if codec unsupported (§8.3). ErrMediaNotFound on bad file.
	Play(ctx context.Context, file string) error
	// Stop stops THIS node's group playback; master only.
	Stop(ctx context.Context) error
	// Settings returns this node's group's settings (GET /api/group/settings).
	Settings() contracts.GroupSettings
	// SetSettings updates this node's group's settings; master only (POST).
	SetSettings(ctx context.Context, s contracts.GroupSettings) error
}

// Media lists this node's local playable files (§6). Piece D or H owns the
// scanner; injected as a func so the API need not import it.
type Media interface {
	List() ([]MediaFile, error)
}

// SyncStat is the per-node clock/playout snapshot for GET /api/status (§9.1).
// Provided by a closure main (K) wires from the sink (E) + clock follower (F).
type SyncStat struct {
	Synced   bool   `json:"synced"`
	Played   uint64 `json:"played"`
	Silence  uint64 `json:"silence"`
	LateDrop uint64 `json:"lateDrop"`
	StaleGen uint64 `json:"staleGen"`
}
```

### 2.2 `internal/api/api.go` — server

```go
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Config bundles everything the API needs, wired by main (K).
type Config struct {
	Cluster  Cluster
	Group    Group
	Media    Media
	SyncStat func() SyncStat // closure over sink (E) + clock (F) stats
	Listener net.Listener    // HTTP listener from netx.BindTCP (K owns binding)
	Log      *slog.Logger
}

// Server is the Echo HTTP server: REST + WebSocket + proxy + SPA.
type Server struct {
	e    *echo.Echo
	cfg  Config
	hub  *wsHub
	log  *slog.Logger
}

// New builds the server, registers all routes/middleware, and starts the WS
// hub goroutine that fans cluster changes to connected clients. It does NOT
// begin accepting connections — call Start.
func New(cfg Config) *Server

// Start serves on cfg.Listener until Shutdown. Blocks; run in a goroutine.
// Returns http.ErrServerClosed on clean shutdown (caller treats as nil).
func (s *Server) Start() error

// Shutdown gracefully drains HTTP, closes all WebSocket connections, and stops
// the hub. Honors ctx deadline.
func (s *Server) Shutdown(ctx context.Context) error

// FollowClient returns a contracts.FollowClient bound to this server's cluster
// (for DialCandidates) so the group engine (H) can drive takeover (§5.2).
// main (K) wires it into H after constructing the server.
func (s *Server) FollowClient() contracts.FollowClient
```

Route registration in `New` (Echo group `/api`):

```go
// Order matters: proxy middleware runs FIRST on the /api group so a request
// for another node never reaches a local handler.
g := e.Group("/api")
g.Use(s.observeMiddleware) // §3.1 — record client IP under any proxied node id
g.Use(s.proxyMiddleware)   // §9.3 — short-circuit + reverse-proxy foreign-node calls

g.GET("/status", s.handleStatus)
g.PATCH("/node", s.handleRenameNode)
g.GET("/cluster", s.handleCluster)
g.GET("/media", s.handleMedia)
g.POST("/follow", s.handleFollow)
g.POST("/unfollow", s.handleUnfollow)
g.POST("/group/name", s.handleGroupName)
g.POST("/group/master", s.handleGroupMaster)
g.POST("/play", s.handlePlay)
g.POST("/stop", s.handleStop)
g.GET("/group/settings", s.handleGetSettings)
g.POST("/group/settings", s.handleSetSettings)
g.GET("/ws", s.handleWS) // §9.2 — upgraded; not proxied (handled below)

// SPA: everything not under /api. Registered on the root Echo, last.
e.GET("/*", s.handleSPA)
```

`/api/ws` and the proxy: the proxy middleware matches `/api/:seg/*` where `:seg`
is a node id-or-name; the literal routes above (`status`, `cluster`, `ws`, …)
are never node ids (not 32 hex, and reserved names), so they fall through to the
local handler. See §2.5 for the disambiguation rule.

### 2.3 `internal/api/dto.go` — pinned wire shapes (§9.1)

Request bodies and the non-snapshot responses. `GET /api/cluster` and the WS
`cluster` event serialize `contracts.Snapshot` **verbatim** (no wrapper DTO) so
J codes against the skeleton's JSON tags directly.

```go
package api

import "ensemble/internal/contracts"

// --- GET /api/status -------------------------------------------------------
type StatusResp struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	HTTPPort   int               `json:"httpPort"`
	StreamPort int               `json:"streamPort"`
	GossipPort int               `json:"gossipPort"`
	Role       string            `json:"role"`    // "master" | "follower"
	GroupID    string            `json:"groupId"` // derived group this node is in
	Sync       SyncStat          `json:"sync"`    // §2.1
}

// --- PATCH /api/node -------------------------------------------------------
type RenameReq struct {
	Name string `json:"name"`
}

// --- GET /api/media (§6) ---------------------------------------------------
type MediaFile struct {
	Path      string `json:"path"`      // relative to MEDIA_DIR
	Name      string `json:"name"`      // base name
	SizeBytes int64  `json:"sizeBytes"`
	ModTime   int64  `json:"modTime"`   // unix seconds
}
type MediaResp struct {
	Files []MediaFile `json:"files"`
}

// --- POST /api/follow ------------------------------------------------------
type FollowReq struct {
	Target string `json:"target"` // 32-hex node id
}

// --- POST /api/group/name --------------------------------------------------
type GroupNameReq struct {
	Group string `json:"group"` // 32-hex group id
	Name  string `json:"name"`
}

// --- POST /api/group/master (§5.2) -----------------------------------------
type MasterReq struct {
	Node string `json:"node"` // 32-hex node id to become master
}

// --- POST /api/play --------------------------------------------------------
type PlayReq struct {
	File string `json:"file"`           // relative media path
	Node string `json:"node,omitempty"` // optional: target node (proxy hint; see §3.5)
}

// --- group settings (§8.3/§8.4/§9.1) ---------------------------------------
// GET response and POST request are the same shape = contracts.GroupSettings.
// Re-exported as a named type only for handler clarity.
type SettingsBody = contracts.GroupSettings

// --- error envelope (every 4xx/5xx) ----------------------------------------
type ErrorResp struct {
	Error string `json:"error"`          // machine-stable short code, e.g. "not_master"
	Hint  string `json:"hint,omitempty"` // human hint (§9.1 "hint to use takeover")
}
```

`GET /api/cluster` response: the `Snapshot` value directly (`{"nodes":[…],
"groups":[…]}`) — no envelope. WS frames wrap it: `{"type":"cluster","data":{…
snapshot …}}` (§2.4 below).

### 2.4 `internal/api/ws.go` — WebSocket hub (§9.2)

```go
package api

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"

	"ensemble/internal/contracts"
)

// wsEvent is the envelope for every server→client WS message (§9.2).
type wsEvent struct {
	Type string             `json:"type"` // "cluster"
	Data contracts.Snapshot `json:"data"`
}

// debounceWindow / heartbeat per §9.2.
const (
	wsDebounce  = 250 * time.Millisecond
	wsHeartbeat = 5 * time.Second
	wsWriteWait = 10 * time.Second
	wsPongWait  = 30 * time.Second // client must answer pings within this
	wsPingEvery = wsPongWait * 8 / 10
)

// wsHub fans cluster snapshots out to all connected clients. One goroutine
// (run) owns the client set and the debounce timer; clients are added/removed
// over channels — no per-client mutex, no shared map under lock on the hot path.
type wsHub struct {
	cluster   Cluster
	log       *slog.Logger
	register   chan *wsClient
	unregister chan *wsClient
	done       chan struct{}
	wg         sync.WaitGroup
}

// wsClient is one upgraded connection. Its own goroutine owns all writes; the
// hub pushes snapshots into send (buffered; slow client → drop oldest, never
// block the hub).
type wsClient struct {
	conn *websocket.Conn
	send chan wsEvent
}

func newWSHub(c Cluster, log *slog.Logger) *wsHub
func (h *wsHub) run()                 // owns client set; selects on cluster.Subscribe(), register, unregister, ticks
func (h *wsHub) close()               // stop run, close all clients
func (h *wsHub) handleUpgrade(c echo.Context) error // upgrade, spawn read+write pumps, register
```

### 2.5 `internal/api/proxy.go` — node proxy (§9.3)

```go
package api

import "github.com/labstack/echo/v4"

// proxiedHeader marks a one-hop proxied request (§9.3).
const proxiedHeader = "X-Ensemble-Proxied"

// proxyMiddleware short-circuits requests whose first /api path segment is a
// 32-hex node id OR a unique node name, reverse-proxying them to that node's
// HTTP port (§9.3). All other requests pass through to local handlers.
func (s *Server) proxyMiddleware(next echo.HandlerFunc) echo.HandlerFunc

// resolveTarget maps the first path segment to a target node id. It returns
// (Zero,false) when seg is a reserved local route (status/cluster/ws/…) or not
// resolvable, so the request is handled locally. Ambiguous name → error sentinel.
func (s *Server) resolveTarget(seg string) (target id.ID, ok bool, ambiguous bool)
```

Resolution rule (decisive):

1. If `seg` is exactly 32 hex chars and parses → treat as a node id.
2. Else if `seg` equals a known node's `Name` and that name is **unique** across
   alive nodes → that node's id.
3. Else (reserved literal like `status`, non-unique name, unknown) → not a
   proxy target; fall through to local routing. A non-unique name returns
   **404** `{"error":"ambiguous_node"}` rather than guessing.

Self-target short-circuit: if the resolved id == `cluster.Self()`, **strip the
segment and handle locally** (no network hop). This makes
`GET /api/<selfId>/media` work uniformly from the UI.

One-hop guard: if the inbound request already carries `X-Ensemble-Proxied: 1`,
the middleware **never re-proxies** — it strips the node segment and routes
locally even if the segment is a foreign id (the previous hop already chose us;
re-proxying would loop). Outbound proxied requests always set the header to `1`.

Reverse proxy mechanics: build the upstream URL from
`cluster.DialCandidates(target)` (ordered best-first); rewrite the path to drop
`/api/<seg>` → `/api`; copy method, body, and headers (adding the proxied
header); try candidates in order until one connects; on total failure return
**502** `{"error":"unreachable"}`. Use `net/http/httputil.ReverseProxy` with a
custom `Director` per candidate, or a plain `http.Client` round-trip streamed
back (chosen: plain `http.Client` — simpler, lets us iterate candidates and
control the one-hop header without Director surprises). WebSocket upgrades
(`/api/<id>/ws`) are **not** proxied in v1 (the SPA connects to its own node);
a proxied `/ws` falls through and 404s — acceptable, out of UI scope.

### 2.6 `internal/api/observe.go` — client-IP feed (§3.1)

```go
package api

import "github.com/labstack/echo/v4"

// observeMiddleware records the client's remote IP into the cluster's
// observation map when the request is a proxied call FROM a known node (§3.1).
// A request carrying X-Ensemble-Proxied:1 came from a peer node; its RemoteAddr
// IP is a real, observed address for THAT peer. We learn the peer id from the
// already-resolved proxy segment of the ORIGINATING hop — but since we only see
// the final hop, we instead observe on every inbound request that names a node
// id we can attribute. Concretely: observe(parsedSegmentNodeId, remoteIP) when
// the request both names a node id in /api/<id>/… AND is the first hop (no
// proxied header). This feeds C the (peerId→ip) pairs §3.1 requires.
func (s *Server) observeMiddleware(next echo.HandlerFunc) echo.HandlerFunc
```

Decisive scope: the HTTP layer's observation contribution is **secondary** to
gossip (which observes far more peers). We only record what we can attribute
with certainty: a proxied request that arrives here with `X-Ensemble-Proxied:1`
came directly from a peer node socket, so `remoteIP` is a genuine observed
address of *that peer*. We learn the peer id from the proxied request's
`X-Ensemble-From: <hex>` header that the *outbound* proxy sets alongside the
one-hop header. So:

- Outbound proxy sets `X-Ensemble-Proxied:1` and `X-Ensemble-From:<selfId>`.
- `observeMiddleware`, on any request bearing those, calls
  `cluster.Observe(fromId, remoteIP)`.

This is the only HTTP-derived observation; it is correct (real peer socket, real
IP) and cheap. We never trust `X-Forwarded-For` (§3.1 trust model). RemoteAddr
only.

### 2.7 `internal/api/follow_client.go` — takeover client (§5.2)

```go
package api

import (
	"context"

	"ensemble/internal/id"
)

// followClient implements contracts.FollowClient. The group engine (H) calls it
// during takeover (§5.2) to drive POST /api/follow / /api/unfollow on peers.
// It dials peers directly (not through the proxy) using DialCandidates, setting
// X-Ensemble-Proxied:1 so the peer treats it as a terminal request.
type followClient struct {
	cluster Cluster
	http    *http.Client
}

func (f *followClient) Follow(ctx context.Context, peer, target id.ID) error
func (f *followClient) Unfollow(ctx context.Context, peer id.ID) error
```

### 2.8 `internal/api/spa.go` — embedded SPA (§10)

```go
package api

import "embed"

//go:embed all:web/dist
var distFS embed.FS

// handleSPA serves the embedded Svelte build for any non-/api path. Unknown
// paths fall back to index.html (client-side routing). If the embed is only the
// committed placeholder, the served index notes the UI isn't built yet.
func (s *Server) handleSPA(c echo.Context) error
```

Mechanics: `fs.Sub(distFS, "web/dist")`, wrap in `http.FileServer`. For a path
with no matching file and no extension, serve `index.html` (SPA fallback). A
real asset 404 (e.g. `/missing.js`) returns 404. Placeholder detection: read
`index.html` once at `New`; if it contains the sentinel comment
`<!-- ensemble-placeholder -->` (committed in the S placeholder), log a warning
once. The embed path is relative to the **api package directory**, so the
Makefile/K must ensure `web/dist` is reachable — but per S, the embed directive
lives in this piece and `web/dist/index.html` is committed at repo root; the
directive therefore uses a module-root-relative path. **See contract concern
#1** — the embed path placement.

---

## 3. Control flow

### 3.1 Startup (driven by main, K)

1. K binds the HTTP listener (`netx.BindTCP`) and constructs C (cluster), H
   (group), the media scanner, and the sync-stat closure.
2. K calls `api.New(Config{...})`. `New`:
   - creates the `echo.Echo`, sets `HideBanner`, a `slog`-backed error handler,
     and a recover middleware;
   - registers the `/api` group with `observeMiddleware` then `proxyMiddleware`,
     then all REST routes and `/api/ws`;
   - registers the root SPA catch-all last;
   - constructs the `wsHub` and starts its `run` goroutine.
3. K calls `server.FollowClient()` and injects it into H (so takeover can call
   back through HTTP). Then K wires H↔I cycle: I depends on H (`Group`), H
   depends on I only via the `contracts.FollowClient` it received — no Go import
   cycle (H imports `contracts`, not `api`).
4. K runs `server.Start()` in a goroutine.

### 3.2 Steady state — goroutines

- **Echo accept loop** (Echo-owned): one goroutine per connection via the std
  `http.Server`. Handlers are stateless; they read `cluster.Snapshot()` (deep
  copy, lock-free to the caller) or call a C/H mutation, then marshal a DTO.
- **wsHub.run** (one goroutine): owns the live client set (`map[*wsClient]bool`,
  no mutex — only this goroutine touches it). Selects on:
  - `cluster.Subscribe()` signal → arm/reset the 250 ms debounce timer;
  - debounce timer fire → take `cluster.Snapshot()`, build one `wsEvent`, fan it
    to every client's `send` channel (non-blocking; full channel → drop that
    client's oldest by draining one, per slow-client policy);
  - 5 s heartbeat ticker → same as a debounced push (snapshot carries playback
    position, satisfying "heartbeat with playback position");
  - `register` / `unregister` → mutate the set;
  - `done` → close all clients, return.
- **per-client write pump** (one goroutine per WS conn): ranges over `send`,
  writes each event with a `wsWriteWait` deadline; on the `wsPingEvery` ticker
  writes a ping; on any write error unregisters itself.
- **per-client read pump** (one goroutine per WS conn): the SPA sends nothing
  meaningful, but we must read to process pongs/close. Sets `wsPongWait` read
  deadline, refreshes it on pong; on read error unregisters and returns.

Single-mutex rule (S §3): the API's only shared mutable state is the WS client
set, and it is owned by **one goroutine** (`wsHub.run`) reached only via
channels — so the API holds **zero mutexes**. Everything else is request-scoped
or delegated to C/H (which hold their own single mutex each).

### 3.3 Shutdown

`server.Shutdown(ctx)`:
1. `hub.close()` → closes `done`; `run` closes every client `send` channel and
   sends a WS close frame; pumps exit; `wg.Wait()`.
2. `e.Shutdown(ctx)` → Echo stops accepting, drains in-flight requests to the
   deadline.
K orders this **before** tearing down C/H so in-flight handlers still have valid
deps.

---

## 4. Edge cases & failure handling

- **Non-master play/stop/settings (§9.1, §5.2)**: H returns `ErrNotMaster`; the
  handler maps it to **409 Conflict**
  `{"error":"not_master","hint":"use POST /api/group/master to take over first"}`.
  The SPA's "Play here" turns this into takeover+play (J), but the API itself
  does not auto-takeover on `/api/play` — explicit per spec.
- **Opus requested without support (§8.3)**: `SetSettings`/`Play` returns
  `ErrNoCodec` → **400** `{"error":"unsupported_codec"}`.
- **Follow target not alive / not a master / unknown (§5.1)**: H returns
  `ErrNotAlive`/`ErrNotMaster`/`ErrUnknownNode` → **409**/**404** with stable
  error codes (`not_alive`, `target_not_master`, `unknown_node`).
- **Path traversal in media/play (§6)**: the media scanner and H reject paths
  escaping `MEDIA_DIR`; a `..` path → **400** `{"error":"bad_path"}`. The API
  passes the raw relative path through; rejection is centralized in D/H, but the
  handler still rejects an absolute path or one containing `..` up front as a
  cheap guard.
- **Proxy: target unknown / undialable (§9.3)**: empty `DialCandidates` → **502**
  `{"error":"unreachable"}`. All candidates fail to connect → same.
- **Proxy: ambiguous name (§9.3)**: name matches >1 alive node → **404**
  `{"error":"ambiguous_node"}` (never silently pick one).
- **Proxy loop prevention (§9.3)**: inbound `X-Ensemble-Proxied:1` is treated as
  terminal; we strip the node segment and route locally, never re-dial. Tested
  explicitly (A→B→C must be impossible).
- **Self-proxy (§9.3)**: `/api/<selfId>/x` resolves to self → handled locally,
  no socket hop, no header set.
- **WS slow client (§9.2)**: `send` channel full → drop the oldest queued event
  (cluster snapshots are idempotent; latest wins), never block `run`. If the
  client stays full past one heartbeat, unregister it.
- **WS client gone (§9.2)**: read or write error → the offending pump
  unregisters its client exactly once (guard with `sync.Once`-style closed flag
  on `send`); `run` removes it from the set and closes the conn.
- **Malformed JSON body**: Echo bind error → **400** `{"error":"bad_request"}`.
- **Unknown route under /api (not a node id)**: falls through proxy → Echo 404
  with the JSON error shape (custom `HTTPErrorHandler` emits `ErrorResp`, never
  Echo's default HTML/text).
- **Rename to empty string (PATCH /api/node)**: reject **400**
  `{"error":"empty_name"}` (a node must keep a usable display name; §1).
- **SPA placeholder (§10)**: only the committed placeholder is embedded → still
  serves it (build succeeds per S); a `/api/*` call still works, so a freshly
  built binary is fully API-usable before the UI is built.
- **Body size**: cap request bodies (Echo `BodyLimit("1M")`) — defends the open
  LAN port without affecting any legitimate JSON request (§9 no large uploads).

---

## 5. Test plan

`internal/api/api_test.go`
- `TestNewRegistersAllRoutes` — every §9.1 path+method is in the router.
- `TestStartShutdownClean` — Start in goroutine, Shutdown returns nil, no leak.
- `TestErrorHandlerEmitsJSON` — a forced error yields `ErrorResp` JSON, not HTML.
- `TestBodyLimitRejectsOversize` — >1M body → 413.

`internal/api/handlers_test.go` (httptest + fake Cluster/Group)
- `TestStatusShape` — GET /api/status returns id/name/ports/role/groupId/sync.
- `TestStatusRoleMaster` / `TestStatusRoleFollower` — role derives from snapshot.
- `TestRenameNode` — PATCH /api/node calls Cluster.SetName; empty name → 400.
- `TestClusterReturnsSnapshotVerbatim` — body == json(Snapshot), no wrapper.
- `TestMediaList` — GET /api/media returns scanner output in MediaResp shape.
- `TestFollowOK` / `TestFollowUnknownNode` / `TestFollowTargetNotMaster` — codes.
- `TestUnfollow` — POST /api/unfollow calls Group.Unfollow.
- `TestGroupNameOK` / `TestGroupNameBadGroupID` — name set; bad hex → 400.
- `TestGroupMasterForwards` — POST /api/group/master calls Group.MakeMaster.
- `TestPlayOK` — master play succeeds.
- `TestPlayNonMaster409WithHint` — error=not_master + takeover hint present.
- `TestPlayBadPath` — `../x` → 400 bad_path.
- `TestStopOK` / `TestStopNonMaster` — stop routes; non-master 409.
- `TestGetSettings` / `TestSetSettingsMasterOnly` / `TestSetSettingsBadCodec`.

`internal/api/dto_test.go`
- `TestStatusRespJSONGolden` — field names/order match §9.1 exactly.
- `TestErrorRespOmitsEmptyHint` — `hint` omitted when empty.
- `TestSnapshotJSONTagsStable` — re-assert the contracts JSON tags the SPA uses.

`internal/api/ws_test.go` (httptest server + gorilla dialer)
- `TestWSUpgradeAndFirstEvent` — connect → receive a `cluster` event.
- `TestWSDebouncesBurst` — 10 rapid Subscribe signals → ≤2 events in 300 ms.
- `TestWSHeartbeat` — with no changes, an event arrives within ~5 s (shortened
  via injected interval in tests).
- `TestWSEventEnvelope` — frame is `{"type":"cluster","data":{nodes,groups}}`.
- `TestWSClientDisconnectCleanup` — close client → hub unregisters, no goroutine
  leak (goleak or count).
- `TestWSSlowClientDropsOldest` — fill send buffer → newest delivered, hub never
  blocks.

`internal/api/proxy_test.go` (two httptest servers wired by a fake Cluster)
- `TestProxyByNodeID` — /api/<id>/media on node1 returns node2's media.
- `TestProxyByUniqueName` — name resolves to id, proxies.
- `TestProxyAmbiguousName404` — duplicate name → 404 ambiguous_node.
- `TestProxyReservedRouteLocal` — /api/status is never treated as a node id.
- `TestProxySelfHandledLocally` — /api/<selfId>/status served without a hop.
- `TestProxyOneHopGuard` — request with X-Ensemble-Proxied:1 is not re-proxied.
- `TestProxySetsProxiedHeader` — outbound carries X-Ensemble-Proxied + From.
- `TestProxyDialFailover` — first candidate dead, second serves.
- `TestProxyUnreachable502` — no/failed candidates → 502 unreachable.
- `TestProxyStreamsBodyAndMethod` — POST body + method preserved through proxy.

`internal/api/observe_test.go`
- `TestObserveOnProxiedRequest` — proxied req w/ From header → Observe(from,ip).
- `TestObserveIgnoresXForwardedFor` — XFF header does not influence the IP.
- `TestObserveSkipsLocalNonProxied` — a plain local request observes nothing.

`internal/api/spa_test.go`
- `TestSPAServesIndexAtRoot` — GET / returns the embedded index.html.
- `TestSPAFallbackToIndex` — GET /groups (no file) → index.html (SPA routing).
- `TestSPAAssetMissing404` — GET /missing.js → 404 (not index).
- `TestSPAPlaceholderDetected` — placeholder sentinel logs a warning once.

`internal/api/follow_client_test.go` (stub peer httptest server)
- `TestFollowClientFollow` — issues POST /api/follow {target} to peer, proxied
  header set, target in body.
- `TestFollowClientUnfollow` — issues POST /api/unfollow to peer.
- `TestFollowClientDialFailover` — uses DialCandidates order; second wins.
- `TestFollowClientErrorPropagates` — peer 409 → typed error to caller.

All tests run over loopback httptest servers with in-process fake `Cluster` /
`Group` implementations; no real cluster, no audio, no multicast, no root.
