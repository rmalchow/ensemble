# 08 — HTTP API reference

> **Scope.** Every HTTP endpoint Ensemble nodes expose: bootstrap, setup/auth,
> cluster ops, node config, groups, media, and status. This document elaborates the
> conventions locked in [README.md §6.6](./README.md) and the auth/adoption model in
> [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md). It
> defines request/response shapes that build on the `ConfigDoc` schema
> ([README.md §6.5](./README.md), full detail in
> [07-config-and-replication.md](./07-config-and-replication.md)) and notes which UI
> screen drives each call ([09-ui-screens.md](./09-ui-screens.md)).
>
> **Do not redefine** the canonical names from the spine; this doc only *uses* them.

---

## 0. Conventions (recap of [README.md §6.6](./README.md))

These hold for **every** endpoint below unless an endpoint explicitly overrides them.

### 0.1 Base path & versioning
- All cluster API lives under **`/api/v1`**.
- The **bootstrap** surface (§A) is the sole exception: it lives at **`/bootstrap/*`**,
  *outside* `/api/v1` and *outside* mTLS, because it is served by a node that does not
  yet have a cluster cert. See [03 §adoption](./03-adoption-takeover-security-pki.md).

### 0.2 Content type
- JSON only. Requests with a body MUST send `Content-Type: application/json`.
- Responses are `application/json` except binary/stream-free downloads (none here);
  empty success bodies use `204 No Content`.

### 0.3 Authentication classes
Every endpoint is tagged with exactly one **auth class**:

| Tag | Meaning | Mechanism |
|---|---|---|
| **public-bootstrap** | Reachable before the node is initialized/adopted; no cluster identity exists yet. | Plain TLS (node self-signed) + adoption **PIN** challenge (D9). No mTLS, no session. |
| **mTLS-node** | Node-to-node control plane. | mTLS client cert signed by the cluster CA (D10). Used by **cross-node proxying** and gossip-adjacent control. |
| **admin-session** | Browser/operator path. | Cluster **admin password** login → `Set-Cookie` session (D11). |
| **api-key** | Programmatic operator path. | `Authorization: Bearer <api-key>` header (D11). |

Per [README.md §6.6](./README.md): an `/api/v1` endpoint is satisfied by **mTLS client
cert (node)** *OR* a valid **admin session / API key**. Where a call is restricted
further (e.g. setup must be unauthenticated-but-uninitialized), it is called out
explicitly. Full auth/threat detail: [03 §UI/API auth](./03-adoption-takeover-security-pki.md).

### 0.4 Error envelope
All non-2xx responses use the locked envelope:
```json
{ "error": { "code": "string", "message": "human readable" } }
```
with a matching HTTP status. Common `code` values used throughout:

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `invalid_request` | Malformed JSON / failed validation. |
| 401 | `unauthenticated` | No/invalid session, API key, mTLS cert, or PIN. |
| 403 | `forbidden` | Authenticated but not permitted (e.g. PIN ok but node already adopted without takeover). |
| 404 | `not_found` | Unknown node/group/media id. |
| 409 | `version_conflict` | `If-Match` did not match current `ConfigDoc.Version`. |
| 409 | `conflict` | State conflict other than version (e.g. node already a member). |
| 412 | `precondition_required` | Config-mutating call sent without `If-Match`. |
| 422 | `unprocessable` | Semantically invalid (e.g. `channel:"middle"`, member in another group). |
| 502 | `proxy_failed` | Cross-node proxy could not reach / was rejected by the target node. |
| 503 | `not_ready` | Node uninitialized for a call that needs a cluster, or no sync yet. |

### 0.5 Optimistic concurrency (`If-Match`)
- Every **config-mutating** call (anything that writes the replicated `ConfigDoc`)
  REQUIRES `If-Match: <version>`, where `<version>` is the integer `ConfigDoc.Version`
  the client last read.
- On match: the node applies the change, bumps `Version`, sets `UpdatedBy` to the
  applying node id, gossips the new doc (LWW per
  [07](./07-config-and-replication.md)), and returns the **new** version both in the
  response body (`version`) and an `ETag: <newVersion>` header.
- On mismatch: `409 version_conflict`. The client should re-`GET`, rebase, retry.
- Missing header on a mutating call: `412 precondition_required`.
- Read-only calls return the current version via `ETag` so the client can seed
  `If-Match` for a follow-up write.

Each endpoint below states **If-Match: required / not used**.

### 0.6 Cross-node proxy / fan-out (locked in [README.md §6.6](./README.md))
> *"Any node serves the full API; cross-node operations are proxied node→node over mTLS."*

