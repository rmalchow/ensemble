# 09 — UI screens

> Part of the **Ensemble** spec set. Spine: [README.md](./README.md) (read first).
> This document elaborates [§6.7 UI navigation](./README.md#67-ui-navigation--web) and
> consumes the API surface defined in [§6.6 HTTP/API conventions](./README.md#66-httpapi-conventions--internalweb)
> and detailed in [08-http-api-reference.md](./08-http-api-reference.md).
>
> **Dependency note.** [08-http-api-reference.md](./08-http-api-reference.md) is now
> **authoritative** on wire details (base `/api/v1`, namespaced paths, JSON,
> mTLS-or-session/API-key auth, `If-Match` optimistic concurrency, node→node proxying);
> this document is authoritative on **screen behavior**. Every endpoint cited below uses
> the canonical namespaced paths from 08 and the ConfigDoc shape in
> [§6.5](./README.md#65-config-document--internalstate).

---

## 0. Conventions for this document

The UI is a **Svelte + Vite** single-page app embedded into and **served by every
node** (mirrors the `mpvsync` `web/` pattern; see [§5 package map](./README.md#5--package--module-structure),
`web/` and `internal/web` embedded assets). Because every full node hosts the full
API and **cross-node operations are proxied node→node over mTLS** (§6.6), the UI you
load on *any* node can **operate the whole cluster**. The browser authenticates to
the node it is loaded from via an **admin session** (cookie) or **API key**
(§6.6, [D11](./README.md#3-locked-decisions-decision-log)); that node then proxies
to peers as needed using its **node client cert (mTLS)**.

**Terminology** is used exactly as defined in [§2 Glossary](./README.md#2-glossary):
*Node / player*, *Full node*, *Cluster*, *Group*, *Master*, *Controller*, *Adoption*,
*Takeover*, *Forget*, *Config doc*, *Profile*, *Allowlist*.

**Shared screen states.** Every data-backed screen implements this state machine; per
screen we only call out what is *distinctive*:

| State | Trigger | UI behavior |
|---|---|---|
| **uninitialized** | Node has no cluster identity (first run) | Whole app redirects to **Setup Wizard** (screen 1); all other routes are blocked. |
| **loading** | Initial fetch / `If-Match` revalidation in flight | Skeleton placeholders for the region being fetched; controls disabled, not hidden. |
| **empty** | Request OK but the relevant collection is `[]` | Friendly empty-state with the primary call-to-action for that screen. |
| **error** | Non-2xx; envelope `{"error":{"code","message"}}` (§6.6) | Inline error banner showing `code` + `message`, a **Retry**, and (for `409`) a **Reload & reapply** affordance. |
| **offline** | Target node unreachable, or a **proxied** call to a peer times out / TLS-fails | Region greyed with an "offline" chip; the value last seen from the ConfigDoc (gossiped, so still known) is shown dimmed with a "last known" tag. |

**`409` (optimistic concurrency).** Config-mutating screens send `If-Match: <version>`
(the ConfigDoc `version`, §6.5/§6.6). On `409` the screen reloads the current doc,
shows what changed, and asks the user to reapply — never silently overwrites.

**Proxying callouts.** Where an action targets a node/group that may not be the node
serving the UI, the action **proxies** node→node. Such actions show a subtle "via
`<servingNode>` → `<targetNode>`" hint and surface **offline** if the proxy hop fails.

**Live sync metrics** (per-node LIVE sync error, master, codec/FEC/rate in effect,
buffer health) come from **`GET /api/v1/groups/{id}/status`**. This is a **live** endpoint (polled ~1 Hz, or SSE/WebSocket if 08
defines a stream); it is distinct from the static ConfigDoc group record.

**Navigation shell.** Once initialized + authenticated, a persistent left nav exposes
the seven operational screens from §6.7: **Dashboard · Cluster · Groups · Node detail
· Media · Settings** (Node detail is reached contextually, not a flat nav item). A
global header shows the **cluster name**, the **serving node** identity, a
**connection/health** indicator, and a user menu (logout).

---

## Screen index

| # | Screen | Route (SPA) | Primary backing data |
|---|---|---|---|
| 1 | Setup Wizard | `/setup` | first-run probe; `POST /setup`; node identity |
| 2 | Login | `/login` | `POST /api/v1/auth/login` |
| 3 | Dashboard | `/` | ConfigDoc groups/nodes + `GET /api/v1/groups/{id}/status` |
| 4 | Cluster | `/cluster` | discovery feed + ConfigDoc nodes + adopt/takeover/leave |
| 5 | Groups | `/groups` | ConfigDoc groups + profile/transport + media select |
| 6 | Node detail | `/nodes/{id}` | ConfigDoc node record (incl. structured `Capabilities`) + `GET /api/v1/groups/{id}/status` |
| 7 | Media | `/media` (+ group/node scope) | `data/` browse + media commands |
| 8 | Settings | `/settings` | auth/admin pw, API keys, cluster info + CA fingerprint |

Cross-references: adoption / PIN / takeover / forget / auth → [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md);
calibration workflow / channel / HWDelayUs / drift → [06-audio-output-scheduling.md](./06-audio-output-scheduling.md);
endpoints → [08-http-api-reference.md](./08-http-api-reference.md); ConfigDoc shape +
**Capabilities** → [README §6.5](./README.md#65-config-document--internalstate);
per-node capability config (disable backend/codec, force `render:false`) →
[07-config-and-replication.md](./07-config-and-replication.md); runtime backend
discovery ([D12](./README.md#3-locked-decisions-decision-log)) + sink-less nodes
([D16](./README.md#3-locked-decisions-decision-log)/[D17](./README.md#3-locked-decisions-decision-log))
→ Node detail (screen 6); profile negotiation → [04-clock-and-groups.md](./04-clock-and-groups.md).

---

## 1. Setup Wizard (uninitialized node)

### Purpose
First-run experience for an **uninitialized** node (no cluster identity / no signed
cert yet). Offers the **two and only two** paths a fresh node can take, per
[Adoption](./README.md#2-glossary) and [D9](./README.md#3-locked-decisions-decision-log):

- **(a) Create new cluster** — this node becomes the founding member; it generates the
  **cluster CA**, sets the **cluster name** + **admin password**, and self-signs/issues
  its own node cert. (`POST /setup`.)
- **(b) Wait to be adopted** — this node stays uninitialized and **advertises itself**
  on discovery; an operator on an *already-initialized* node will **Adopt** it (screen
  4) by entering this node's **PIN**. This screen's job is to display the identity an
  operator needs and to explain the flow.

### First-run detection
On every app load the SPA probes node status (e.g. **`GET /api/v1/status`**, returning
`{initialized: bool, nodeId, fingerprint, clusterName?}`).
If `initialized == false`, the router forces `/setup` and blocks all other routes
(the **uninitialized** shared state). If `true`, `/setup` redirects to `/login` (or
`/` if a session exists). Detection must be resilient: if the probe itself errors, show
the **error** state with Retry rather than guessing.

### Wireframe
```
┌──────────────────────────────────────────────────────────────┐
│  Ensemble — Set up this player                                 │
│                                                                │
│  This node is not part of any cluster yet.                     │
│                                                                │
│   ( • ) Create a new cluster        (   ) Wait to be adopted   │
│   ───────────────────────────────────────────────────────────│
│                                                                │
│   [ Create a new cluster ]                                     │
│     Cluster name   [ Living Room Cluster................... ]  │
│     Admin password [ ••••••••••••• ]  strength: ▓▓▓▓▓░  good   │
│     Confirm        [ ••••••••••••• ]                           │
│                                                                │
│     This node becomes the first member and issues the          │
│     cluster CA. You can adopt more nodes afterward.            │
│                                                                │
│                                   [ Create cluster → ]         │
│                                                                │
│   ── or ──────────────────────────────────────────────────────│
│   [ Wait to be adopted ]                                       │
│     Node ID      n-7f3a91c2                          [copy]     │
│     Fingerprint  SHA256:1f:aa:…:9c   (this node's CSR key)      │
│     Adoption PIN  0 0 0 0   ⚠ placeholder — treated as secret   │
│                                                                │
│     On another node's UI → Cluster → Adopt, enter the          │
│     Node ID and PIN above to bring this player in.             │
│                                                                │
└──────────────────────────────────────────────────────────────┘
```

### Components
- **Path selector** (radio / segmented): *Create new cluster* vs *Wait to be adopted*.
- **Create form:** `clusterName` (required), `adminPassword` + confirm (with a
  client-side strength meter; hashing is **argon2id** server-side per `internal/auth`,
  [§5](./README.md#5--package--module-structure)).
- **Adopt-target panel:** read-only **Node ID**, **Fingerprint** (the node's CSR public
  key fingerprint that the CA will sign — see [03](./03-adoption-takeover-security-pki.md)),
  and the **Adoption PIN** with a prominent note that the placeholder is `0000` but is
  **treated as a real secret** in the protocol ([D9](./README.md#3-locked-decisions-decision-log)).
  Copy buttons on ID + fingerprint.
- Submit + inline validation; help text linking the adopt flow to **Cluster** (screen 4).

### Data sources
- First-run probe: `GET /api/v1/status`.
- Path (a): **`POST /api/v1/setup`** with `{clusterName, adminPassword}`. On success the node initializes,
  the SPA stores the new session (or routes to **Login**), and lands on **Dashboard**.
- Path (b): identity for display comes from the same status probe (`nodeId`,
  `fingerprint`). No write happens here — the node simply keeps advertising on
  **discovery** (`internal/discovery`) until adopted from elsewhere.

### States
- **uninitialized**: the *only* screen reachable; this is its native state.
- **loading**: probing first-run status; skeleton on both panels.
- **error**: `POST /setup` failure (e.g. weak password rejected, or node already
  initialized → redirect to Login) shows the envelope `code`/`message`.
- **empty / offline**: not meaningful here (single local node, pre-cluster). If the
  local API itself is unreachable, show a hard "cannot reach this player" error.

### User flows
1. **Create cluster:** select (a) → fill name + password → **Create cluster** → node
   issues CA + self-cert, persists initial ConfigDoc (`version: 1`) → auto-login →
   **Dashboard**.
2. **Be adopted:** select (b) → operator reads Node ID + fingerprint + PIN → goes to
   another node's **Cluster** screen and **Adopts** this node (screen 4). When adoption
   completes, this node flips to `initialized` (it learns this via gossip / its CSR
   being signed); the wizard auto-advances to **Login**. See full handshake in
   [03 §adoption](./03-adoption-takeover-security-pki.md).

---

## 2. Login

### Purpose
Authenticate the browser to the serving node for an **initialized** node. Establishes
an **admin session** from the single **cluster admin password**
([D11](./README.md#3-locked-decisions-decision-log)). API keys are for programmatic
clients (managed in **Settings**), not entered here.

### Wireframe
```
┌────────────────────────────────────────────┐
│            Ensemble — Living Room            │
│                                              │
│        Admin password                        │
│        [ ••••••••••••••••••• ]               │
│        [ ] keep me signed in                 │
│                                              │
│                       [  Sign in  ]          │
│                                              │
│        Wrong password.            (error)    │
│        Serving node: n-7f3a91c2 ·  CA ✔      │
└────────────────────────────────────────────┘
```

### Components
- Password field; "keep me signed in" (session duration); submit.
- Footer chips: **cluster name**, **serving node id**, CA-trust indicator.

### Data sources
- **`POST /api/v1/auth/login`** with `{password}` → sets session cookie. Single-admin
  model; no username (D11).
- Logout: **`POST /api/v1/auth/logout`**, clears session.

### States
- **loading**: submit in flight (button spinner, field locked).
- **error**: bad password → generic "wrong password" (no user enumeration); rate-limit
  message if the node throttles attempts (see [03 auth/threat model](./03-adoption-takeover-security-pki.md)).
- **uninitialized**: if the node is not initialized, redirect to **Setup Wizard**.
- **offline**: serving node unreachable → "cannot reach this player; try another node's
  URL" (any node hosts the UI, so the user can use a peer).

### User flows
- Enter password → **Sign in** → session set → land on the route originally requested
  (default **Dashboard**). Session expiry anywhere returns the user here.

---

## 3. Dashboard

### Purpose
At-a-glance operation of the **whole cluster**: every **Group** with its play state,
its **members**, the **master**, and the **per-node LIVE sync error**; plus
quick **play/stop** per group. This is the landing screen.

### Wireframe
```
┌─ Dashboard ───────────────────────────────────  cluster: Living Room  ●3 nodes ┐
│                                                                                 │
│  ┌─ Group: Downstairs ─────────────────────────────────┐  ▶ playing            │
│  │  master: N1 (Kitchen)    media: jazz-loop.mp3 ↻      │  [ Stop ]             │
│  │  profile: opus · XOR · 48k   transport: UDP unicast  │                      │
│  │  ┌ member ──────┬ role ──┬ sync err ─┬ state ──────┐ │                      │
│  │  │ N1 Kitchen   │ left   │  — (mstr) │ ● rendering  │ │                      │
│  │  │ N2 Diner     │ right  │  +0.21 ms │ ● rendering  │ │                      │
│  │  │ N4 Hall      │ stereo │  +1.8 ms ⚠│ ● rendering  │ │                      │
│  │  └──────────────┴────────┴───────────┴──────────────┘ │                      │
│  └──────────────────────────────────────────────────────┘                      │
│                                                                                 │
│  ┌─ Group: Studio (solo N3) ──────────────────────────┐  ◼ stopped             │
│  │  master: N3   media: (none selected)                │  [ Play ] (disabled)   │
│  │  N3 Studio   stereo   — (mstr)   ○ idle             │   select media →       │
│  └─────────────────────────────────────────────────────┘                      │
│                                                                                 │
│  ┌─ Group: Outdoor ────────────────────────────────────┐  ▶ playing            │
│  │  master: N8 (NAS) ⊘ master (no local audio)          │  [ Stop ]             │
│  │  media: ocean.mp3 ↻    profile: pcm · XOR · 48k       │                      │
│  │  ┌ member ──────┬ role ──┬ sync err ─┬ state ──────┐ │                      │
│  │  │ N8 NAS       │  —     │  — (mstr, │ ⊘ no sink    │ │                      │
│  │  │              │        │  no audio)│  (origin)    │ │                      │
│  │  │ N6 Office    │ stereo │  +0.30 ms │ ● rendering  │ │                      │
│  │  └──────────────┴────────┴───────────┴──────────────┘ │                      │
│  └─────────────────────────────────────────────────────┘                      │
│                                                                                 │
│  Offline: N5 (Garage) — last known in group "Outdoor"   ⌁ offline               │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Components
- **Group cards**, one per `Groups[]` in the ConfigDoc. Each card:
  - Header: group name, **play state** (the group record's **`Playing`** bool, §6.5 —
    `playing`/`stopped`), **quick Play/Stop**.
  - Sub-line: **master** node, selected **media** (+ loop ↻), and the **negotiated
    profile** (codec/FEC/rate) and **transport** (read-only summary; edited in Groups).
  - **Members table**: node name, **channel role**, **LIVE sync error** (ms, signed;
    master shows "—"), and per-node render **state** dot. Sync error over a threshold
    is flagged ⚠ (threshold/units per [06](./06-audio-output-scheduling.md) drift loop).
    A **sink-less node** (`Caps.Render == false`, [D17](./README.md#3-locked-decisions-decision-log))
    **is not a listener**: it shows **no per-listener sync error** ("—"), no channel role,
    and a **"⊘ no sink"** state instead of a render dot. It **can still be a group
    master/origin** — when it is, the card header and its member row carry a
    **"master (no local audio)"** badge (⊘) to make clear it drives the stream/clock but
    plays nothing locally.
- **Offline nodes strip**: cluster members currently unreachable, with their last-known
  group (from gossiped ConfigDoc).
- Cluster health chip (node count online/total) in the header.

### Data sources
- Static structure (groups, members, roles, selected media, profile, transport, and the
  stored **`Playing`** bool): ConfigDoc composed from **`GET /api/v1/cluster/info`** +
  **`GET /api/v1/nodes`** + **`GET /api/v1/groups`**.
- **LIVE** per-node sync error, master, codec/FEC/rate-in-effect, buffer health:
  **`GET /api/v1/groups/{id}/status`**, polled ~1 Hz per visible group (or a single
  SSE/WS stream if 08 defines one).
- Quick Play/Stop: **`POST /api/v1/groups/{id}/play`** and **`POST /api/v1/groups/{id}/stop`**.
  These flip the group record's **`Playing`** bool (§6.5) and **proxy** to the group
  **master** if the serving node isn't the master.

### States
- **loading**: card skeletons; play/stop disabled until status arrives.
- **empty**: no groups → empty state "No groups yet — create one in **Groups**" (link).
- **error**: per-card error banner if that group's `/status` fails; the rest still render.
- **offline**: a member node unreachable → its row dims, sync-error shows "—" with an
  offline chip; whole-group offline (master unreachable) disables Play/Stop and shows
  last-known state from gossip. (Distinguish from a **sink-less** "—": offline is dimmed +
  offline-chip; sink-less is normal-weight with the "⊘ no sink" / "master (no local
  audio)" badge — it is a configuration, not a fault.)
- **uninitialized**: never reached (gated by router).

### User flows
- **Quick play/stop** a group from its card; spinner on the button until `/status`
  reflects the new play state. If the master is a peer, the call proxies (hint shown);
  proxy failure → offline treatment + error toast.
- Click a group → **Groups** (screen 5) for that group. Click a member → **Node
  detail** (screen 6). Click "select media" on a media-less group → **Media** (screen 7).

---

## 4. Cluster

### Purpose
Manage **cluster membership**: discover **uninitialized** nodes and **Adopt** them
(PIN-gated), and manage **existing member nodes** — **Takeover** (force re-adopt) and
**Forget** (revoke + remove), with online state and the **CA fingerprint** shown.
All trust operations are specified in [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md);
this screen is its operator surface.

### Wireframe
```
┌─ Cluster ───────────────────────────  CA fingerprint: SHA256:1f:aa:…:9c  [copy] ┐
│                                                                                 │
│  Discovered — not yet in this cluster                              [ rescan ]   │
│  ┌ node ─────────┬ addr ───────────┬ fingerprint ──┬ action ──────────────────┐ │
│  │ n-9c12… (new) │ 192.168.1.42    │ SHA256:be:…:04│ PIN [0 0 0 0] [ Adopt ]   │ │
│  │ n-aa57… (new) │ 192.168.1.50    │ SHA256:77:…:e1│ PIN [0 0 0 0] [ Adopt ]   │ │
│  └───────────────┴─────────────────┴───────────────┴──────────────────────────┘ │
│                                                                                 │
│  Cluster members                                                                │
│  ┌ node ─────────┬ addrs ──────────┬ status ─┬ actions ──────────────────────┐ │
│  │ N1 Kitchen    │ 192.168.1.10    │ ● online│ [ Node ] [ Takeover ] [Forget]│ │
│  │ N2 Diner      │ 192.168.1.11    │ ● online│ [ Node ] [ Takeover ] [Forget]│ │
│  │ N5 Garage     │ 192.168.1.20    │ ⌁ offlin│ [ Node ] [ Takeover ] [Forget]│ │
│  └───────────────┴─────────────────┴─────────┴───────────────────────────────┘ │
│                                                                                 │
│  ⚠ Forget N5? Revokes its cert and drops it from config + allowlist. [Cancel][Forget] │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Components
- **CA fingerprint** in the header (from `Cluster.CACert`, ConfigDoc §6.5) with copy —
  the trust anchor operators verify out-of-band.
- **Discovered (uninitialized) list**: each row shows node id, source addr, the node's
  **CSR fingerprint**, and a **PIN field defaulting to `0000`** ([D9](./README.md#3-locked-decisions-decision-log))
  plus an **Adopt** button. A **Rescan** triggers a fresh discovery sweep.
- **Members list**: node id/name, known **addrs**, **online status** (gossip/health),
  and per-row **Node** (→ screen 6), **Takeover**, **Forget** actions. A **sink-less**
  member (`Caps.Render == false`, [D17](./README.md#3-locked-decisions-decision-log)) is
  tagged "**control / media only**" so an operator knows it has no audio output (it can
  still be a group master/origin); the tag links to its **Node detail** Capabilities panel.
- **Confirm dialogs** for Takeover and Forget (both are disruptive; Forget revokes the
  cert and removes the node from the **allowlist**).

### Data sources
- Discovered nodes: **`GET /api/v1/discovery`** — the broadcast/mDNS feed of nearby
  uninitialized advertisers (`internal/discovery`).
- Members + status: ConfigDoc `Nodes[]` (**`GET /api/v1/nodes`**) joined with
  online/health from cluster gossip (**`GET /api/v1/cluster/info`**).
- **Adopt**: **`POST /api/v1/cluster/adopt`** with `{nodeId, addr, pin}`. The serving
  node performs the PIN-gated CSR-sign handshake (CA signs the new node's CSR; see
  [03 §adoption](./03-adoption-takeover-security-pki.md)), then writes the new
  `NodeRecord` to the ConfigDoc with `If-Match`.
- **Takeover**: **`POST /api/v1/cluster/takeover`** — forced cert re-issue for a node
  that belongs to another/old cluster ([Takeover](./README.md#2-glossary)), also
  PIN-gated per [03].
- **Forget**: **`POST /api/v1/nodes/{id}/forget`** — revoke cert, drop the `NodeRecord`, and
  remove its addrs from the **allowlist** (§6.5). (A node forgetting *itself* uses the
  coordinated **`POST /api/v1/cluster/leave`** path — see Settings, screen 8.)
- CA fingerprint: derived from `Cluster.CACert` in the ConfigDoc.

### States
- **loading**: two skeleton tables.
- **empty**: no discovered nodes → "No new players found nearby — power one on, or it
  may already belong to a cluster (use Takeover)"; no members would only happen on a
  one-node cluster (the serving node lists itself).
- **error**: adopt failure surfaces the envelope `code`/`message` (notably a **bad-PIN**
  error from [03] — keep it generic, no PIN oracle beyond the protocol's own gating);
  `409` on the config write → reload + reapply.
- **offline**: a member shows ⌁ offline; **Takeover/Forget still available** (Forget of
  an offline node still revokes + removes it; Takeover may need the node reachable to
  complete the handshake — show a clear "node must be reachable" message if so).
- All of Adopt/Takeover/Forget **proxy** when the operation must run from/against a
  different node; the "via serving node → target" hint applies.

### User flows
1. **Adopt:** operator confirms the discovered node's fingerprint matches the one shown
   on that node's **Setup Wizard** (screen 1b), enters the **PIN** (default `0000`),
   clicks **Adopt** → handshake → node joins → it disappears from "discovered" and
   appears under "members" once gossip converges.
2. **Takeover:** for a node stuck in another/old cluster → **Takeover** → confirm →
   PIN-gated forced re-issue ([03]) → node re-homes into this cluster.
3. **Forget:** **Forget** → confirm dialog (explicitly states cert revocation +
   allowlist removal) → node removed; if it was a group member, the group's membership
   updates and (if it was master) a re-election occurs (see [04](./04-clock-and-groups.md)).

---

## 5. Groups

### Purpose
Compose and operate **Groups**: list/create groups, **assign/move nodes** between
groups, view/override a group's **negotiated Profile** (codec/FEC/rate — mostly
**auto/read-only** with override) and **transport**, and **select media** for the group.
Profile negotiation semantics live in [04-clock-and-groups.md](./04-clock-and-groups.md);
this screen edits the inputs and shows the negotiated result.

### Wireframe
```
┌─ Groups ─────────────────────────────────────────────────────  [ + New group ] ┐
│  ┌ Groups ───────────┐   ┌ Group: Downstairs ─────────────────────────────────┐ │
│  │ ▸ Downstairs (3)  │   │  name [ Downstairs............ ]   master: N1        │ │
│  │   Studio (1)      │   │                                                      │ │
│  │   Outdoor (1)     │   │  Members            Unassigned / other groups        │ │
│  │   ─────────────   │   │  ┌───────────┐      ┌───────────┐                    │ │
│  │   Unassigned (0)  │   │  │ N1 Kitchen│  ◀▶  │ N6 Office │  (drag or select)  │ │
│  │                   │   │  │ N2 Diner  │      │ N7 Den    │                    │ │
│  │                   │   │  │ N4 Hall   │      └───────────┘                    │ │
│  │                   │   │  └───────────┘                                       │ │
│  │                   │   │                                                      │ │
│  │                   │   │  Profile (auto-negotiated)            [ override ▾ ] │ │
│  │                   │   │   codec  opus   (least-capable: N4)   ○ auto ● set   │ │
│  │                   │   │   FEC    XOR-parity                   ○ auto ● set   │ │
│  │                   │   │   rate   48000 Hz                                    │ │
│  │                   │   │   transport  ● UDP unicast  ○ TCP fallback           │ │
│  │                   │   │                                                      │ │
│  │                   │   │  Media   jazz-loop.mp3  ↻ loop   [ change → Media ]   │ │
│  │                   │   │                              [ Play ]  [ Stop ]      │ │
│  └───────────────────┘   └──────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Components
- **Group list** (left): all `Groups[]` with member counts, plus a synthetic
  **Unassigned** bucket for adopted nodes not in any explicit group. **New group** CTA.
  (Per [glossary](./README.md#2-glossary) a node is in **exactly one group**, possibly
  a group of one; "Unassigned" is the UI's name for not-yet-placed members.)
- **Group editor** (right):
  - **Rename** field; **master** indicator (elected, read-only — see [04]/[02]). When the
    elected master is a **sink-less** node (`Caps.Render == false`,
    [D17](./README.md#3-locked-decisions-decision-log)) it carries a **"master (no local
    audio)"** badge: it originates the stream + serves the clock but renders nothing
    locally (it is not a listener). Such a node is a valid master/origin and a valid group
    member; it simply never appears as a *listener* in profile negotiation (its
    `DecodeCodecs`/render are absent from the least-capable computation).
  - **Membership transfer**: two panels (this group ↔ others/unassigned) supporting
    **drag** or **multiselect + move**. Moving a node rewrites `memberNodeIDs[]` on both
    affected groups.
  - **Profile** block: **codec / FEC / rate**, each shown as the **auto-negotiated**
    value with the limiting ("least-capable") member named ([D3](./README.md#3-locked-decisions-decision-log)
    negotiates to least-capable; codec set is `"pcm"|"opus"` with **`pcm`** the mandatory
    baseline, R4(a)). Each is **auto by default / read-only**, with an explicit
    **override** toggle (D3/D4 are 🟡 defaults). Codec/FEC/transport are the JSON
    **string enums** (`"pcm"|"opus"`, `"none"|"xorParity"|"duplicate"`, `"udp"|"tcp"`),
    not wire integer ids. `rate` is canonical-rate framing (48k) per §6.4.
  - **Transport** radio: **UDP unicast** (default) / **TCP fallback** ([D2](./README.md#3-locked-decisions-decision-log)).
  - **Media** summary + loop toggle + a jump to **Media** (screen 7) scoped to this group.
  - **Play / Stop** for the group.

### Data sources
- Groups + members + profile + transport + media + **`Playing`**: ConfigDoc `Groups[]`
  (**`GET /api/v1/groups`**).
- Create group: **`POST /api/v1/groups`** `{name}`.
- Rename / membership move / profile override / transport / loop: **`PATCH
  /api/v1/groups/{id}`** with `If-Match`. Membership moves that touch two groups are a
  single transactional config write (§6.5 is one doc; LWW per §6.5/§7).
- Negotiated profile *result* (and which member is limiting) is reported by **`GET
  /api/v1/groups/{id}/status`** — the live view, vs. the stored override.
- Media selection is performed in **Media** (screen 7) and stored on the group record
  (`Groups[].media`, §6.5).
- Play/Stop: **`POST /api/v1/groups/{id}/play`** / **`POST /api/v1/groups/{id}/stop`**
  flip the group's **`Playing`** bool (§6.5), proxied to master.

### States
- **loading**: list + editor skeletons.
- **empty**: no groups → big **New group** prompt; **Unassigned** still lists adopted
  nodes awaiting placement.
- **error**: write failures show envelope; **`409`** is likely here (membership is hot)
  → reload doc, re-show the move, ask to reapply.
- **offline**: a member node offline still shows in the group (membership is config, not
  liveness) but is dimmed; the **negotiated profile may be stale** if the limiting node
  is offline — flagged. Play/Stop offline if master unreachable.
- **uninitialized**: gated.

### User flows
1. **Create group:** **New group** → name → empty group created → drag nodes in.
2. **Move node between groups:** drag from one panel to another (or multiselect →
   *Move to…*) → single `PATCH` updates both groups' `memberNodeIDs[]` → re-election /
   profile re-negotiation happen server-side ([04]); the live profile updates from
   `/status`.
3. **Override profile:** flip a codec/FEC field from **auto** to **set**, choose a value
   → `PATCH` → negotiation respects the override (subject to least-capable feasibility;
   infeasible overrides return an error). Switch **transport** UDP↔TCP similarly.
4. **Select media / loop:** jump to **Media** → pick mp3 + loop → returns here; or
   toggle loop inline. Then **Play**.

---

## 6. Node detail (any node in the cluster)

### Purpose
Configure and inspect **any** node in the cluster (the serving node or any peer, via
proxy): identity/**rename**, **channel role** (stereo/left/right), **hardware-delay**
(`HWDelayUs`, **manual entry** with a **calibration helper**), **gain**, addresses,
cert/online status, and the node's structured **`Capabilities`** & audio backends
([D16](./README.md#3-locked-decisions-decision-log),
[§6.5 Capabilities](./README.md#65-config-document--internalstate)). Channel/gain/HWDelayUs
and the calibration workflow are specified in [06-audio-output-scheduling.md](./06-audio-output-scheduling.md).

> **Sink-less (`Render=false`) nodes.** A full node may run **without an audio sink**
> ([D17](./README.md#3-locked-decisions-decision-log)) — e.g. a NAS/docker node — and
> still be a cluster member, **media store**, **clock**, **group master/origin**, and UI.
> Such a node is **not a listener** and has **no local audio output**. On Node detail this
> means the **audio-rendering controls (channel role, hardware delay, gain) are hidden**
> and replaced by a clear "control / media only" state; the **Capabilities & audio
> backends** panel is shown in both cases (it is *how* a node is made sink-less and *why*).
> Whether a node is sink-less is read from its advertised `Caps.Render` (effective =
> probed ∩ enabled, [D16](./README.md#3-locked-decisions-decision-log)), not a build flag.

> **Spine terminology note.** The ConfigDoc field is `HWDelayUs`
> ([§6.5 NodeRecord](./README.md#65-config-document--internalstate)). The glossary/[D13]
> prose also writes "`DelayUs`"; this screen uses the **field name `HWDelayUs`** and
> labels it "Hardware delay (µs)". (Flagged in the final note.)

### Wireframe — A. rendering node (`Caps.Render = true`)
```
┌─ Node: N4 — Hall ───────────────────────  ● online · cert ✔ (signed by CA)  ⤺ back ┐
│                                                                                     │
│  Identity                                                                           │
│   name [ Hall.................. ]   id n-4d22…   group: Downstairs                   │
│                                                                                     │
│  ── Audio output (shown only when Render = true) ───────────────────────────────── │
│  Channel role        ○ stereo   ○ left   ● right                                    │
│  Gain (dB)           [-6 ────●──────────── +6 ]   -1.5 dB                            │
│                                                                                     │
│  Hardware delay (HWDelayUs)  — manual entry                                          │
│   [ 0 ─────────●──────────── 20000 µs ]   8200 µs   [ 8200 ] µs                      │
│   ┌ Calibration helper ───────────────────────────────────────────────┐            │
│   │ 1. Add N4 to a group with a reference node.                        │            │
│   │ 2. [ Play test signal ]  (built-in click+tone, synchronous, see 06)│            │
│   │ 3. Judge the offset by ear / phone-mic.                            │            │
│   │ 4. Type the trim into HWDelayUs above and Save.                    │            │
│   │    live sync err (from group status): +0.2 ms ✔                    │            │
│   │  (automated cross-correlation measurement = future enhancement)    │            │
│   └────────────────────────────────────────────────────────────────────┘           │
│  ──────────────────────────────────────────────────────────────────────────────── │
│                                                                                     │
│  Capabilities & audio backends            (runtime-discovered — D12; not a build flag) │
│   render          ● yes (listener-capable)                                          │
│   sinks (probed)  ☑ alsa     (precise — snd_pcm_delay)                               │
│                   ☑ pipewire (precise)                                               │
│                   ☑ exec:aplay (coarse — Delay() ok=false)                           │
│   encode codecs   ☑ pcm   ☑ opus            (wire set; pcm baseline)                 │
│   decode codecs   ☑ pcm   ☑ opus                                                    │
│   fec             ☑ xorParity   ☑ duplicate                                         │
│   max rate        [ 48000 ] Hz                                                       │
│   ┌ Disable available paths (per-node config → effective caps) ─────────┐           │
│   │ Unchecking a probed path removes it from what this node advertises.  │           │
│   │ [ ] force control-only (render:false) — turns this into a sink-less  │           │
│   │     node; hides the audio-output controls above.                     │           │
│   └──────────────────────────────────────────────────────────────────────┘          │
│                                                                                     │
│  Network                                                                            │
│   addrs  192.168.1.13   (drives allowlist)        fingerprint SHA256:be:…:04        │
│                                                                                     │
│                                            [ Revert ]   [ Save changes ]            │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Wireframe — B. sink-less node (`Caps.Render = false`, e.g. NAS/docker)
```
┌─ Node: N8 — NAS ────────────────────────  ● online · cert ✔ (signed by CA)  ⤺ back ┐
│                                                                                     │
│  Identity                                                                           │
│   name [ NAS.................. ]   id n-8e10…   group: Downstairs (master)           │
│                                                                                     │
│  ┌ Control / media only — no audio output ───────────────────────────────┐         │
│  │ This node advertises render = false (no usable+enabled audio sink).    │         │
│  │ It can be a group master/origin, media store, clock, and UI — but it   │         │
│  │ is not a listener. Channel role, hardware delay, and gain do not apply  │         │
│  │ and are hidden. To give it audio output, enable a sink below (if a      │         │
│  │ backend was probed) or clear "force control-only".                      │         │
│  └─────────────────────────────────────────────────────────────────────────┘        │
│                                                                                     │
│  Capabilities & audio backends            (runtime-discovered — D12; not a build flag) │
│   render          ○ no  (control / media only)                                      │
│   sinks (probed)  (none usable+enabled)                                             │
│      ↳ probed but disabled:  ☐ exec:aplay (coarse)   ← re-enable to gain output     │
│   encode codecs   ☑ pcm   ☑ opus              (can ORIGINATE as master)             │
│   decode codecs   —  (not a listener)                                               │
│   fec             ☑ xorParity   ☑ duplicate                                         │
│   max rate        [ 48000 ] Hz                                                       │
│   ┌ Disable available paths (per-node config → effective caps) ─────────┐           │
│   │ [✓] force control-only (render:false)  — clear to attempt rendering  │           │
│   └──────────────────────────────────────────────────────────────────────┘          │
│                                                                                     │
│  Network                                                                            │
│   addrs  192.168.1.18   (drives allowlist)        fingerprint SHA256:c0:…:7a         │
│                                                                                     │
│                                            [ Revert ]   [ Save changes ]            │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

### Components
- **Header**: node name, **online status**, **cert status** (signed-by-CA / present),
  back link to wherever the user came from (Dashboard/Cluster/Groups).
- **Identity**: **rename** field; read-only id + current group (with a **master** marker,
  and "(no local audio)" appended when the node is the group master *and* sink-less).
- **Audio output section — conditional on `Caps.Render`** ([D17](./README.md#3-locked-decisions-decision-log)):
  - **When `Render == true`:** show the **Channel role** radio (**stereo / left / right**,
    [D13](./README.md#3-locked-decisions-decision-log)), the **Gain** slider (dB trim,
    `GainDB`), and the **Hardware delay** control for **`HWDelayUs`** as **manual entry**
    (slider + numeric µs field) with the **Calibration helper** card. The helper offers a
    **"Play test signal"** button (the built-in click+tone signal, played **synchronously**
    on the selected node(s) via **`POST /api/v1/calibrate/play`**, R10); the operator
    **judges the offset by ear / phone-mic** and **types the trim into `HWDelayUs`** then
    Saves. The helper reads the **live sync error** from `GET /api/v1/groups/{id}/status`
    to confirm convergence. **Automated cross-correlation measurement is a future
    enhancement** (would upload a recording → suggest `HWDelayUs`); the MVP measurement is
    manual. Exact signal and procedure per [06-audio-output-scheduling.md](./06-audio-output-scheduling.md).
  - **When `Render == false`** (sink-less, e.g. NAS/docker): **all three of channel role,
    hardware delay, and gain are hidden** (they have no meaning without a sink). In their
    place a **"Control / media only — no audio output"** panel explains the node can still
    be **master/origin, media store, clock, and UI** but is **not a listener**, and points
    at the Capabilities panel to re-enable a sink.
- **Capabilities & audio backends** panel (**always shown**, both Render states): renders
  the node's advertised **effective structured `Capabilities`** object
  ([§6.5](./README.md#65-config-document--internalstate),
  [D16](./README.md#3-locked-decisions-decision-log) — `detected(runtime) ∩ enabled(config)`),
  **not** a flat string array of feature names:
  - **`Render`** flag (yes = listener-capable / no = control-media-only).
  - **Sinks** — the usable+enabled output backends from `Caps.Sinks`, each tagged
    **precise** or **coarse** per its `Backend.Precise` (`alsa` direct-ioctl/precise, `pipewire`
    precise, `exec:aplay` coarse — `Delay()` `ok=false`; see [§6.1](./README.md#61-audio-output--internalaudiosink)/[D12](./README.md#3-locked-decisions-decision-log)).
  - **Encode codecs** (`EncodeCodecs`, what it can ORIGINATE as master) and **Decode
    codecs** (`DecodeCodecs`, what it can PLAY as a listener — shown as "—" when not a
    listener), each a **string enum** set (`"pcm"|"opus"`, R4(a); `pcm` is the mandatory
    wire baseline, R2); **FEC** (`FEC`, `"xorParity"|"duplicate"`), and **Max rate**
    (`MaxRate`).
  - **Per-node disable toggles** — for each **technically-available (probed) path**, an
    enable/disable control that **masks** the runtime-detected capability via per-node
    config, changing what the node advertises: disable a **backend** (e.g. PipeWire),
    disable a **codec** (e.g. Opus), disable a **FEC**, or **force control-only
    (`render:false`)**. These write the **per-node config** (cross-ref
    [07-config-and-replication.md](./07-config-and-replication.md)); the node re-probes /
    re-masks and re-advertises its effective `Caps`. A path that was **never probed** on
    this node is not offered (you cannot enable what the runtime did not discover, D12).
    Toggling **force control-only** flips the Audio-output section above between A/B.
- **Network**: known **addrs** (with a note they **drive the allowlist**, §6.5) and the
  node's cert **fingerprint**.
- **Save / Revert** (dirty-state tracked; `If-Match` on save).

### Data sources
- Node record (incl. advertised effective structured `Capabilities`): ConfigDoc `Nodes[]`
  (**`GET /api/v1/nodes/{id}`**). `Capabilities` is `detected(runtime) ∩ enabled(config)`
  (§6.5/D16); the node computes it from a **runtime probe** of available backends (D12),
  not a build flag, then masks it with per-node config.
- Save: **`PATCH /api/v1/nodes/{id}`** `{name?, channel?, gainDb?, hwDelayUs?,
  capabilities?}` with `If-Match`. **Proxies** to the target node where the change must
  take local effect (e.g. channel/HWDelayUs affect that node's renderer/sink, [06];
  capability enable/disable changes what the node probes/masks and re-advertises); the
  config write itself replicates by gossip (§7).
- Capability/backend toggles (disable a backend/codec/FEC, force `render:false`): write the
  **per-node config** carried in the same node record — cross-ref
  [07-config-and-replication.md](./07-config-and-replication.md). After save the target
  node re-probes/re-masks and the new effective `Caps` (including a flipped `render`)
  re-appears via gossip; channel/HWDelayUs/gain edits sent for a node that is now
  `render:false` are ignored server-side (no sink).
- Calibration test signal: **`POST /api/v1/calibrate/play`** `{nodeIds | groupId,
  durationSec}` — plays the built-in click+tone signal **synchronously** on the selected
  node(s) (R10). The operator then **manually enters `HWDelayUs`** (no server-side offset
  suggestion in the MVP); automated cross-correlation `measure` is a documented future
  enhancement. See [06](./06-audio-output-scheduling.md).
- Live sync error: **`GET /api/v1/groups/{id}/status`**, for the node's current group.

### States
- **loading**: form skeleton.
- **empty**: not applicable (a node detail always has a record; a stale link to a
  forgotten node → **error**/not-found).
- **error**: not-found (node forgotten mid-edit) → message + back to Cluster; save
  `409` → reload + reapply.
- **sink-less (`Render == false`)**: a *content* variant, not an error — the audio-output
  controls (channel/HWDelay/gain + calibration helper) are **hidden** and the "control /
  media only" panel is shown; the Capabilities panel remains, so the operator can re-enable
  a probed sink or clear "force control-only".
- **offline**: target node offline → **read-only** view from gossiped ConfigDoc with an
  offline banner; **HWDelayUs/channel/gain edits, the calibration helper, and the
  capability/backend toggles are disabled** (they require the live node to re-probe);
  the last-known effective `Caps` is shown dimmed. Rename (config-only) **may** still be
  allowed and will apply on reconverge — clearly labeled "applies when node returns".
- **uninitialized**: gated.

### User flows
1. **Rename / set channel / gain:** edit → **Save** → `PATCH` (proxied) → values
   replicate; group profile/render updates as needed. (Channel/gain only present when
   `Render == true`.)
2. **Calibrate hardware delay:** ensure node is in a group with a reference → **Play test
   signal** (`POST /api/v1/calibrate/play`, synchronous on the selected node) → judge the
   offset by ear / phone-mic → **type the trim into `HWDelayUs`** (manual entry) → **Save**
   → watch **live sync err** trend to ✔. Automated cross-correlation measurement is a
   future enhancement. Full method: [06]. (Only available on a rendering node; hidden when
   `Render == false`.)
3. **Disable an available path:** in **Capabilities & audio backends**, uncheck a probed
   backend/codec/FEC → **Save** → node re-masks and re-advertises a narrower effective
   `Caps`; group profile re-negotiates if a now-removed codec/FEC was in use ([04]).
4. **Make a node control-only (sink-less):** check **force control-only (`render:false`)**
   → **Save** → the node drops to `Render=false`, the audio-output controls disappear
   (variant B), and it remains usable as master/origin/media/clock. Clearing the toggle
   (with a probed sink available) restores rendering and the audio-output controls.
5. **From offline → back:** if node is offline, view last-known config + last-known
   `Caps`; edits requiring the live node (including capability re-probe) are blocked until
   it returns.

---

## 7. Media

### Purpose
Browse a node's / group's **`data/`** folder, list the **mp3s**, **select** one,
toggle **loop**, **play/stop**, and show a **now-playing** indicator. Media model is
[D14](./README.md#3-locked-decisions-decision-log): pick an mp3 from `data/`, play
**looped**, **master-side decode** ([D5](./README.md#3-locked-decisions-decision-log)).
The `data/` browse comes from `internal/stream/source` ([§5](./README.md#5--package--module-structure)).

### Wireframe
```
┌─ Media ─────────────────────  scope: Group "Downstairs"  (master N1)  ▼ change ┐
│                                                                                │
│  data/ on N1 (Kitchen)                                          [ refresh ]    │
│  ┌ file ───────────────────────┬ length ─┬ ─────────────────────────────────┐ │
│  │ ● jazz-loop.mp3   ♪ playing  │ 3:12    │ [ Stop ]   ↻ loop ON              │ │
│  │   ocean.mp3                  │ 9:48    │ [ Select & play ]                 │ │
│  │   chime.mp3                  │ 0:04    │ [ Select & play ]                 │ │
│  │   speech-test.mp3            │ 1:20    │ [ Select & play ]                 │ │
│  └──────────────────────────────┴─────────┴───────────────────────────────────┘ │
│                                                                                │
│  Now playing:  jazz-loop.mp3 ↻   on group Downstairs   ▶ 00:48 / 03:12          │
└────────────────────────────────────────────────────────────────────────────────┘
```

### Components
- **Scope switcher**: choose the **group** (media plays on the group's **master**) or a
  **node** scope. The browsed `data/` belongs to the **master** of the scoped group
  (master-side decode), surfaced clearly ("data/ on N1").
- **File list**: filenames, length, and per-row **Select & play** (or **Stop** for the
  current selection). **Loop** toggle (↻) — default loop per D14.
- **Now-playing** bar: current file, loop state, target group, and a **position/length**
  readout (from live status if available).
- **Refresh** to re-scan the folder.

### Data sources
- Folder listing: **`GET /api/v1/media?node={id}`** (or scoped via
  **`GET /api/v1/groups/{id}/media`**) — lists `data/` mp3s of the relevant **master**
  node (proxied to that node).
- Select + play: **`POST /api/v1/groups/{id}/media`** `{file, loop}` writes
  `Groups[].media` (§6.5) and starts playback (setting the group's **`Playing`** bool);
  **Stop** via **`POST /api/v1/groups/{id}/stop`**. Selection persists in the ConfigDoc so
  Dashboard/Groups reflect it.
- Now-playing/position: **`GET /api/v1/groups/{id}/status`**.
- All media commands **proxy** to the group **master**.

### States
- **loading**: list skeleton; commands disabled.
- **empty**: `data/` has no mp3s → "No media in data/ on `<master>` — drop mp3s into the
  node's `data/` folder" (the folder is per-node, §5).
- **error**: scan/playback failure → envelope `code`/`message` + Retry.
- **offline**: scoped master offline → list shows **last-selected** media from ConfigDoc
  (dimmed) and disables browse/play with an offline banner.
- **uninitialized**: gated.

### User flows
- Pick scope (group) → browse the master's `data/` → **Select & play** a file (toggle
  **loop**) → master decodes once and unicasts to listeners ([D5]); **Now-playing**
  updates → **Stop** to end. Selection is remembered on the group record.

---

## 8. Settings

### Purpose
Cluster-level administration: **change admin password**, manage **API keys**
(list/create/revoke), view **cluster info + CA fingerprint**, and the dangerous
**leave / reset cluster** action. Auth model is [D11](./README.md#3-locked-decisions-decision-log);
trust/PKI details in [03](./03-adoption-takeover-security-pki.md).

### Wireframe
```
┌─ Settings ───────────────────────────────────────────────────────────────────┐
│                                                                                │
│  Admin password                                                                │
│   current [ •••••••• ]  new [ •••••••• ]  confirm [ •••••••• ]   [ Update ]     │
│                                                                                │
│  API keys                                                          [ + Create ]│
│   ┌ name ───────────┬ created ────┬ last used ──┬ ─────────────┐               │
│   │ home-assistant  │ 2026-04-01  │ 2026-06-05  │ [ Revoke ]    │               │
│   │ grafana-poller  │ 2026-05-12  │  —          │ [ Revoke ]    │               │
│   └─────────────────┴─────────────┴─────────────┴───────────────┘               │
│   (new key shown once on creation — copy it now)                               │
│                                                                                │
│  Cluster                                                                       │
│   name  Living Room        created  2026-03-18      nodes  4 (3 online)         │
│   CA fingerprint  SHA256:1f:aa:…:9c                              [ copy ]        │
│                                                                                │
│  ⚠ Danger zone                                                                  │
│   [ Leave / reset this cluster ]   removes this node's identity & cluster state │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

### Components
- **Change admin password**: current + new + confirm → updates the argon2id hash in
  `Auth` (ConfigDoc §6.5). Single admin (D11).
- **API keys**: table of keys (name, created, last-used) with **Revoke**; **Create**
  generates a key whose **plaintext is shown exactly once** (only hashes are stored,
  §6.5 `Auth.apiKeyHashes`).
- **Cluster info**: name, created, node count (online/total), and **CA fingerprint**
  (from `Cluster.CACert`) with copy — same anchor shown on **Cluster** (screen 4).
- **Danger zone**: **Leave / reset cluster** — a destructive action behind a typed
  confirm; resets this node toward **uninitialized** (and, for a multi-node cluster,
  is effectively a self-**Forget**). After completion the node returns to the **Setup
  Wizard**.

### Data sources
- Change password: **`POST /api/v1/auth/password`** `{current,new}`, config write with
  `If-Match` (on the config `version`).
- API keys: **`GET /api/v1/auth/keys`**, **`POST /api/v1/auth/keys`** (returns plaintext
  once), **`DELETE /api/v1/auth/keys/{id}`**.
- Cluster info + CA fingerprint: ConfigDoc `Cluster` (**`GET /api/v1/cluster/info`**).
- Leave/reset (danger zone): **`POST /api/v1/cluster/leave`** — coordinated self-forget
  (over mTLS the node asks the cluster to add its cert fingerprint to `RevokedSet` + drop
  its `NodeRecord`, gossiped, then wipes its own certs/identity/config and reboots into the
  setup wizard; if unreachable, wipes locally anyway). Per [03] (Forget semantics).

### States
- **loading**: section skeletons.
- **empty**: no API keys → "No API keys — create one for programmatic access".
- **error**: wrong current password on change → field error; key create/revoke errors →
  envelope; `409` on writes → reload + reapply.
- **offline**: cluster-wide info still renders from gossiped ConfigDoc; **leave/reset**
  remains possible locally (it tears down *this* node), but a clean coordinated revoke
  needs reachable peers — warn if offline.
- **uninitialized**: gated (you reach Settings only when initialized).

### User flows
1. **Change password** → re-auth may be required (existing sessions per [03] policy).
2. **Create API key** → name it → **copy the shown plaintext once** → use for headless
   clients (API-key auth path, §6.6/D11). **Revoke** invalidates immediately.
3. **Leave / reset** → typed confirmation → node tears down identity → returns to
   **Setup Wizard** (screen 1); other nodes drop it from membership/allowlist via [03].

---

## Cross-reference summary

- **Adoption / PIN / Takeover / Forget / auth / CA / threat model** → [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md)
  (screens 1b, 2, 4, 8).
- **Calibration (sync-test clip + measured offset), channel role, `HWDelayUs`, gain,
  drift** → [06-audio-output-scheduling.md](./06-audio-output-scheduling.md) (screen 6;
  live sync err on screens 3 & 6). These controls are **shown only when `Caps.Render ==
  true`**; sink-less nodes hide them (screen 6 variant B).
- **Capabilities & audio backends** (effective `Caps` = runtime-probed ∩ config-enabled,
  per-node disable of backend/codec/FEC, force `render:false`) → [D16](./README.md#3-locked-decisions-decision-log)
  / [D17](./README.md#3-locked-decisions-decision-log) / [D12](./README.md#3-locked-decisions-decision-log),
  [README §6.5](./README.md#65-config-document--internalstate), per-node config write →
  [07-config-and-replication.md](./07-config-and-replication.md) (screen 6). **Sink-less
  masters** ("master — no local audio") surface on screens 3 (Dashboard), 4 (Cluster), 5
  (Groups).
- **Every endpoint cited** → [08-http-api-reference.md](./08-http-api-reference.md)
  (all screens; 08 is **authoritative** on wire details — paths here use its canonical
  namespaced scheme).
- **ConfigDoc shapes** (Nodes/Groups/Auth/Cluster, allowlist derivation) → [README §6.5](./README.md#65-config-document--internalstate)
  and [07-config-and-replication.md](./07-config-and-replication.md).
- **Profile negotiation / master election / timeline** → [04-clock-and-groups.md](./04-clock-and-groups.md).
- **Live sync metrics** for screens 3 & 6 come from **`GET /api/v1/groups/{id}/status`**.
