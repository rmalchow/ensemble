# J — Svelte SPA

Source of truth: [docs/README.md](../README.md) §9, §10. Shared contracts:
[S-skeleton.md](./S-skeleton.md) (the `Snapshot`/`NodeView`/`GroupView` JSON
shape). This piece owns **everything under `web/`** and nothing else.

Design rule for J: **the SPA is a thin renderer of one JSON document.** After
load it never polls; it subscribes to `/api/ws`, holds the latest `cluster`
snapshot in one store, and renders three sections from it. Every user action is
a single REST call (optionally proxied) that mutates server state; the resulting
state change comes back over the websocket and re-renders the page. No optimistic
UI, no client-side group derivation, no local cache beyond "last snapshot seen".

Stack: **Svelte 5 (runes) + Vite + plain JavaScript** (no TypeScript, no
component library, no router — one page). Hand-written dark CSS. Built to
`web/dist` with **relative** asset paths so `//go:embed web/dist` + Echo static
serving works under `/`.

---

## 1. Package / file layout

Files **J creates and owns** (everything under `web/`; `web/dist` is generated):

```
web/package.json              deps (svelte, vite, @sveltejs/vite-plugin-svelte), scripts
web/vite.config.js            base:'./', outDir '../web/dist'→ actually 'dist', relative assets, emptyOutDir
web/jsconfig.json             editor hints (svelte, $lib-free); not load-bearing
web/index.html                Vite entry; <div id="app">; loads src/main.js
web/.gitignore                node_modules, dist (placeholder handled by repo .gitignore)

web/src/main.js               mounts <App> onto #app (Svelte 5 mount())
web/src/App.svelte            shell: header (self status + ws state) + 3 sections; owns nothing but layout
web/src/app.css               global dark theme, layout primitives, card/button/badge styles

web/src/lib/ws.js             cluster store: websocket w/ auto-reconnect; exports reactive snapshot + connection state
web/src/lib/api.js            REST helpers + proxy-aware fetch; one function per §9.1 action
web/src/lib/fmt.js            pure formatters: shortId, relTime, position mm:ss, bytes, cidrList
web/src/lib/derive.js         pure view-selectors over a snapshot (selfNode, nodeById, isMaster, memberNames…)

web/src/sections/Groups.svelte   Groups section: list of GroupCard
web/src/sections/Nodes.svelte    Nodes section: table of NodeRow
web/src/sections/Media.svelte    Media section: node picker → file list → Play here

web/src/components/GroupCard.svelte   one derived group: name, members, playback, actions
web/src/components/MemberRow.svelte   one member inside a card: name, master badge, make-master, leave
web/src/components/JoinDropdown.svelte a member's "Join group…" select → POST /api/follow
web/src/components/NodeRow.svelte     one node in the Nodes table: editable name, id, addrs, caps, liveness
web/src/components/PlaybackBar.svelte playback state + position + stop button for a group
web/src/components/EditableText.svelte click-to-rename inline text editor (used by group + node names)
web/src/components/Toast.svelte       transient error/success banner (action failures surface here)
```

Component count is deliberately small: three sections, one card, a handful of
leaf components, two stores-ish modules (`ws.js`, `api.js`) and three pure
helper modules. No store framework beyond Svelte 5 runes (`$state`, `$derived`,
`$effect`).

---

## 2. Data model the SPA renders

The SPA renders **exactly** the `contracts.Snapshot` JSON from S §2.5, received
two ways:

- `GET /api/cluster` → `Snapshot` (used once, as the initial paint before the
  first WS frame arrives, so the page is not blank if WS connect lags).
- `GET /api/ws` → frames `{type:"cluster", data:<Snapshot>}` (steady state) and
  `{type:"cluster", data:<Snapshot>}` heartbeats every 5 s (§9.2). The SPA
  treats every `cluster` frame identically: replace the held snapshot.

The shapes J codes against (verbatim from S, JSON field names are the contract):