- **Local** — handled entirely by the receiving node.
- **Proxied (single target)** — the receiving node forwards the request to the one
  owning node over **mTLS-node**, returns its response verbatim (including `ETag`).
  Used when a write must persist on a *specific* node (e.g. a node's `hwDelayUs`).
- **Fan-out** — the receiving node performs the op across **multiple** peers over
  mTLS (e.g. a group transport command reaching every member-master). Partial failure
  is reported in the body; see each endpoint.

Each endpoint states its proxy behavior: **local / proxied / fan-out**.

### 0.7 Common objects
Referenced repeatedly; canonical schema in [README.md §6.5](./README.md) /
[07](./07-config-and-replication.md).

```jsonc
// NodeRecord (public projection — cert PEM is public trust material)
{
  "id": "n-7a3f",
  "name": "Living Room",
  "addrs": ["192.168.1.21", "fe80::1"],
  "hwDelayUs": 1200,
  "channel": "left",            // "stereo" | "left" | "right"
  "gainDb": -2.5,
  // caps is the structured Capabilities object (README §6.5), NOT a flat string array.
  // Codec/FEC values are string enums in JSON (wire ints exist only at §6.4).
  "caps": {
    "render": true,                       // false => control/media-only node (no audio sink)
    "sinks": ["alsa", "exec:aplay"],      // usable+enabled output backends
    "encode": ["pcm", "opus"],            // codecs this node can ORIGINATE (as master)
    "decode": ["pcm", "opus"],            // codecs this node can PLAY (as listener)
    "fec": ["none", "xorParity", "duplicate"],
    "maxRate": 48000
  }
}

// GroupRecord
{
  "id": "g-kitchen",
  "name": "Kitchen + Bath",
  "memberNodeIds": ["n-7a3f", "n-91c2"],
  // profile uses STRING enums for codec/fec (wire ints only at §6.4) and exposes
  // the read-mostly transport params framesPerChunk/fecK/interleave.
  "profile": {
    "codec": "pcm",               // "pcm" | "opus"
    "fec": "xorParity",           // "none" | "xorParity" | "duplicate"
    "rate": 48000,
    "framesPerChunk": 480,
    "fecK": 8,
    "interleave": 4
  },
  "media": { "file": "song.mp3", "loop": true },
  "playing": true   // replicated GroupRecord.Playing (R4b); play/stop flips it,
                    // a new master reads it and resumes. There is no separate transport.state.
}
```

---

# A. BOOTSTRAP — `/bootstrap/*`  (outside mTLS, PIN-gated)

Served by an **uninitialized** node (no cluster cert yet). This is the *only* surface
reachable before adoption. Full protocol & threat model:
[03 §adoption](./03-adoption-takeover-security-pki.md). UI: the **Setup Wizard** /
**Cluster → Discover** screens ([09](./09-ui-screens.md)).

---

### A.1 `GET /bootstrap/info`
- **Auth:** public-bootstrap (no PIN required — this is the unauthenticated probe).
- **Purpose:** Let a controller (or operator’s browser) identify an unadopted node:
  its node id, its self-signed cert **fingerprint** (to pin before sending the PIN),
  and its init state. Used to populate **Cluster → Discovery** rows for raw nodes.
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "nodeId": "n-7a3f",
    "name": "ensemble-7a3f",
    "fingerprint": "sha256:9f86d081884c7d65...",
    "state": "uninitialized",
    "softwareVersion": "0.1.0",
    "protocolEpoch": 1,
    "caps": {
      "render": true,
      "sinks": ["alsa", "exec:aplay"],
      "encode": ["pcm", "opus"],
      "decode": ["pcm", "opus"],
      "fec": ["none", "xorParity", "duplicate"],
      "maxRate": 48000
    }
  }
  ```
  `caps` is the structured `Capabilities` object (README §6.5), **not** a flat string
  array. `state` ∈ `"uninitialized"` (never adopted) · `"foreign"` (belongs to another
  cluster — needs **takeover**, see C.4) · `"member"` (already adopted; bootstrap is
  closed and returns `403 forbidden`).
- **Versioning (m7):** `softwareVersion` is **informational only** (UI visibility). The
  load-bearing compatibility check is the **protocol epoch**: all nodes in a cluster run
  the **same epoch** (mixed epochs unsupported). Adoption (A.2 / C.3) **refuses** a
  protocol-epoch mismatch — upgrade nodes together.
- **Status codes:** `200`; `403 forbidden` if already a healthy member.
- **If-Match:** not used (read-only, no ConfigDoc).
- **Proxy:** local (the target node answers about itself). When surfaced via the
  controller’s `GET /discovery` (C.2), the controller fetches this from each raw node.

---

### A.2 `POST /bootstrap/adopt`
- **Auth:** public-bootstrap + **PIN challenge-response** (D9). The PIN (`"0000"`
  placeholder, treated as a real secret) gates this call. Exact challenge-response
  framing is owned by [03 §adoption PIN exchange](./03-adoption-takeover-security-pki.md);
  the body below carries its fields.
- **Purpose:** The adopting controller sends the node a **CSR**; the cluster CA (on the
  controller side) signs it and returns the **signed node cert + CA bundle**, plus the
  cluster identity the node needs to come up as a member. After this the node closes
  `/bootstrap/*` and serves `/api/v1` under mTLS.
- **Request body:**
  ```json
  {
    "pinProof": "hmac-or-challenge-response-per-03",
    "clusterName": "home",
    "caBundlePem": "-----BEGIN CERTIFICATE-----\n...CA...\n-----END CERTIFICATE-----",
    "signedCertPem": "-----BEGIN CERTIFICATE-----\n...node leaf...\n-----END CERTIFICATE-----",
    "assignedNodeId": "n-7a3f",
    "seedPeers": ["192.168.1.10:7946"]
  }
  ```
  In the controller-initiated flow (see C.3) the node’s CSR is collected over this same
  channel; in the symmetric framing the node POSTs its CSR and receives the signed
  artifacts. The fields above are the **post-sign** artifacts the node persists. (CSR
  carrier field `csrPem` is present in the node→controller direction; see
  [03](./03-adoption-takeover-security-pki.md) for the exact two-message exchange.)
- **Response `200`:**
  ```json
  {
    "nodeId": "n-7a3f",
    "state": "member",
    "joinedClusterName": "home"
  }
  ```
- **Status codes:** `200`; `401 unauthenticated` (bad PIN proof); `403 forbidden`
  (already a member of a *different* cluster — requires **takeover**, C.4);
  `400 invalid_request` (malformed CSR / cert); `422 unprocessable` (cert chain does
  not verify against the supplied CA bundle, **or a protocol-epoch mismatch** between the
  adopting cluster and this node — upgrade together, m7).
- **If-Match:** not used. The *controller* side records the new node into the
  `ConfigDoc` via the adopt flow (C.3), which is where `If-Match` applies.
- **Proxy:** local on the node being adopted. The CA signing happens on the
  **controller** node; see C.3 `POST /adopt` for the cluster-side counterpart.

---

# B. SETUP & AUTH — `/api/v1/...`

UI: **Setup Wizard** (first init) and **Settings** ([09](./09-ui-screens.md)).
Auth model detail: [03 §UI/API auth](./03-adoption-takeover-security-pki.md).

---

### B.1 `POST /api/v1/setup`
- **Auth:** **public-bootstrap-equivalent, gated on uninitialized state.** Reachable
  only while this node has no cluster (no CA, no admin password). Once a cluster
  exists, returns `409 conflict`. No session/cert/PIN required — this is how the
  *very first* node becomes a cluster.
- **Purpose:** First-init. Creates the **cluster CA**, sets the **admin password**, and
  makes this node the founding **member** (it self-signs its own leaf from the new CA
  and writes the genesis `ConfigDoc`, `Version = 1`).
- **Request body:**
  ```json
  {
    "clusterName": "home",
    "adminPassword": "correct horse battery staple",
    "nodeName": "Living Room"
  }
  ```
- **Response `200`:**
  ```json
  {
    "cluster": { "name": "home", "caFingerprint": "sha256:1b2c3d...", "created": "2026-06-05T10:00:00Z" },
    "node":    { "id": "n-7a3f", "name": "Living Room" },
    "version": 1
  }
  ```
  Also sets a session cookie (the operator is logged in immediately) and returns
  `ETag: 1`.
- **Status codes:** `200`; `409 conflict` (already initialized); `400 invalid_request`
  (missing/weak fields per [03](./03-adoption-takeover-security-pki.md) password policy);
  `422 unprocessable` (empty cluster name).
- **If-Match:** **not used** (genesis write — there is no prior version).
- **Proxy:** local only. Setup never proxies.

---

### B.2 `POST /api/v1/auth/login`
- **Auth:** public within an initialized cluster (it *is* the authentication call);
  body carries the credential.
- **Purpose:** Exchange the admin password for a browser **session** cookie.
- **Request body:** `{ "password": "correct horse battery staple" }`
- **Response `200`:**
  ```json
  { "session": { "expiresAt": "2026-06-06T10:00:00Z" } }
  ```
  with `Set-Cookie: ensemble_session=...; HttpOnly; Secure; SameSite=Strict`.
- **Status codes:** `200`; `401 unauthenticated` (wrong password);
  `429`-class throttling per [03](./03-adoption-takeover-security-pki.md) brute-force
  guard (envelope `code:"rate_limited"`); `503 not_ready` (node uninitialized).
- **If-Match:** not used.
- **Proxy:** local. Session validity is per-node; cross-node calls re-auth via mTLS or
  API key, not by forwarding cookies.

---

### B.3 `POST /api/v1/auth/logout`
- **Auth:** admin-session.
- **Purpose:** Invalidate the current session.
- **Request body:** none.
- **Response:** `204 No Content`; clears the session cookie.
- **Status codes:** `204`; `401 unauthenticated` (no session).
- **If-Match:** not used. **Proxy:** local.

---

### B.3a `POST /api/v1/auth/password`
- **Auth:** admin-session (the interactive admin changes their own password).
- **Purpose:** Change the **cluster admin password**. Writes the new hash to
  `ConfigDoc.Auth` (D11). Existing sessions/API keys are unaffected unless the body
  requests revocation.
- **Request body:**
  ```json
  {
    "currentPassword": "correct horse battery staple",
    "newPassword": "a much longer and stronger passphrase here"
  }
  ```
- **Response `200`:** `{ "version": 47 }` with `ETag: 47`.
- **Status codes:** `200`; `401 unauthenticated` (no session **or** wrong
  `currentPassword`); `400 invalid_request` (new password fails the
  [03](./03-adoption-takeover-security-pki.md) password policy); `409 version_conflict`;
  `412 precondition_required`.
- **If-Match:** **required** (mutates `ConfigDoc.Auth` — the admin password hash, on the
  cluster config **version**).
- **Proxy:** local write → gossip to all full nodes.

---

### B.4 `GET /api/v1/auth/session`
- **Auth:** admin-session **or** api-key.
- **Purpose:** "Who am I / am I still authenticated"; drives UI auth-guard and the
  Settings header. Returns the caller’s identity and the current `ConfigDoc.Version`
  so the UI can seed `If-Match`.
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "authenticated": true,
    "method": "session",          // "session" | "apiKey" | "node"
    "nodeId": "n-7a3f",
    "configVersion": 42
  }
  ```
  Also returns `ETag: 42`.
- **Status codes:** `200`; `401 unauthenticated`.
- **If-Match:** not used (read-only). **Proxy:** local.

---

### B.5 `GET /api/v1/auth/keys`
- **Auth:** admin-session (managing keys requires the interactive admin).
- **Purpose:** List existing API keys (metadata only — never the secret). UI: **Settings → API keys**.
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "version": 42,
    "keys": [
      { "id": "k-1", "label": "home-assistant", "createdAt": "2026-05-01T12:00:00Z", "lastUsedAt": "2026-06-04T22:10:00Z" }
    ]
  }
  ```
  `ETag: 42`. Keys are stored as hashes in `ConfigDoc.Auth`; only metadata is returned.
- **Status codes:** `200`; `401 unauthenticated`.
- **If-Match:** not used (read-only). **Proxy:** local (reads from the replicated doc).

---

### B.6 `POST /api/v1/auth/keys`
- **Auth:** admin-session.
- **Purpose:** Mint a new API key. The plaintext secret is returned **exactly once**;
  only its hash is stored in `ConfigDoc.Auth` (D11).
- **Request body:** `{ "label": "home-assistant" }`
- **Response `201`:**
  ```json
  {
    "version": 43,
    "key": { "id": "k-2", "label": "home-assistant", "secret": "ek_live_8f3a...ONCE", "createdAt": "2026-06-05T10:05:00Z" }
  }
  ```
  `ETag: 43`.
- **Status codes:** `201`; `400 invalid_request` (empty label);
  `409 version_conflict`; `412 precondition_required`; `401 unauthenticated`.
- **If-Match:** **required** (mutates `ConfigDoc.Auth`).
- **Proxy:** local write, then gossiped to all full nodes ([07](./07-config-and-replication.md)).

---

### B.7 `DELETE /api/v1/auth/keys/{id}`
- **Auth:** admin-session.
- **Purpose:** Revoke an API key (drops its hash from `ConfigDoc.Auth`).
- **Request body:** none.
- **Response:** `200` `{ "version": 44 }` with `ETag: 44` (or `204` with `ETag`).
- **Status codes:** `200`/`204`; `404 not_found`; `409 version_conflict`;
  `412 precondition_required`; `401 unauthenticated`.
- **If-Match:** **required**. **Proxy:** local write → gossip.

---

# C. CLUSTER — `/api/v1/cluster`, `/api/v1/discovery`, adoption ops

UI: **Cluster** screen (discover / adopt / takeover / forget) ([09](./09-ui-screens.md)).
Trust/PKI mechanics: [03](./03-adoption-takeover-security-pki.md).

---

### C.1 `GET /api/v1/cluster/info`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Cluster identity + CA fingerprint + summary counts. Drives the Cluster
  screen header and **Settings → Cluster info**.
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "version": 42,
    "cluster": {
      "name": "home",
      "caFingerprint": "sha256:1b2c3d...",
      "created": "2026-05-01T12:00:00Z"
    },
    "counts": { "nodes": 4, "groups": 2 }
  }
  ```
  `ETag: 42`. CA cert (public) lives in `ConfigDoc.Cluster`; the fingerprint here lets
  an operator pin it before adopting raw nodes (A.1).
- **Status codes:** `200`; `401 unauthenticated`; `503 not_ready` (uninitialized).
- **If-Match:** not used (read-only). **Proxy:** local (read from replicated doc).

---

### C.2 `GET /api/v1/discovery`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Live discovery snapshot — every node seen on the LAN via broadcast/mDNS
  ([02](./02-cluster-discovery-membership.md)), **including uninitialized** ones not yet
  in the cluster. Drives **Cluster → Discover**.
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "members": [
      { "id": "n-7a3f", "name": "Living Room", "addrs": ["192.168.1.21"], "state": "member", "online": true }
    ],
    "discovered": [
      {
        "nodeId": "n-9b1d",
        "name": "ensemble-9b1d",
        "addrs": ["192.168.1.55"],
        "fingerprint": "sha256:aa11bb22...",
        "state": "uninitialized",      // or "foreign"
        "softwareVersion": "0.1.0"
      }
    ]
  }
  ```
  The `discovered[]` entries are the controller’s aggregation of each raw node’s
  `GET /bootstrap/info` (A.1).
- **Status codes:** `200`; `401 unauthenticated`.
- **If-Match:** not used (read-only).
- **Proxy:** **fan-out (read)** — the controller may probe `GET /bootstrap/info` on each
  raw node to enrich `discovered[]`; member liveness comes from gossip (local).

---

### C.3 `POST /api/v1/cluster/adopt`
- **Auth:** admin-session / api-key (operator-initiated) — runs on a **controller**
  node that holds the CA.
- **Purpose:** Controller-initiated adoption of a **discovered, uninitialized** node:
  the controller collects the target’s CSR over `/bootstrap/*`, signs it with the
  cluster CA (D9, gated by the supplied **PIN**), pushes back the signed cert + CA
  bundle (A.2), then **records the new `NodeRecord`** into the `ConfigDoc`. See
  [03 §adoption](./03-adoption-takeover-security-pki.md) for the full exchange.
- **Request body:**
  ```json
  {
    "nodeId": "n-9b1d",
    "addr": "192.168.1.55",
    "fingerprint": "sha256:aa11bb22...",   // pin the target’s self-signed cert
    "pin": "0000",
    "name": "Bedroom"                        // optional initial display name
  }
  ```
- **Response `200`:**
  ```json
  {
    "version": 45,
    "node": {
      "id": "n-9b1d", "name": "Bedroom", "addrs": ["192.168.1.55"],
      "channel": "stereo", "hwDelayUs": 0, "gainDb": 0,
      "caps": {
        "render": true, "sinks": ["alsa"],
        "encode": ["pcm"], "decode": ["pcm"],
        "fec": ["none", "xorParity"], "maxRate": 48000
      }
    }
  }
  ```
  `ETag: 45`. `caps` is the structured `Capabilities` object (README §6.5). Adding the
  node’s `Addrs` updates the **allowlist**
  ([README.md §6.5](./README.md) / [07](./07-config-and-replication.md)).
- **Status codes:** `200`; `401 unauthenticated` (operator) / `401` (bad PIN to target,
  surfaced as `proxy_failed` detail); `403 forbidden` (target already a member of a
  different cluster → use **takeover**, C.4); `404 not_found` (target not discoverable);
  `409 version_conflict` / `conflict` (already a member); `412 precondition_required`;
  `422 unprocessable` (fingerprint mismatch, **or protocol-epoch mismatch** — m7;
  upgrade together); `502 proxy_failed` (target unreachable).
- **If-Match:** **required** (adds a `NodeRecord` → mutates `ConfigDoc`).
- **Proxy:** **proxied to the target** over the bootstrap channel for the CSR/sign
  exchange (A.2), **then** local config write → gossip to all full nodes.

---

### C.4 `POST /api/v1/cluster/takeover`
- **Auth:** admin-session / api-key.
- **Purpose:** Re-adopt a node that already belongs to **another/old cluster** — forced
  re-issue of identity. Same as adopt but overrides the target’s existing membership
  (D9/[03 §takeover](./03-adoption-takeover-security-pki.md)). Still PIN-gated.
- **Request body:** identical shape to C.3 plus an explicit override flag:
  ```json
  {
    "nodeId": "n-9b1d",
    "addr": "192.168.1.55",
    "fingerprint": "sha256:aa11bb22...",
    "pin": "0000",
    "name": "Bedroom",
    "force": true
  }
  ```
- **Response `200`:** same shape as C.3 (`version`, `node`).
- **Status codes:** `200`; `401`; `404 not_found`; `409 version_conflict`;
  `412 precondition_required`; `422 unprocessable` (fingerprint mismatch);
  `502 proxy_failed`.
- **If-Match:** **required**.
- **Proxy:** **proxied to the target** (forced re-issue of cert) → local config write →
  gossip.

---

### C.5 `POST /api/v1/nodes/{id}/forget`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Remove a node from the cluster: revoke its cert, drop its `NodeRecord`
  from the `ConfigDoc`, remove its addrs from the **allowlist**, and pull it from any
  group membership ([03 §forget](./03-adoption-takeover-security-pki.md)).
- **Request body:** none (or optional `{ "reason": "decommissioned" }`).
- **Response `200`:**
  ```json
  { "version": 46, "removedNodeId": "n-9b1d", "affectedGroups": ["g-kitchen"] }
  ```
  `ETag: 46`.
- **Status codes:** `200`; `404 not_found`; `409 version_conflict` / `conflict`
  (cannot forget the last/only node, or a sole group master mid-play — see
  [04](./04-clock-and-groups.md)); `412 precondition_required`; `401 unauthenticated`.
- **If-Match:** **required** (removes `NodeRecord` + edits `Groups`).
- **Proxy:** local config write → gossip; best-effort revoke notification to the target
  is **proxied** over mTLS (the target may already be gone — failure is non-fatal and
  reported as `affectedGroups`/warnings, not an error).

---

### C.6 `POST /api/v1/cluster/leave`
- **Auth:** mTLS-node / admin-session / api-key. This is the node **self-forgetting** —
  the complement to C.5 (`/nodes/{id}/forget`, where the *cluster* removes a different
  node). It runs on the node that is leaving.
- **Purpose:** **Coordinated self-forget.** The leaving node asks the cluster (over
  **mTLS**) to add its own cert **fingerprint** to the grow-only `RevokedSet` and drop
  its `NodeRecord` from the `ConfigDoc` (gossiped, monotonic-union merge per
  [README §6.5](./README.md) / [07](./07-config-and-replication.md)). Once the cluster
  acknowledges, the node **wipes its own certs / identity / config locally** and reboots
  into the **Setup Wizard** (state `"uninitialized"`).
  - **Unreachable-cluster fallback:** if the node cannot reach any peer to coordinate the
    revoke + record drop, it **wipes locally anyway** and the operator must forget it
    from the cluster side via C.5. This keeps a decommissioned node from coming back up
    as a member.
- **Request body:** none (or optional `{ "reason": "decommissioned" }`).
- **Response `200`:**
  ```json
  { "version": 47, "leftNodeId": "n-7a3f", "coordinated": true, "affectedGroups": ["g-kitchen"] }
  ```
  `ETag: 47` (the version *after* the cluster dropped the record). `coordinated:false`
  signals the unreachable-cluster fallback path — the local wipe happened but the cluster
  was not updated by this node.
- **Status codes:** `200`; `401 unauthenticated`; `409 conflict` (cannot leave as the
  last/only node, or a sole group master mid-play — see [04](./04-clock-and-groups.md));
  `412 precondition_required`; `502 proxy_failed` (no peer reachable — the node still
  wipes locally and returns `coordinated:false` rather than a hard error where possible).
- **If-Match:** **required** for the coordinated path (removes its own `NodeRecord` +
  edits `Groups` on the replicated doc). Not applicable to the local-only wipe fallback.
- **Proxy:** **proxied** — the leaving node drives the cluster-side revoke + record drop
  over mTLS (fingerprint → `RevokedSet`, drop `NodeRecord` → gossip), **then** performs
  the local identity/config wipe.

---

# D. NODES — `/api/v1/nodes`

UI: **Dashboard** (node tiles) and **Node detail** (any node) ([09](./09-ui-screens.md)).
Field semantics (channel/hwDelayUs/gainDb) live in
[06-audio-output-scheduling.md](./06-audio-output-scheduling.md); persistence/merge in
[07](./07-config-and-replication.md).

---

### D.1 `GET /api/v1/nodes`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** List all member nodes (the `NodeRecord` projection from §0.7). Drives the
  Dashboard node grid.
- **Request body:** none.
- **Response `200`:**
  ```json
  { "version": 42, "nodes": [ { /* NodeRecord */ }, { /* NodeRecord */ } ] }
  ```
  `ETag: 42`.
- **Status codes:** `200`; `401 unauthenticated`.
- **If-Match:** not used (read-only). **Proxy:** local (read from replicated doc).

---

### D.2 `GET /api/v1/nodes/{id}`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** One node’s full record. Drives **Node detail**.
- **Request body:** none.
- **Response `200`:** `{ "version": 42, "node": { /* NodeRecord */ } }`, `ETag: 42`.
- **Status codes:** `200`; `404 not_found`; `401 unauthenticated`.
- **If-Match:** not used (read-only).
- **Proxy:** local — the record is in the replicated doc, so *any* node can answer.
  (Live runtime metrics for the node come from `GET /api/v1/status`, G.1, which **is**
  node-specific.)

---

### D.3 `PATCH /api/v1/nodes/{id}`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Update a node’s **identity/audio config**: display `name`, `channel`
  (`stereo|left|right`), `hwDelayUs` (hardware/output latency trim), `gainDb`. These
  map to `NodeRecord` fields (D13/[06](./06-audio-output-scheduling.md)). Drives **Node
  detail** edits.
- **Request body** (all fields optional; partial update):
  ```json
  {
    "name": "Bedroom",
    "channel": "right",
    "hwDelayUs": 1500,
    "gainDb": -1.5
  }
  ```
- **Response `200`:** `{ "version": 47, "node": { /* updated NodeRecord */ } }`, `ETag: 47`.
- **Status codes:** `200`; `404 not_found`; `422 unprocessable`
  (`channel` not in `stereo|left|right`; `hwDelayUs` out of sane range);
  `409 version_conflict`; `412 precondition_required`; `401 unauthenticated`;
  `502 proxy_failed` (owning node unreachable).
- **If-Match:** **required** (mutates `ConfigDoc.Nodes`).
- **Proxy:** **proxied to the owning node** `{id}` over mTLS. The owning node applies the
  change so its live renderer picks up `channel`/`hwDelayUs`/`gainDb` immediately
  ([06](./06-audio-output-scheduling.md)), **persists** to its local copy, and the new
  `ConfigDoc` version **gossips to all full nodes**
  ([07](./07-config-and-replication.md)). A non-owning node that receives this `PATCH`
  forwards it; it does not write locally first.

---

# E. GROUPS — `/api/v1/groups`

UI: **Groups** screen (create / assign members / profile / media transport)
([09](./09-ui-screens.md)). Group engine, election, profile negotiation:
[04-clock-and-groups.md](./04-clock-and-groups.md).

---

### E.1 `GET /api/v1/groups`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** List all groups (`GroupRecord` from §0.7). Drives Dashboard + Groups list.
- **Request body:** none.
- **Response `200`:** `{ "version": 42, "groups": [ { /* GroupRecord */ } ] }`, `ETag: 42`.
- **Status codes:** `200`; `401 unauthenticated`.
- **If-Match:** not used (read-only). **Proxy:** local.

---

### E.2 `POST /api/v1/groups`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Create a group. Members are moved into it (a node is in **exactly one**
  group — [README.md §2](./README.md)); the profile is negotiated to the least-capable
  member ([04](./04-clock-and-groups.md)) unless overridden.
- **Request body:**
  ```json
  {
    "name": "Kitchen + Bath",
    "memberNodeIds": ["n-7a3f", "n-91c2"],
    "profile": {
      "codec": "pcm",
      "fec": "xorParity",
      "rate": 48000,
      "framesPerChunk": 480,
      "fecK": 8,
      "interleave": 4
    },
    "playing": false
  }
  ```
  `profile` is optional (auto-negotiated if omitted); `codec`/`fec` are **string enums**
  (`"pcm"|"opus"`, `"none"|"xorParity"|"duplicate"`) and `framesPerChunk/fecK/interleave`
  are read-mostly transport params (R4). `playing` defaults to `false` if omitted.
- **Response `201`:** `{ "version": 48, "group": { /* GroupRecord */ } }`, `ETag: 48`.
- **Status codes:** `201`; `400 invalid_request`; `404 not_found` (unknown member);
  `422 unprocessable` (member already in another group and no `reassign` semantics;
  unsupported codec for a member’s `caps`); `409 version_conflict`;
  `412 precondition_required`; `401 unauthenticated`.
- **If-Match:** **required** (adds a `GroupRecord`, edits members’ group membership).
- **Proxy:** local config write → gossip. Member nodes pick up their new group role
  (master election etc.) from the gossiped doc; no direct fan-out needed at create time.

---

### E.3 `GET /api/v1/groups/{id}`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** One group’s full record. Drives the Groups detail pane.
- **Request body:** none.
- **Response `200`:** `{ "version": 42, "group": { /* GroupRecord */ } }`, `ETag: 42`.
- **Status codes:** `200`; `404 not_found`; `401 unauthenticated`.
- **If-Match:** not used (read-only). **Proxy:** local.
  (Live sync metrics are at `GET /api/v1/groups/{id}/status`, G.2.)

---

### E.4 `PATCH /api/v1/groups/{id}`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Edit a group: `memberNodeIds[]` (add/remove members),
  `profile` overrides (codec/fec/rate/`framesPerChunk`/`fecK`/`interleave`), and the
  `playing` flag. Adding/removing members triggers re-election + allowlist recompute
  ([04](./04-clock-and-groups.md), [07](./07-config-and-replication.md)).
- **Request body** (all optional; partial):
  ```json
  {
    "name": "Kitchen",
    "memberNodeIds": ["n-7a3f"],
    "profile": { "codec": "opus", "fec": "duplicate", "rate": 48000, "framesPerChunk": 480, "fecK": 8, "interleave": 4 },
    "playing": true
  }
  ```
  `codec`/`fec` are string enums (R4). Setting `playing` here flips the replicated
  `GroupRecord.Playing` bool — equivalent to the F.3/F.4 transport endpoints.
- **Response `200`:** `{ "version": 49, "group": { /* updated GroupRecord */ } }`, `ETag: 49`.
- **Status codes:** `200`; `404 not_found`; `422 unprocessable`
  (member in another group; profile unsupported by a member’s `caps`);
  `409 version_conflict` / `conflict`; `412 precondition_required`;
  `401 unauthenticated`; `502 proxy_failed` (a member unreachable for a transport change).
- **If-Match:** **required** (mutates `ConfigDoc.Groups` and possibly `Nodes` membership).
- **Proxy:** config write is local → gossip. A **`playing` change fans out** to the
  affected member(s)/master over mTLS so playback starts/stops promptly rather than
  waiting on gossip convergence; partial failure is reported per-member in the body
  (`"warnings": [...]`). (The dedicated transport endpoints F.3/F.4 are the preferred
  play/stop path; this overload exists for atomic membership+play-state edits.)

---

### E.5 `DELETE /api/v1/groups/{id}`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Delete a group. Members fall back to solo groups (a node is always in
  exactly one group), stream stops.
- **Request body:** none.
- **Response `200`:** `{ "version": 50, "freedNodeIds": ["n-7a3f", "n-91c2"] }`, `ETag: 50`.
- **Status codes:** `200`; `404 not_found`; `409 version_conflict`;
  `412 precondition_required`; `401 unauthenticated`.
- **If-Match:** **required**.
- **Proxy:** local config write → gossip; a stop **fans out** to the former master(s)
  over mTLS (best-effort; non-fatal if a member is offline).

---

# F. MEDIA — `/api/v1/media`, `/api/v1/groups/{id}/media|play|stop`

UI: **Media** screen (browse `data/`, select, play/stop/loop) ([09](./09-ui-screens.md)).
Media model (mp3 from `data/`, master-side decode, loop): D14 +
[05-audio-streaming-protocol.md](./05-audio-streaming-protocol.md).

---

### F.1 `GET /api/v1/media`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** List mp3s in the node’s `data/` folder with metadata. **Per-node** —
  each node has its own `data/`, so this is node-scoped.
- **Query params:** optional `?nodeId=<id>` to list a *specific* node’s folder
  (defaults to the receiving node).
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "nodeId": "n-7a3f",
    "files": [
      { "file": "song.mp3", "title": "Song", "artist": "Artist", "durationMs": 213000, "sizeBytes": 5123456, "sampleRate": 44100 }
    ]
  }
  ```
