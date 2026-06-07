# B — mDNS discovery

Source of truth: [docs/README.md](../README.md) §3 / §3.1; piece contract:
[IMPLEMENTATION.md](../../IMPLEMENTATION.md) wave 1, piece B. Shared contracts:
[S-skeleton.md](./S-skeleton.md).

This piece is **only** the mDNS half of discovery (README §3.1): register the
node's `_ensemble._tcp` service with TXT `id/gossip/http/stream`, browse the LAN
continuously, and emit deduplicated `Peer` events on a channel. It has **no
memberlist dependency** — the cluster piece (C) consumes the channel and does the
actual joining. Keep it small: one file of real code, one of tests.

Library: `github.com/grandcat/zeroconf v1.0.0` (already in the module cache).
Relevant surface used here:

```go
// register; returns a *Server with Shutdown()
func zeroconf.Register(instance, service, domain string, port int,
        text []string, ifaces []net.Interface) (*zeroconf.Server, error)
func (*zeroconf.Server) Shutdown()

// browse; blocks until ctx is cancelled, pushing entries to the channel
func zeroconf.NewResolver(opts ...zeroconf.ClientOption) (*zeroconf.Resolver, error)
func (*zeroconf.Resolver) Browse(ctx context.Context, service, domain string,
        entries chan<- *zeroconf.ServiceEntry) error

type zeroconf.ServiceEntry struct {
        ServiceRecord            // .Instance string
        Port     int
        Text     []string
        AddrIPv4 []net.IP
        AddrIPv6 []net.IP
        TTL      uint32
        // ...
}
```

---

## 1. Package / file layout

Files B creates and owns (everything under `internal/discovery/`):

```
internal/discovery/discovery.go   Config, Peer, Discovery: New/Run/Peers/Close; register + browse loops, dedup
internal/discovery/parse.go       parseEntry: *zeroconf.ServiceEntry -> (Peer, ok); TXT parsing, addr selection
internal/discovery/discovery_test.go  loopback register+browse round-trip, restart-on-error, shutdown, dedup
internal/discovery/parse_test.go      TXT/address parsing table tests (no sockets)
```

