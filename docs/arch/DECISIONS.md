# Contract reconciliation — integrator decisions

The eleven piece architects raised 43 contract concerns against
[S-skeleton.md](S-skeleton.md) and the [spec](../README.md). This file
resolves every one that needed a decision; trivially-confirmed items are
grouped at the end. **These decisions amend the arch docs** — where a piece
doc disagrees with this file, this file wins. (Surgical fixes already applied
to S-skeleton.md are marked ✎S.)

## Decisions

**D1 — node.json holds exactly `{id, name}`.** (A) Everything else (following,
ports, observations) is runtime/replicated, in-memory only, rebuilt on start.

**D2 — `ENSEMBLE_OUTPUT` is env-only** (`auto` default | `null` | `file:<path>`
| explicit backend name). No flag. Added to spec §2. (A/E/K)

**D3 — capabilities are assembled by K (main) at startup** — PATH probe for
the output backends, build-tag for opus, static format list — and handed to
cluster via its config/setter. A stays pure, D stays decode-only. A node with
`ENSEMBLE_OUTPUT=null` reports `playback:false` but still receives and
"plays" to the null sink; playback capability never gates group membership or
stream fan-out. (A/D/K)

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
changes. (H/K)

**D18 — stream endpoints**: H's `Resolver` seam (`StreamEndpoints(members)`)
is implemented in K as a thin adapter over `cluster.DialCandidates` +
each node's `streamPort`. (H/K)

**D19 — `/api/status` JSON envelope** (pinned for I, J, and the e2e):

```json
{
  "id": "<32hex>", "name": "...", "role": "master|follower|solo",
  "groupId": "<32hex>",
  "ports": {"http": 8080, "stream": 9090, "gossip": 7946},
  "sink":  {"played": 0, "silence": 0, "lateDrop": 0, "staleGen": 0, "synced": false},
  "clock": {"synced": false, "offsetNs": 0, "rttNs": 0}
}
```
(I/K; `role:"solo"` = master of a group of 1.)

**D20 — `--join` / `ENSEMBLE_JOIN`** (comma-separated `host:gossipPort` seed
list) is added as a dev flag in A, passed to `cluster.Join`. It exists for
hermetic loopback e2e tests; mDNS remains the production path. Added to spec
§2 as dev-only. (K)

**D21 — bufferMs is fixed per session**; changing group settings applies from
the next `play`. Sink `Stats().Synced` is computed live from the Clock at
call time. `Backend.Write` may block; the exec backend gets a write deadline
via process kill on Close — accepted v1 limitation. (E)

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