- **Status codes:** `200`; `404 not_found` (unknown `nodeId`); `401 unauthenticated`;
  `502 proxy_failed` (target node unreachable).
- **If-Match:** not used (read-only; `data/` is not part of the `ConfigDoc`).
- **Proxy:** **proxied to the target node** when `nodeId` ≠ receiving node (the file
  listing is local to that node’s disk). Otherwise local.

---

### F.2 `POST /api/v1/groups/{id}/media`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Select the media file for a group and set loop. Writes
  `GroupRecord.media` (`{file, loop}` per [README.md §6.5](./README.md)). The selected
  file must exist on the group’s **master** node (master-side decode, D14/[05](./05-audio-streaming-protocol.md)).
- **Request body:** `{ "file": "song.mp3", "loop": true }`
- **Response `200`:** `{ "version": 51, "group": { /* GroupRecord with media set */ } }`, `ETag: 51`.
- **Status codes:** `200`; `404 not_found` (unknown group, or file absent on the
  master — `code:"not_found"` with `message` naming the node); `422 unprocessable`
  (non-mp3); `409 version_conflict`; `412 precondition_required`; `401 unauthenticated`.
- **If-Match:** **required** (mutates `ConfigDoc.Groups`).
- **Proxy:** local config write → gossip. Existence check against the master’s `data/`
  is **proxied** to the master over mTLS.