S's stub `internal/discovery/discovery.go` is replaced by B. No other piece may
edit these files. B imports only stdlib, `ensemble/internal/id`, and
`github.com/grandcat/zeroconf`. **No `internal/cluster`, no `memberlist`** — the
dependency arrow is C→B (C consumes B's channel), never B→C.

---

## 2. Concrete Go API

```go
package discovery

import (
        "context"
        "log/slog"
        "net/netip"
        "sync"
        "time"

        "ensemble/internal/id"

        "github.com/grandcat/zeroconf"
)

// ServiceName / Domain are the fixed mDNS coordinates for ensemble (§3).
const (
        ServiceName = "_ensemble._tcp"
        Domain      = "local."
)

// Peer is one discovered ensemble node, as advertised in its mDNS TXT record
// (§3) plus the address the responder was reached on. It is the unit C consumes
// to decide gossip joins; B does no joining itself.
type Peer struct {
        ID         id.ID          // from TXT "id=" (32 hex); never Zero on a valid Peer
        Addr       netip.Addr     // chosen responder IP (IPv4 preferred, then IPv6)
        GossipPort int            // from TXT "gossip="
        HTTPPort   int            // from TXT "http="
        StreamPort int            // from TXT "stream="
}

// GossipAddrPort is the address C dials to join the peer's gossip cluster (§3).
func (p Peer) GossipAddrPort() netip.AddrPort {
        return netip.AddrPortFrom(p.Addr, uint16(p.GossipPort))
}

// Config carries this node's own advertised identity. All fields required.
type Config struct {
        ID         id.ID  // this node's immutable node ID (§1)
        Instance   string // mDNS instance name; use ID.String() for uniqueness
        GossipPort int    // actually-bound ports (§2 bind-or-increment result)
        HTTPPort   int
        StreamPort int
        Log        *slog.Logger // optional; defaults to slog.Default() with comp=discovery
}

// Discovery registers this node over mDNS and continuously browses for peers,
// emitting deduplicated Peer events. One per node. Zero-value is not usable;
// construct with New.
type Discovery struct {
        cfg  Config
        log  *slog.Logger
        peers chan Peer

        mu     sync.Mutex            // guards seen + server + closed
        seen   map[id.ID]seenEntry   // dedup / throttle state, keyed by peer ID
        server *zeroconf.Server      // mDNS registration handle (for Shutdown)
        closed bool

        ctx    context.Context       // cancelled by Close
        cancel context.CancelFunc
        wg     sync.WaitGroup        // browse + register-refresh goroutines
}

// New constructs a Discovery. It does not touch the network; call Run to start.
func New(cfg Config) *Discovery

// Run registers the mDNS service and starts the continuous browse loop in the
// background. It is non-blocking and returns once registration is attempted.
// A registration error is logged and retried inside Run's goroutines, not
// returned, so a transient mDNS failure never aborts node startup (§3: "both
// always on"). Run is called once; calling it after Close is a no-op.
func (d *Discovery) Run()

// Peers returns the receive end of the deduplicated peer-event channel. C ranges
// over it. The channel is closed when Close completes, so a `for p := range
// d.Peers()` loop in C terminates on shutdown.
func (d *Discovery) Peers() <-chan Peer

// Close stops browsing, unregisters the mDNS service, waits for goroutines, and
// closes the Peers channel. Idempotent; safe to call once concurrently.
func (d *Discovery) Close() error

// --- internal ---

// seenEntry is per-peer dedup state (§ control flow).
type seenEntry struct {
        peer     Peer      // last emitted value
        lastSeen time.Time // last time we OBSERVED this peer (browse hit)
        emitted  time.Time // last time we EMITTED on the channel
}

// parseEntry converts a raw zeroconf entry into a Peer, returning ok=false for
// our own advertisement, malformed TXT, or an entry with no usable address.
func parseEntry(e *zeroconf.ServiceEntry, self id.ID) (Peer, bool)
```

### Dedup / throttle contract (what "deduped, throttled" means, §B)

`parseEntry` yields a candidate `Peer` for every browse hit (zeroconf re-queries
periodically, so the same node reappears every few seconds). `Discovery` emits on
the channel only when, under the one mutex:

1. the peer ID is **new** (not in `seen`), **or**
2. a **material field changed** vs. the last emitted value — any of `Addr`,
   `GossipPort`, `HTTPPort`, `StreamPort` (re-advertise after a port change, a
   new IP from multi-homing, §3.1), **or**
3. it has been ≥ `reEmitInterval` (30 s) since we last emitted for this ID
   (liveness refresh so C can re-confirm a long-lived peer).

`seen[id].lastSeen` is always updated on every hit (cheap liveness bookkeeping);
only emission is throttled. This makes the channel low-rate and idempotent: C can
treat every `Peer` as "join/refresh this peer" without flooding.

---

## 3. Control flow, goroutines, locking

One mutex (`d.mu`) guards `seen`, `server`, `closed`. Two long-lived goroutines.

### Startup — `New` then `Run`

- `New(cfg)` fills defaults (`Log` → `slog.Default().With("comp","discovery")`,
  `Instance` → `cfg.ID.String()` if empty), allocates `seen`, the buffered
  `peers` channel (cap 64, see edge cases), and the `context.WithCancel`. No I/O.
- `Run()`:
  1. Builds the TXT slice via `txtRecords(cfg)` →
     `["id=<hex>", "gossip=<p>", "http=<p>", "stream=<p>"]`.
  2. Starts goroutine **R (register-keeper)**: calls `register()` which does
     `zeroconf.Register(instance, ServiceName, Domain, cfg.HTTPPort, txt, nil)`
     (advertised port is HTTP — informational only; real ports are in TXT).
     On success stores `d.server` under the lock. On error, logs and retries
     every `retryInterval` (5 s) until `d.ctx` is done. (zeroconf's `Server`
     self-maintains its announcements once registered, so R mostly sleeps after
     the first success; it exists so a *failed* initial registration heals.)
  3. Starts goroutine **W (browse-keeper)**: runs `browseOnce(d.ctx)` in a loop.
     `browseOnce` creates a fresh `zeroconf.NewResolver()`, a local
     `entries := make(chan *zeroconf.ServiceEntry, 16)`, launches an inline
     drain goroutine that ranges `entries` → `parseEntry` → `maybeEmit`, then
     calls `resolver.Browse(d.ctx, ServiceName, Domain, entries)` which **blocks
     until ctx is cancelled or errors**. On return (error or ctx-done) the drain
     goroutine ends when `entries` closes. If `d.ctx` is still live, log and loop
     after `retryInterval` — this is the **restart-on-error** requirement (§B).

`Run` returns immediately after launching R and W.

### Steady state

- W's drain goroutine receives every `*zeroconf.ServiceEntry`, calls
  `parseEntry(e, cfg.ID)`. On `ok`, `maybeEmit(peer)`:
  - Lock `d.mu`. If `d.closed`, unlock and return (drop).
  - Apply the three-rule dedup test against `seen[peer.ID]`.
  - Update `seen[peer.ID].lastSeen = now` always.
  - If emitting: update `seen[peer.ID].{peer,emitted}`, unlock, then send.
    Send uses a non-blocking `select { case d.peers <- peer: default: drop+count }`
    so a slow/absent C consumer can never wedge the browse loop (§ edge cases).
    Releasing the lock **before** the channel send keeps the send off the lock.
- R is idle (sleeping on a ticker / ctx) after the first successful register.

### Shutdown — `Close`

1. Lock `d.mu`; if already `closed`, unlock, return nil (idempotent).
   Set `closed = true`; capture `server`; unlock.
2. `d.cancel()` — unblocks `resolver.Browse` (W) and R's retry sleep.
3. `if server != nil { server.Shutdown() }` — sends mDNS goodbye, frees 5353.
4. `d.wg.Wait()` — both goroutines (and W's inner drain) return.
5. `close(d.peers)` — terminates C's `range`. Done exactly once because step 1
   guards re-entry. After this, `maybeEmit` is short-circuited by `closed`, so no
   send-on-closed-channel race (the `closed` check + send both happen, but only
   under sequencing: emitters that passed the `closed==false` check before step 1
   are joined by `wg.Wait()` in step 4 *before* `close` in step 5 — the drain
   goroutine is part of `wg`).

Locking summary: single `sync.Mutex`. The channel send is the only cross-goroutine
hand-off and is non-blocking. No lock is held across a blocking call.

---

## 4. Edge cases & failure handling

- **Self-discovery (§3).** Every node browses and *will* see its own
  advertisement. `parseEntry` drops entries whose TXT `id ==` our own `cfg.ID`
  (returns `ok=false`). This is the primary filter, not address comparison
  (loopback/own IPs are unreliable). Tested.
- **Malformed / partial TXT (§3).** A peer mid-(re)announce may expose a TXT set
  missing a key or with a non-numeric port. `parseEntry` requires all four keys
  (`id`,`gossip`,`http`,`stream`) to parse (`id.Parse`, `strconv.Atoi` with port
  in 1..65535); any failure → `ok=false`, logged at debug, dropped. Better to
  miss one announce (zeroconf re-queries) than emit a half-formed Peer.
- **No usable address (§3.1).** Choose `Addr` from `AddrIPv4` first, else
  `AddrIPv6`, skipping loopback/unspecified, taking the first valid via
  `netip.AddrFromSlice` + `Is*` checks. If none, `ok=false`. B reports the
  *responder* address only; the authoritative CIDR/observed-IP intersection
  (§3.1) is C's job — B just needs one reachable IP to seed the gossip join.
- **Restart-on-error (§B, §3 "always on").** Both `register()` and
  `browseOnce()` failures are logged and retried (5 s) until ctx-done; a failure
  never propagates out of `Run`. mDNS being temporarily broken (no multicast on a
  bridge, momentary EADDRNOTAVAIL) must not crash or stop the node — gossip can
  still carry state once any single peer is met.
- **Slow/absent consumer.** The `peers` channel is buffered (64) and the send is
  non-blocking with a drop+counter. C is expected to drain promptly, but B never
  blocks its browse loop on C. Dropped events are recovered by the 30 s re-emit
  rule and by zeroconf's periodic re-queries, so an event lost to a full buffer
  is re-offered shortly — eventual delivery, no liveness coupling.
- **Duplicate/flapping advertisements.** Handled by the dedup rules in §2: a
  peer re-advertising identical fields within 30 s produces no channel traffic;
  a real change (new IP after roaming, re-bound port after restart, §2/§3.1) is
  emitted immediately.
- **Port-number churn (§2 bind-or-increment).** Because the *bound* ports go in
  TXT, a peer that restarted onto base+1 looks like a field change and is
  re-emitted — C then re-resolves and re-joins. B is correct as long as `Run` is
  given the *actually-bound* ports (K passes the netx results).
- **Close before Run / double Close / Run after Close.** `New`→`Close` without
  `Run`: cancel + `server==nil` skip + `wg.Wait()` over zero goroutines +
  `close(peers)`; fine. `Close` twice: guarded by `closed`. `Run` after `Close`:
  the register/browse goroutines see `d.ctx` already cancelled and exit at once
  (their first ctx check); `Run` itself can early-return if `closed`.
- **IPv6 / link-local.** Link-local v6 (`fe80::`) responders are skipped (no
  zone handling in v1; gossip over link-local is out of scope §11-adjacent).
  Global/ULA v6 is kept. Matches netx `InterfaceCIDRs` skipping link-local.
- **zeroconf `nil` ifaces.** Passing `nil` for `ifaces` lets the library use all
  multicast-capable interfaces, which is what we want; no interface selection in
  v1 (KISS). If a host has zero multicast interfaces, register/browse error and
  retry harmlessly forever — the node still works via any later direct gossip.

---

## 5. Test plan

All tests use real loopback mDNS where two in-process `Discovery` instances on
the same host discover each other (multicast 5353 works on loopback/localhost in
CI containers; if the CI environment blocks multicast the socket tests
`t.Skip` on a register error rather than hang). Pure parsing tests use no
sockets.

`internal/discovery/parse_test.go` (no network):
- `TestParseEntryValid` — full TXT + IPv4 → correct `Peer` (id, ports, addr).
- `TestParseEntryDropsSelf` — TXT `id` == self → `ok==false`.
- `TestParseEntryMissingKey` — drop `gossip=`/`http=`/`stream=`/`id=` each → `ok==false`.
- `TestParseEntryBadPort` — non-numeric / out-of-range port → `ok==false`.
- `TestParseEntryBadID` — TXT id not 32-hex → `ok==false`.
- `TestParseEntryPrefersIPv4` — both v4+v6 present → v4 chosen.
- `TestParseEntryIPv6Fallback` — only global v6 → v6 chosen.
- `TestParseEntrySkipsLoopbackLinkLocal` — only 127.0.0.1 / fe80:: → `ok==false`.
- `TestGossipAddrPort` — `Peer.GossipAddrPort()` composes addr+gossip port.
- `TestTXTRecords` — `txtRecords(cfg)` emits the four `key=value` strings.

`internal/discovery/discovery_test.go` (loopback mDNS; skip on register error):
- `TestRegisterBrowseRoundTrip` — node A registers, node B browses, B emits a
  `Peer` for A with A's id/ports within a timeout.
- `TestDedupNoRepeatWithinInterval` — A static; B emits A exactly once over a
  window shorter than `reEmitInterval` (drain channel, assert count == 1).
- `TestReEmitAfterInterval` — with `reEmitInterval` shrunk via a test hook, B
  re-emits the same unchanged peer after the interval elapses.
- `TestEmitOnFieldChange` — A re-registers with a different stream port (new
  `Discovery`/`Server`); B emits a second `Peer` with the updated port.
- `TestSelfNotEmitted` — a single node browsing alone never emits its own Peer.
- `TestRestartOnBrowseError` — inject a resolver factory that fails once then
  succeeds (test seam `newResolver func() (*zeroconf.Resolver, error)`); assert
  the browse loop retried and subsequently emitted (no permanent stall).
- `TestCloseClosesPeersChannel` — after `Close`, the `Peers()` channel is closed
  (`_, ok := <-ch; ok==false`) and the goroutines exited (no leak via a
  `wg`-backed deadline / `goleak`-style check on a done channel).
- `TestCloseIdempotent` — second `Close` returns nil, no panic.
- `TestCloseBeforeRun` — `New` then `Close` (no `Run`) closes the channel cleanly.
- `TestNonBlockingSendNeverStalls` — fill the channel buffer, keep browse hits
  coming, assert the browse drain goroutine keeps making progress (no deadlock;
  observed via a liveness counter advancing) — proves the non-blocking send.

Test seams kept minimal and unexported: `reEmitInterval`/`retryInterval` as
package vars overridable in tests; an optional `newResolver` field on
`Discovery` defaulting to `zeroconf.NewResolver` for the restart test. No public
API exists solely for tests.