```jsonc
// Snapshot
{
  "nodes": [ NodeView, … ],
  "groups": [ GroupView, … ]
}

// NodeView
{
  "id": "ab12…(32 hex)",
  "name": "kitchen",
  "addrs": ["192.168.1.17/24", "fd00::5/64"],
  "httpPort": 8080, "streamPort": 9090, "gossipPort": 7946,
  "capabilities": { "playback": true, "codecs": ["pcm"], "formats": ["wav","mp3","flac"] },
  "following": "0000…(32 hex zero == solo master)",
  "observed": { "<peerId>": { "ip": "192.168.1.9", "lastSeen": 1733570000 } },
  "alive": true, "lastSeen": 1733570000, "stale": false,
  "updatedAt": 1733570000, "version": 42
}

// GroupView
{
  "id": "<32 hex; == XOR of members>",
  "name": "downstairs",            // "" if unnamed
  "master": "<nodeId>",
  "members": ["<nodeId>", …],
  "playback": { "state":"playing", "file":"jazz.flac", "startedAt":…, "positionSec":12.4,
                "codec":"pcm", "transport":"udp" },
  "settings": { "codec":"pcm", "transport":"udp", "bufferMs":150 }
}
```

Important consequences for J:
- `following`, `master`, `id`, member entries are **32-hex strings** (S marshals
  `id.ID` via `MarshalText`). The all-zero id `"00000000000000000000000000000000"`
  means "solo / no group / unfollowed"; J has one constant `ZERO_ID` for it.
- `nodes` is the full node list; `groups` is derived server-side. **J never
  derives groups.** It joins them only for display (e.g. resolve a member id to
  its `NodeView` for the name/liveness badge).
- Media is **not** in the snapshot. The Media section fetches
  `/api/media` (optionally proxied) on demand.