---

### F.3 `POST /api/v1/groups/{id}/play`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Start (or resume) playback for the group: master begins
  decode→encode→FEC→unicast on a fresh `streamGen` ([README.md §6.4](./README.md),
  [05](./05-audio-streaming-protocol.md)). Sets the replicated `GroupRecord.Playing=true`.
- **Request body:** optional `{ "file": "song.mp3", "loop": true }` to select-and-play
  in one shot (equivalent to F.2 then play).
- **Response `200`:**
  ```json
  { "version": 52, "group": { /* GroupRecord, "playing": true */ } }
  ```
  `ETag: 52`.
- **Status codes:** `200`; `404 not_found`; `409 conflict` (no media selected);
  `409 version_conflict` (if body selects media); `412 precondition_required` (only when
  the body mutates config — a pure play with no selection still requires `If-Match`
  because it flips `GroupRecord.Playing`); `401 unauthenticated`; `502 proxy_failed`
  (master unreachable).
- **If-Match:** **required** (`GroupRecord.Playing` is part of the `ConfigDoc`).
- **Proxy:** local config write → gossip, **and fans out** the start command to the
  group **master** over mTLS so audio begins without waiting for gossip convergence.

---

### F.4 `POST /api/v1/groups/{id}/stop`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Stop playback: master halts the origin loop; sets
  `GroupRecord.Playing=false`.
- **Request body:** none.
- **Response `200`:** `{ "version": 53, "group": { /* "playing": false */ } }`, `ETag: 53`.
- **Status codes:** `200`; `404 not_found`; `409 version_conflict`;
  `412 precondition_required`; `401 unauthenticated`; `502 proxy_failed`.
- **If-Match:** **required** (`GroupRecord.Playing` mutation).
- **Proxy:** local config write → gossip + **fan-out stop** to the master over mTLS.

---

# F2. CALIBRATION — `/api/v1/calibrate/*`

UI: **Node detail** calibration flow ([09](./09-ui-screens.md)). Signal spec, manual
measurement, and `HWDelayUs` trim semantics: [06 §5.3](./06-audio-output-scheduling.md)
+ Appendix. The calibration signal is **generated in-process** at the canonical rate (no
external file) and is identical across nodes (R10).

---

### F2.1 `POST /api/v1/calibrate/play`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Play the **built-in calibration signal** (per 1 s: ~1 ms full-scale click
  + ~200 ms 1 kHz tone + silence) **synchronously** on the selected nodes, so the
  operator can judge the inter-node offset by ear / phone-mic and then trim `HWDelayUs`
  (manual MVP measurement, R10 / [06 §5.3](./06-audio-output-scheduling.md)).