- Self id is **not** explicitly in the snapshot. J learns it once from
  `GET /api/status` (which returns this node's id/name/role) and stores it; used
  to mark "this node" and to default the Media picker. `GET /api/status` is
  called once on boot alongside the initial `/api/cluster`.

---

## 3. Concrete module APIs (the JS contracts)

### 3.1 `web/src/lib/ws.js` — cluster store + connection state

A single shared module-level store using Svelte 5 runes in a `.svelte.js` is
overkill; instead `ws.js` exposes a tiny reactive object backed by
`$state` declared in a `.svelte.js` file. To keep runes usable in a plain module
this file is named **`ws.svelte.js`** (Vite/Svelte requires the `.svelte.js`
suffix for runes outside components).

```js
// web/src/lib/ws.svelte.js

// Reactive singletons (module-scoped $state). Components import and read directly.
export const cluster = $state({
  snapshot: { nodes: [], groups: [] }, // latest Snapshot; empty until first frame
  status:   "connecting",              // "connecting" | "open" | "reconnecting" | "closed"
  lastError: "",                       // last ws/onerror message, for the header
  receivedAt: 0,                       // ms epoch of last cluster frame (staleness hint)
});

// connect() opens the websocket and wires auto-reconnect with capped backoff.
// Idempotent: a second call is a no-op while a socket is live.
// Backoff: 0.5s → 1s → 2s → 4s → max 8s, reset on a clean open.
export function connect(): void

// disconnect() closes the socket and stops reconnect (used on hot-reload teardown).
export function disconnect(): void

// wsURL() derives ws(s)://<host>/api/ws from window.location (scheme follows page).
function wsURL(): string
```

Behavior:
- On `connect()`: open `wsURL()`. `onopen` → `status="open"`, reset backoff.
  `onmessage` → JSON.parse; if `msg.type === "cluster"` set
  `cluster.snapshot = msg.data; cluster.receivedAt = Date.now()`. Unknown types
  ignored (forward-compat). `onerror` → record `lastError`. `onclose` →
  `status="reconnecting"`, schedule reconnect with backoff.
- `connect()` is called once from `App.svelte`'s top-level effect on mount, after
  a best-effort initial `GET /api/cluster` + `GET /api/status` seed.

### 3.2 `web/src/lib/api.js` — REST helpers (one per §9.1 action)

All actions are plain `fetch` against `/api/...`. **Proxy awareness** (§9.3):
any call that targets *another node* is issued against
`/api/<nodeId>/<rest>` — the server proxies it. A helper builds the prefix.

```js
// web/src/lib/api.js

// base(nodeId?) → "/api" for local, "/api/<nodeId>" for a proxied target.
// nodeId may be "" / undefined / self id (treated as local, no proxy hop).
export function base(nodeId): string

// --- low level ---
// req(method, path, body?) → parsed JSON or throws ApiError{status,message}.
// On non-2xx it reads {error} or {message} from the body for the toast.
async function req(method, path, body): Promise<any>

// --- self / cluster (local only) ---
export async function getStatus():  Promise<Status>          // GET  /api/status (once, for self id)
export async function getCluster(): Promise<Snapshot>        // GET  /api/cluster (initial seed)

// --- node actions ---
export async function renameNode(nodeId, name): Promise<void>     // PATCH /api/<nodeId>/node {name}
//   nodeId may be remote → proxied; spec §10 "works for remote nodes via proxy".

// --- group membership (issued ON the acting node) ---
export async function follow(nodeId, targetId): Promise<void>     // POST /api/<nodeId>/follow {target}
export async function unfollow(nodeId): Promise<void>             // POST /api/<nodeId>/unfollow
export async function makeMaster(memberId, newMasterId): Promise<void>
//   POST /api/<memberId>/group/master {node:newMasterId}; any member may receive it (§5.2).

// --- group naming ---
export async function renameGroup(groupId, name): Promise<void>   // POST /api/group/name {group,name}
//   group-name writes are LWW from any node (§4); issued locally (no proxy needed).

// --- media + playback ---
export async function getMedia(nodeId): Promise<MediaFile[]>      // GET  /api/<nodeId>/media
export async function play(nodeId, file): Promise<void>           // POST /api/<nodeId>/play {file}
//   "Play here": targets nodeId's group; server does implicit takeover if nodeId
//   is a follower (§5.2 / §10 Media). See §5 below for the takeover-then-play flow.
export async function stop(masterId): Promise<void>               // POST /api/<masterId>/stop

// --- group settings (master only for POST; optional UI, see §6) ---
export async function setGroupSettings(masterId, settings): Promise<void> // POST /api/<masterId>/group/settings

// ApiError carries the server's message for the Toast.
export class ApiError extends Error { status; }
```

Notes:
- `Status` (from `GET /api/status`, §9.1) is `{id, name, ports, role, group,
  sync, playout}`. J uses only `id` (self marker) and `name`/`role` for the
  header. The exact stat fields are I's to define; J reads them defensively.
- `MediaFile` is `{path, name, sizeBytes, modTime}` (§6). `path` is relative to
  the node's `MEDIA_DIR` and is what `play` sends back.

### 3.3 `web/src/lib/derive.js` — pure selectors over a snapshot

No state; pure functions used by sections/components so the same logic isn't
re-inlined. Keeps components dumb.

```js
// web/src/lib/derive.js
export const ZERO_ID = "00000000000000000000000000000000";

export function nodeById(snapshot, id)        // → NodeView | undefined
export function nameOf(snapshot, id)          // → display name, falls back to shortId(id)
export function isMaster(node)                // → node.following === ZERO_ID  (solo or group master)
export function masterCandidates(group)       // → member ids eligible for "make master" (all members)
export function joinTargets(snapshot, node)   // → other masters this node could follow (alive, master, not self)
export function groupLabel(group)             // → group.name || ("Group " + shortId(group.id))
export function selfNode(snapshot, selfId)    // → NodeView | undefined
export function nodesNotInAnyGroupMember(...)  // (not needed — every node is in exactly one group; omitted)
```

`joinTargets` is the data behind `JoinDropdown`: alive nodes that are masters
(`following === ZERO_ID`) and not the acting node's current master and not the
node itself. Following one re-points membership (§5.1).

### 3.4 `web/src/lib/fmt.js` — pure formatters

```js
export function shortId(id)        // first 8 hex chars (matches default node name, §1)
export function relTime(unixSec)   // "just now" | "12s ago" | "3m ago" | "2h ago" | "5d ago"
export function position(sec)      // 75.0 → "1:15"
export function bytes(n)           // 1536 → "1.5 KB"
export function cidrList(addrs)    // join CIDRs, "—" if empty
```

---

## 4. Components — props & state

Svelte 5 runes throughout. Props via `$props()`, local state via `$state`,
computed via `$derived`. Components are **stateless w.r.t. cluster data** — they
receive a slice of the current snapshot as props and call `api.js` on actions.
The only durable client state is in `ws.svelte.js` (the snapshot) plus a few
ephemeral UI bits (which media node is picked, which name is being edited).

### `App.svelte`
- **State:** none of its own beyond reading `cluster` (from `ws.svelte.js`) and a
  `self` `$state` (the `{id,name,role}` from `getStatus`, seeded once).
- **Effect (onMount):** `getStatus()` → `self`; `getCluster()` → seed
  `cluster.snapshot`; then `connect()`.
- **Renders:** header (app name, self name + role, connection dot bound to
  `cluster.status`, "stale" hint if `Date.now()-cluster.receivedAt > 10s`),
  `<Toast>`, `<Groups>`, `<Nodes>`, `<Media>` — each fed `snapshot={cluster.snapshot}`
  and `self={self}`.

### `sections/Groups.svelte`
- **Props:** `snapshot`, `self`.
- **Derived:** `groups = snapshot.groups` (sorted: named first, then by id).
- **Renders:** one `<GroupCard>` per group, passing `group`, `snapshot`, `self`.
- **State:** none.

### `components/GroupCard.svelte`
- **Props:** `group` (GroupView), `snapshot`, `self`.
- **Derived:** `master = nodeById(snapshot, group.master)`; `members =
  group.members.map(id => nodeById(snapshot,id))`; `label = groupLabel(group)`.
- **Renders:**
  - Header: `<EditableText value={label} onsave={(n)=>renameGroup(group.id,n)}>`
    (click name to rename, §10 Groups).
  - `<PlaybackBar group={group}>` (status + position + stop).
  - member list: one `<MemberRow>` per member.
  - footer: `<JoinDropdown node={self...}>`? **No** — join is per-member; see
    MemberRow. Card footer instead shows group settings summary (codec/transport/
    bufferMs) as plain text, read-only in v1 (optional editor §6).
- **State:** none (rename in-flight handled by EditableText).

### `components/MemberRow.svelte`
- **Props:** `member` (NodeView), `group` (GroupView), `self`.
- **Derived:** `isThisMaster = member.id === group.master`;
  `isSelf = member.id === self.id`.
- **Renders:**
  - member name as `<EditableText value={member.name}
    onsave={(n)=>renameNode(member.id,n)}>` (rename works remotely via proxy).
  - master badge if `isThisMaster`.
  - liveness dot (`member.alive`), `stale` mark, `relTime(member.lastSeen)`.
  - **Make master** button (hidden when `isThisMaster`): `makeMaster(member.id,
    member.id)` — issued on the member itself, takes mastership to it (§5.2).
  - **Leave** button (hidden when group has one member, i.e. solo): `unfollow(member.id)`.
  - `<JoinDropdown member={member} snapshot=… />` — re-point this member to follow
    a different master.
- **State:** none.

### `components/JoinDropdown.svelte`
- **Props:** `member` (NodeView whose membership we change), `snapshot`.
- **Derived:** `targets = joinTargets(snapshot, member)` (other alive masters).
- **Renders:** a `<select>` defaulting to "Join group…"; choosing a target calls
  `follow(member.id, target.id)` then resets the select. Disabled/hidden if
  `targets` is empty.
- **State:** local `selected` (`$state`), reset after the call.

### `components/PlaybackBar.svelte`
- **Props:** `group` (GroupView).
- **Derived:** `pb = group.playback`; `playing = pb.state === "playing"`.
- **Renders:** when playing: file name, `position(pb.positionSec)`, codec/transport
  tags, and a **Stop** button → `stop(group.master)`. When idle: "idle".
- **State:** none. (Position advances only when a new snapshot/heartbeat arrives,
  §9.2; v1 does not tick locally — keep it simple. Heartbeat is every 5 s, which
  is acceptable for a status readout.)

### `sections/Nodes.svelte`
- **Props:** `snapshot`, `self`.
- **Derived:** `nodes = snapshot.nodes` sorted (alive first, then name).
- **Renders:** a table; one `<NodeRow>` per node.
- **State:** none.

### `components/NodeRow.svelte`
- **Props:** `node` (NodeView), `self`.
- **Derived:** `isSelf = node.id === self.id`.
- **Renders:** editable name (`renameNode(node.id, …)` — proxied for remote),
  `shortId(node.id)` (full id in a `title=`), `cidrList(node.addrs)`,
  capability chips (playback yes/no, codecs, formats), alive dot + `relTime`,
  `stale` indicator.
- **State:** none.

### `sections/Media.svelte`
- **Props:** `snapshot`, `self`.
- **State (ephemeral, `$state`):**
  - `pickedNodeId` — defaults to `self.id`; chosen from a node `<select>` over
    `snapshot.nodes` that have `capabilities.formats` non-empty.
  - `files` — `MediaFile[]` for the picked node.
  - `loading`, `err`.
- **Effect:** when `pickedNodeId` changes → `getMedia(pickedNodeId)` → `files`
  (errors → toast + empty list).
- **Renders:** node picker; per file a row with `name`, `bytes(sizeBytes)`,
  `relTime(modTime)`, and **Play here** → `play(pickedNodeId, file.path)`.
  "Play here" semantics: the server makes `pickedNodeId`'s group play the file,
  doing implicit takeover if that node is a follower (§5.2 / §10). J issues a
  single `play` call to that node; it does not orchestrate takeover itself.

### `components/EditableText.svelte`
- **Props:** `value` (string), `onsave` (async fn(newValue)), `placeholder?`.
- **State:** `editing` (`$state` bool), `draft` (`$state` string).
- **Behavior:** click text → `editing=true`, `draft=value`, focus an `<input>`.
  Enter/blur → if changed, `await onsave(draft)` (errors → toast, revert);
  Esc → cancel. Reused by group name and node name (§10 "click to rename").

### `components/Toast.svelte`
- **Props:** none — reads a module-level `$state` error list from a tiny
  `toast.svelte.js` (`pushToast(msg, kind)`); `api.js` action wrappers call
  `pushToast` on `ApiError`. Auto-dismiss after ~4 s; click to dismiss.
- **State:** the shared toast list (module-scoped `$state`).

(`toast.svelte.js` is small enough to live beside `ws.svelte.js`; counted under
`web/src/lib/` — add `web/src/lib/toast.svelte.js` to the layout: pushToast +
reactive `toasts` array.)

---

## 5. Control flow

### Startup
1. `index.html` loads `src/main.js`; `main.js` calls Svelte 5 `mount(App, {target:#app})`.
2. `App.svelte` onMount effect:
   a. `getStatus()` → `self = {id,name,role}` (one shot; tolerate failure → retry
      via ws snapshot once self id is otherwise unknown; if it fails, self id is
      "" and "this node" markers are simply absent — non-fatal).
   b. `getCluster()` → seed `cluster.snapshot` so first paint isn't empty.
   c. `connect()` (ws.svelte.js) opens `/api/ws`.
3. First `cluster` WS frame replaces the seed; the page re-renders reactively.

### Steady state
- One websocket. Every `{type:"cluster"}` frame (state changes debounced ≥250 ms
  server-side, plus 5 s heartbeats — §9.2) replaces `cluster.snapshot`. Svelte's
  reactivity re-renders only the affected components.
- User actions are fire-and-forget REST calls (`api.js`). The UI does **not**
  optimistically mutate the snapshot; it waits for the server's next WS frame.
  This means a successful action visibly "lands" within one debounce/heartbeat.
  Action failures surface as toasts and the UI stays on the last server truth.
- No client timers except: ws reconnect backoff, toast auto-dismiss, and a
  cheap "stale connection" derived flag (`Date.now()-receivedAt`), recomputed on
  render (no interval needed — heartbeats drive re-render every 5 s).

### Shutdown / disconnect
- WS `onclose`/`onerror` → `status="reconnecting"`, backoff reconnect (§3.1).
  The header connection dot reflects `cluster.status`; the last snapshot stays on
  screen (greyed via a `stale` class) so the UI degrades gracefully rather than
  blanking.
- Vite HMR teardown calls `disconnect()` (dev only).

### Goroutines/channels analogue
There are none — this is a browser SPA. Concurrency is the single websocket
callback plus `fetch` promises. The "one store" is `cluster` in `ws.svelte.js`;
all components read it; only the ws callback writes `cluster.snapshot`. That is
the single-writer discipline (mirrors S's "one mutex per component" convention).

---

## 6. Edge cases & failure handling

- **WS not yet open / dropped (§9.2):** initial `getCluster()` seed prevents a
  blank page; on drop the last snapshot is shown greyed with a "reconnecting"
  dot; backoff reconnect (0.5→8 s). No data loss because the server resends a
  full snapshot on (re)connect.
- **Proxy unreachable / remote node dead (§9.3):** proxied `renameNode`/`getMedia`/
  `play` against a dead node returns an error from the local node's proxy; J
  shows the server's message as a toast and leaves state unchanged. The Media
  picker only lists nodes with non-empty `formats`, reducing dead-target picks,
  but liveness can still change between render and click → handled by the toast.
- **Play on a follower (§5.2 / §10):** J always issues `play` to the *picked
  node*, not to its master. The server performs implicit takeover when the node
  is a follower; if the server instead returns the "use takeover" hint (a
  non-master rejecting `/api/play`, §9.1), J surfaces it as a toast. (Per §10,
  "Play here" is defined to take over first; J relies on the server semantics and
  does not pre-flight a `makeMaster`.)
- **Make master races:** if a member misses the command it self-heals to solo
  (§5.2); J does nothing special — the next snapshot shows the real topology.
  The button issues exactly one `makeMaster` call.
- **Rename conflicts (LWW, §4):** two clients renaming the same group/node →
  last writer wins server-side; J just reflects whatever the next snapshot says.
  `EditableText` reverts its draft to the snapshot value on each new frame.
- **Solo node "Leave":** hidden when the group has a single member (nothing to
  leave). `unfollow` on a solo node is a harmless no-op but we hide it anyway.
- **Join targets empty:** `JoinDropdown` hides itself when `joinTargets` is empty
  (e.g. a 2-node cluster already grouped), avoiding a dead dropdown.
- **Zero / unknown ids in members:** `nodeById` may return undefined if a member
  id isn't yet in `nodes` (snapshot skew during convergence); `nameOf` falls back
  to `shortId(id)` and the row renders with a "resolving…" liveness state rather
  than crashing.
- **Stale nodes (§4, 30-day purge / liveness):** `node.stale === true` and
  `alive === false` rendered with a distinct muted style + "stale"/"offline"
  label; still listed (purge is server-side).
- **Position readout:** advances only on snapshot/heartbeat (every ≤5 s); v1 does
  not interpolate locally — accepted for simplicity (§11 out-of-scope: no
  seek/pause anyway).
- **XSS / untrusted names:** all dynamic text rendered via Svelte's default
  text interpolation (auto-escaped); no `{@html}` anywhere.
- **Group settings editing:** read-only display in v1 to keep the UI minimal;
  `setGroupSettings` helper exists in `api.js` for K/future but the card shows
  settings as text. (Spec §9.1 exposes the POST; wiring a small editor is a
  stretch goal, not required for §10's three sections.)
- **Embed/base-path (§10):** `vite.config.js` sets `base: './'` so all asset
  URLs in the built `index.html` are relative (`./assets/…`), letting Echo serve
  the embedded `web/dist` under `/` without a configured prefix. API calls use
  absolute `/api/...` paths (always rooted), independent of the embed path.

---

## 7. Build & embed integration

`web/vite.config.js`:

```js
import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";

export default defineConfig({
  plugins: [svelte()],
  base: "./",                 // relative asset paths → go:embed serving works
  build: {
    outDir: "dist",           // → web/dist (the //go:embed target; S committed the placeholder)
    emptyOutDir: true,        // overwrite placeholder on real build
    assetsDir: "assets",
    target: "es2022",
  },
  server: { proxy: {          // dev: forward /api → a running node so `npm run dev` works live
    "/api": { target: "http://localhost:8080", ws: true },
  }},
});
```

- `npm run build` (from `Makefile` `make ui`) produces `web/dist/{index.html,
  assets/*}` with **relative** URLs. The API piece (I) does `//go:embed web/dist`
  and serves `index.html` at `/` plus `assets/*`. The committed placeholder
  `web/dist/index.html` (owned by S) is overwritten by a real build and restored
  by `make clean`.
- Dev loop: `npm run dev` runs Vite with the `/api` proxy (incl. `ws:true` for
  `/api/ws`) pointing at a locally running ensemble node — full live UI without
  rebuilding the Go binary.
- `package.json` scripts: `dev` → `vite`, `build` → `vite build`, `preview` →
  `vite preview`. Deps: `svelte@^5`, `vite@^5`, `@sveltejs/vite-plugin-svelte`.
  No runtime deps shipped to the browser beyond Svelte's compiled output.

---

## 8. Styling (`app.css`)

Hand-written, dark, ~150 lines, no framework:
- CSS custom properties for the palette (`--bg`, `--panel`, `--fg`, `--muted`,
  `--accent`, `--danger`, `--ok`, `--badge`).
- Layout: a centered `max-width` column; each section is a titled panel.
- Primitives: `.card`, `.row`, `.btn`, `.btn-danger`, `.badge`, `.dot`
  (`.dot.alive`/`.dot.dead`), `.chip` (capabilities), `.muted`, `.stale`.
- `EditableText` swaps a `<span>` for a borderless `<input>` styled to match.
- Connection dot in the header reuses `.dot` with `.open/.reconnecting/.closed`.
- No images/fonts beyond system font stack (keeps `dist/assets` tiny for embed).

---

## 9. Test plan

The SPA is plain JS + Svelte; v1 keeps tests light and dependency-free (the
heavy correctness lives server-side and is covered by I/K). Unit tests target
the **pure modules**, which need no DOM and no server. Runner: `vitest`
(node env) as a dev dependency; `make ui` does not require tests to pass for the
embed, but `npm test` is wired for CI.

`web/src/lib/fmt.test.js`
- `shortId` returns first 8 chars; tolerates short input.
- `relTime` buckets: now / seconds / minutes / hours / days.
- `position` formats seconds → `m:ss` with zero-pad.
- `bytes` scales B/KB/MB with one decimal.
- `cidrList` joins, returns "—" on empty.

`web/src/lib/derive.test.js`
- `ZERO_ID` is 32 zeros; `isMaster` true iff `following === ZERO_ID`.
- `nodeById` finds / returns undefined on miss.
- `nameOf` falls back to `shortId` when node absent.
- `joinTargets` excludes self, current master, non-masters, and dead nodes.
- `groupLabel` uses name, falls back to "Group <shortId>".
- `masterCandidates` returns all member ids.

`web/src/lib/api.test.js` (fetch mocked)
- `base("")` → "/api"; `base(selfId)` → "/api" (no self-proxy);
  `base(remoteId)` → "/api/<remoteId>".
- `renameNode(remote,…)` issues `PATCH /api/<remote>/node` with `{name}`.
- `follow/unfollow/makeMaster` hit the right path on the right node id.
- `play(node,file)` posts `{file}` to `/api/<node>/play`.
- non-2xx with `{error}` body → throws `ApiError` carrying status+message.

`web/src/lib/ws.test.js` (WebSocket mocked)
- `wsURL()` derives ws scheme from http page, wss from https.
- `connect()` open → `status="open"`; a `{type:"cluster",data}` message sets
  `snapshot` and `receivedAt`.
- unknown message type is ignored (snapshot unchanged).
- `onclose` → `status="reconnecting"` and schedules a reconnect (timer mocked);
  backoff caps at 8 s; clean open resets backoff.
- second `connect()` while open is a no-op.

Component rendering tests (smoke, optional via `@testing-library/svelte`):
- `GroupCard` renders master badge on the master member and a Stop button only
  when `playback.state==="playing"`.
- `JoinDropdown` hidden when `joinTargets` empty.
- `MemberRow` hides "Make master" on the current master and "Leave" on a solo.

All tests run with `vitest` in `node`/`jsdom`, no live server, no websocket
infrastructure, no audio — matching the project's "testable without hardware"
rule.