- **Request body** (exactly one of `groupId` / `nodeIds`):
  ```json
  {
    "groupId": "g-kitchen",
    "durationSec": 10
  }
  ```
  or
  ```json
  {
    "nodeIds": ["n-7a3f", "n-91c2"],
    "durationSec": 10
  }
  ```
- **Response `200`:**
  ```json
  { "playedOn": ["n-7a3f", "n-91c2"], "durationSec": 10, "warnings": [] }
  ```
  Per-node failure to start is reported in `warnings[]` (a `Render=false` node cannot
  play and is reported, not fatal).
- **Status codes:** `200`; `400 invalid_request` (neither/both of `groupId`/`nodeIds`,
  or bad `durationSec`); `404 not_found` (unknown group/node); `401 unauthenticated`;
  `502 proxy_failed` (a target unreachable — reported per-node where the master is
  reachable).
- **If-Match:** **not used** (transient playback; does not write the `ConfigDoc`).
- **Proxy:** **fan-out** — the receiving node drives synchronous playback on the selected
  nodes over mTLS.

---

### F2.2 `POST /api/v1/calibrate/measure` — *later enhancement (not in MVP)*
- **Status:** **Documented future enhancement**, not implemented in the MVP. The MVP
  measurement path is **manual** (operator judges offset → `PATCH /api/v1/nodes/{id}`
  with `hwDelayUs`, D.3).
- **Purpose (planned):** Upload a recording of the calibration signal (e.g. captured on a
  phone mic); the node cross-correlates the click+tone to compute a **suggested
  `HWDelayUs`** offset, which the operator can then apply via D.3.
- **Planned request:** `multipart/form-data` recording upload + `{ "nodeId": "...", "referenceNodeId": "..." }`.
- **Planned response:** `{ "suggestedHwDelayUs": 1450, "confidence": 0.92 }`.
- **If-Match:** would not apply (analysis only; the apply step is the existing D.3
  `PATCH`). **Proxy:** TBD.

---

# G. STATUS — `/api/v1/status`, `/api/v1/groups/{id}/status`

Live, **non-replicated** runtime telemetry (not in the `ConfigDoc`). UI: **Dashboard**
live tiles and **Groups → status** ([09](./09-ui-screens.md)). Metric meanings:
clock ([04](./04-clock-and-groups.md)), drift/ratio/underruns
([06](./06-audio-output-scheduling.md)).

---

### G.1 `GET /api/v1/status`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** This node’s live runtime status: uptime, role(s), sink state, current
  group, clock health. **Node-specific** (about the *receiving* node’s live process).
- **Query params:** optional `?nodeId=<id>` for a peer’s status.
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "nodeId": "n-7a3f",
    "online": true,
    "uptimeSec": 86400,
    "sink": { "kind": "alsa", "rate": 48000, "channels": 2, "running": true },
    "group": { "id": "g-kitchen", "role": "master" },   // role: "master"|"follower"|"solo"
    "clock": { "synced": true, "offsetUs": -120, "quality": "good" },
    "configVersion": 42
  }
  ```
- **Status codes:** `200`; `401 unauthenticated`; `404 not_found` (unknown `nodeId`);
  `502 proxy_failed`; `503 not_ready` (sink not started / uninitialized).
- **If-Match:** not used (read-only, non-config).
- **Proxy:** local for the receiving node; **proxied** to the peer when `nodeId` is set.

---

### G.2 `GET /api/v1/groups/{id}/status`
- **Auth:** mTLS-node / admin-session / api-key.
- **Purpose:** Per-group **live sync** telemetry, **per member**: sync error, offset,
  drift ratio, underruns — plus the elected master id, the negotiated profile, and clock
  quality. This is the realtime counterpart to the static `GET /groups/{id}` (E.3).
- **Request body:** none.
- **Response `200`:**
  ```json
  {
    "groupId": "g-kitchen",
    "masterNodeId": "n-7a3f",
    "profile": { "codec": "pcm", "fec": "xorParity", "rate": 48000, "framesPerChunk": 480, "fecK": 8, "interleave": 4 },
    "streamGen": 7,
    "playing": true,
    "members": [
      {
        "nodeId": "n-91c2",
        "syncErrorUs": 38,          // content-domain sync error ([06])
        "offsetUs": -120,           // clock offset to master ([04])
        "driftRatio": 1.0000123,    // resampler ppm ratio ([06] drift PI loop)
        "underruns": 0,             // ring underrun count ([06] ring buffer)
        "clockQuality": "good",     // "good"|"fair"|"poor"
        "online": true
      },
      { "nodeId": "n-7a3f", "syncErrorUs": 0, "offsetUs": 0, "driftRatio": 1.0, "underruns": 0, "clockQuality": "good", "online": true }
    ]
  }
  ```
- **Status codes:** `200`; `404 not_found`; `401 unauthenticated`;
  `503 not_ready` (group not yet synced / not playing — `members[]` may report
  `online:false` or omit live fields); `502 proxy_failed` (a member unreachable —
  reported per-member, not as a top-level error, unless the master itself is unreachable).
- **If-Match:** not used (read-only, non-config).
- **Proxy:** **fan-out (read)** — the receiving node queries each member’s live state
  over mTLS (each member knows its own offset/drift/underruns relative to the master)
  and aggregates. The master is authoritative for `masterNodeId`, `profile`, `streamGen`.

---

## Appendix — endpoint index

| # | Method + path | Auth | If-Match | Proxy |
|---|---|---|---|---|
| A.1 | `GET /bootstrap/info` | public-bootstrap | no | local |
| A.2 | `POST /bootstrap/adopt` | public-bootstrap + PIN | no | local (node side) |
| B.1 | `POST /api/v1/setup` | uninitialized-only | no (genesis) | local |
| B.2 | `POST /api/v1/auth/login` | public-in-cluster | no | local |
| B.3 | `POST /api/v1/auth/logout` | admin-session | no | local |
| B.3a | `POST /api/v1/auth/password` | admin-session | **yes** | local→gossip |
| B.4 | `GET /api/v1/auth/session` | session / api-key | no | local |
| B.5 | `GET /api/v1/auth/keys` | admin-session | no | local |
| B.6 | `POST /api/v1/auth/keys` | admin-session | **yes** | local→gossip |
| B.7 | `DELETE /api/v1/auth/keys/{id}` | admin-session | **yes** | local→gossip |
| C.1 | `GET /api/v1/cluster/info` | node/session/api-key | no | local |
| C.2 | `GET /api/v1/discovery` | node/session/api-key | no | fan-out (read) |
| C.3 | `POST /api/v1/cluster/adopt` | session / api-key | **yes** | proxied + local→gossip |
| C.4 | `POST /api/v1/cluster/takeover` | session / api-key | **yes** | proxied + local→gossip |
| C.5 | `POST /api/v1/nodes/{id}/forget` | node/session/api-key | **yes** | local→gossip (+proxy notify) |
| C.6 | `POST /api/v1/cluster/leave` | node/session/api-key | **yes** | proxied (revoke+drop) → local wipe |
| D.1 | `GET /api/v1/nodes` | node/session/api-key | no | local |
| D.2 | `GET /api/v1/nodes/{id}` | node/session/api-key | no | local |
| D.3 | `PATCH /api/v1/nodes/{id}` | node/session/api-key | **yes** | proxied to owner→gossip |
| E.1 | `GET /api/v1/groups` | node/session/api-key | no | local |
| E.2 | `POST /api/v1/groups` | node/session/api-key | **yes** | local→gossip |
| E.3 | `GET /api/v1/groups/{id}` | node/session/api-key | no | local |
| E.4 | `PATCH /api/v1/groups/{id}` | node/session/api-key | **yes** | local→gossip (+fan-out on `playing`) |
| E.5 | `DELETE /api/v1/groups/{id}` | node/session/api-key | **yes** | local→gossip (+fan-out stop) |
| F.1 | `GET /api/v1/media` | node/session/api-key | no | proxied (per-node `data/`) |
| F.2 | `POST /api/v1/groups/{id}/media` | node/session/api-key | **yes** | local→gossip (+proxy check) |
| F.3 | `POST /api/v1/groups/{id}/play` | node/session/api-key | **yes** | local→gossip + fan-out to master |
| F.4 | `POST /api/v1/groups/{id}/stop` | node/session/api-key | **yes** | local→gossip + fan-out to master |
| F2.1 | `POST /api/v1/calibrate/play` | node/session/api-key | no | fan-out |
| F2.2 | `POST /api/v1/calibrate/measure` | node/session/api-key | n/a | *later enhancement* |
| G.1 | `GET /api/v1/status` | node/session/api-key | no | local (or proxied) |
| G.2 | `GET /api/v1/groups/{id}/status` | node/session/api-key | no | fan-out (read) |
