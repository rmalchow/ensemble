# Appendix A — Reusable algorithms, pinned choices & starting parameters

This appendix makes the spec **self-contained**: it inlines the algorithms that the
section docs describe "by reuse," pins the concrete third-party libraries and crypto
primitives, concretizes the adoption PIN exchange, specifies the cluster/runtime glue
(A.14), and gives starting values for every tunable. With this appendix an implementer
needs only the Go standard library and the named third-party packages' own docs — no
access to the `mpvsync` codebase.

All pseudocode is normative-by-intent (clarify behavior, not exact Go). Names match
[README §6](./README.md). Cross-refs: clock/timeline [04](./04-clock-and-groups.md),
streaming [05](./05-audio-streaming-protocol.md), output [06](./06-audio-output-scheduling.md),
config [07](./07-config-and-replication.md), security [03](./03-adoption-takeover-security-pki.md).

---

## A.1 Shared clock — NTP-style offset estimator

**Wire exchange** (clock plane, UDP, source-allowlisted, per group; follower → its
group master). Fixed 40-byte big-endian packet, four monotonic-ns timestamps:

```
magic uint32 'MPVC' | ver u8 | kind u8(1=req,2=reply) | pad u16 | seq u64 | t1 i64 | t2 i64 | t3 i64
```
- Follower sends `kind=req` stamped with `t1` (its send mono).
- Master replies `kind=reply` echoing `t1`, adding `t2` (master recv mono) and `t3`
  (master send mono).
- Follower records `t4` (its recv mono) on receipt.

**Per-sample math** (monotonic ns; robust to wall-clock steps):
```
offset = ((t2 - t1) + (t3 - t4)) / 2      // master_mono - follower_mono
delay  = (t4 - t1) - (t3 - t2)            // round trip minus server processing
if delay < 0 { delay = 0 }
```

**Estimator** (min-delay filter + EWMA — queuing jitter inflates delay, so the
lowest-delay sample is the most trustworthy offset):
```
window  : ring of last N samples           (N = 8 wired, 16 WiFi)
alpha   : EWMA factor                       (0.15 wired, 0.10 WiFi — smoother)
Add(s):
    push s into window (drop oldest beyond N)
    best = argmin_{w in window} w.delay
    if !have { offset = best.offset; have = true }
    else      { offset += alpha * (best.offset - offset) }   // slew, never step
    return offset
Offset() -> (offset, have)
MinDelay() -> min delay in window           // sync-quality proxy for the UI
```
Ping cadence: 500 ms during the first ~5 s of a lock (fast convergence), then 1 s.
A follower with `have=false` does not render (Timeline `ok=false`).

## A.2 Group timeline & projection

The synchronized quantity is the **stream sample index** `S` (canonical-rate frames
since stream start), carried on every audio chunk as `sampleIndex` + `masterMono`
([README §6.4](./README.md)). A follower computes the index expected *now*:

```
NowSample():
    off, ok = clock.Offset();           if !ok { return _,_,false }
    b, ok   = latestChunkMeta();         if !ok { return _,_,false }   // newest seen chunk
    now_master_mono = clock.NowMono() + off          // this follower's mono -> master mono
    elapsed_ns      = now_master_mono - b.masterMono
    S = b.sampleIndex + round(elapsed_ns * rate / 1e9)
    return S, playing, true
```
`rate` is the group profile rate (48000). `streamGen` mismatch ⇒ treat as not-synced
until a chunk of the current generation arrives (handles media change/seek).

## A.3 Near-unity resampler (cubic / 4-tap Farrow)

Converts source frames → output frames at a ratio set per block by the drift loop.
Keeps fractional phase + 3-tap history across calls so block boundaries are seamless.

```
state: phase in [0,1), step = 1/ratio, hist[3] per channel (s0,s1,s2), nhist
SetRatio(r): r = clamp(r, 1 - MaxPPM*1e-6, 1 + MaxPPM*1e-6); step = 1/r     // MaxPPM=200
Reset(): phase=0; nhist=0
Process(in) -> (out, consumedInputFrames):
    for each input frame f (interleaved):
        if nhist < 3 { push f into hist; continue }          // prime the window
        // window s0=hist[0], s1=hist[1], s2=hist[2], s3=f  (s1 is integer pos)
        while phase < 1.0:
            for ch: out += cubic(s0,s1,s2,s3, phase)[ch]      // Catmull-Rom / Hermite
            phase += step
        phase -= 1.0
        slide hist left, push f
    return out, framesConsumed
cubic(y0,y1,y2,y3,t):   // interpolate between y1 and y2 at fractional t
    a0 = y3 - y2 - y0 + y1; a1 = y0 - y1 - a0; a2 = y2 - y0; a3 = y1
    return ((a0*t + a1)*t + a2)*t + a3
```
±200 ppm ≈ ±0.02 % → inaudible; this is the drift actuator, never used for large-ratio
conversion (the source is pre-resampled to the canonical rate at the master).

## A.4 Drift control — corrected content-domain PI loop

**The fix (see [06](./06-audio-output-scheduling.md)):** regulate a **content/source-domain**
error so the resample ratio actually has authority over it. `played_content` is where in
the *source content* the DAC currently is; `want_content` is where the group timeline
says it should be.

```
// counters maintained by the render path (per-channel frames):
//   srcConsumed  : cumulative SOURCE frames the resampler has consumed since reseek
//   buffered     : source-equivalent frames still un-played = ring backlog (in source
//                  frames) + device.Delay()/ratio   (device frames -> source frames)
played_content = wantBaseSrc + (srcConsumed - buffered)
want_content   = groupSampleIndex + round(HWDelayUs * 1e-6 * rate)   // + per-node trim
errSamples     = played_content - want_content        // + => ahead => must slow => ratio<1

DriftLoop.Update(errSamples):
    if |errSamples| > HardErrSamp: return RESEEK, 1.0          // do NOT integrate
    integral += errSamples
    clamp Ki*integral to ±IntegralClamp (anti-windup) by clamping integral
    ppm   = -(Kp*errSamples + Ki*integral)
    ppm   = clamp(ppm, -MaxPPM, +MaxPPM)
    return HOLD, 1 + ppm*1e-6
```
RESEEK = drop ring, `Source.Seek(want)`, `Resampler.Reset()`, `DriftLoop.Reset()`,
rebaseline counters to `want`. Only on startup / underrun / gross error.

> **Convergence test MUST model the real plant** (the mpvsync bug was a test that
> didn't): the DAC drains OUTPUT frames at the crystal rate `(1+driftPPM)`; the ratio
> sets the SOURCE→OUTPUT mapping, so `srcConsumed` advances at `output_rate / ratio`.
> A faithful sim drives `errSamples` to a sub-ms steady state for any drift in
> ±100 ppm; a sign error or output-domain `played` will fail it.

## A.5 Per-group master election

Per **group** (not cluster-wide): election honors a **soft** `MasterHint` preference,
falling back to the lowest stable node id among the group's currently-alive members; a
generation counter fences stale masters.

```
candidates = { n in group.MemberNodeIDs : membership.alive(n) }
master     = (group.MasterHint in candidates) ? group.MasterHint   // soft preference: hinted node iff alive & member
                                              : min(candidates by node id)   // else lowest stable id => stable choice
if master changed: generation++                 // new master rebaselines clock+stream (new streamGen)
I_am_master = (master == self.id)
```
`MasterHint` is a **soft preference**, not a lock: it is honored only while the hinted
node is alive and still a group member; otherwise the deterministic lowest-id rule
applies, keeping the choice split-brain-safe. **Rejoin-flip cost:** when a hinted node
rejoins and reclaims mastership, that master change bumps `streamGen` (receivers flush +
re-prime, ~one buffer) — the price of honoring the hint.

Trigger on membership change (join/leave/suspect) and on `ConfigDoc.Groups` change.
Split-brain: two partitions may each elect a master; on heal, the lower-id master wins
and the other steps down (its higher generation is superseded by id order). A node with
no alive group peers is its own master (solo).

## A.6 Config replication — gossip LWW + grow-only revoked set

```
Apply(update):                     // local edit via API
    require update.Version == doc.Version   // optimistic; else 409
    next = clone(update); next.Version++; next.UpdatedBy = self.id
    doc = next; persist(next); signalChanged()
Merge(remote):                     // gossip anti-entropy
    take = remote.Version > doc.Version
         || (remote.Version == doc.Version && remote.UpdatedBy > doc.UpdatedBy)   // deterministic tiebreak
    body = take ? remote : doc
    body.RevokedSet = union(doc.RevokedSet, remote.RevokedSet)   // ALWAYS grow-only union, regardless of `take`
    if changed { doc = body; persist; signalChanged() }
```
Persistence: atomic temp-write + rename, mode `0600` (carries the CA key + hashes).
On rejoin, the highest `Version` wins; the `RevokedSet` is the union seen anywhere, so
a forgotten cert can never be resurrected by a stale replica.

## A.7 SPSC ring (jitter buffer)

```
NewRing(capSamples): fixed []float32, head/tail, mutex+cond
Write(p)  -> n   // copies up to free space; short when full (producer backs off)
Read(p)   -> n   // copies up to available; short when empty (caller pads silence)
Len()/Cap()/Reset()
```
Sized to `LeadMs` of audio: `cap = LeadMs/1000 * rate * channels` (+ headroom for the
FEC interleave span, A.10).

## A.8 Allowlist gate

```
allowed = ∪ NodeRecord.Addrs (all nodes in ConfigDoc) ∪ live-member addrs (membership)
recompute on state.Changed(); install as a per-packet source-IP check on the clock+audio
UDP sockets; drop (no reply) packets from any source not in `allowed`.
```
This is the ONLY protection on the unauthenticated realtime planes; keep `Addrs` fresh
(a node writes its own observed addrs into its `NodeRecord` on startup / IP change).

---

## A.9 Adoption PIN exchange — concrete primitive

> **NORMATIVE (R1):** this A.9 scheme (X25519 ECDH for confidentiality + PIN-keyed
> HMAC-SHA256 for auth + ChaCha20-Poly1305) is THE adoption handshake. [03](./03-adoption-takeover-security-pki.md)
> defers to it (its HKDF(PIN)+HMAC / PIN-only-AEAD scheme is deleted).

Goal on the **plaintext bootstrap channel** (no mTLS yet): a passive eavesdropper must
not learn the CA bundle; an active MITM must not be able to get a cert signed or
impersonate the controller — bounded only by *online* PIN guessing (acceptable for a
4-digit placeholder under the limited LAN threat model; upgrade to a PAKE later).

**Scheme = ephemeral ECDH (confidentiality) + PIN-keyed HMAC over the transcript (authentication):**
```
primitives (all Go stdlib / x/crypto, pinned A.11):
  X25519 (crypto/ecdh) · HKDF-SHA256 (x/crypto/hkdf) · HMAC-SHA256 (crypto/hmac)
  ChaCha20-Poly1305 (x/crypto/chacha20poly1305)

node (uninit) has PIN p (default "0000"); controller is given p out-of-band by the operator.
1. node:       generate ECDH ephemeral (nA, NA); show/hold PIN p; expose GET /bootstrap/info
2. controller: generate ECDH ephemeral (nB, NB); POST /bootstrap/adopt { NB, nonceB }
3. node reply: { NA, nonceA }
4. both:  Z = X25519(own_priv, peer_pub)
          k = HKDF(Z, salt = nonceA||nonceB, info = "ensemble-adopt-v1")
          kp = HKDF(p,  salt = nonceA||nonceB, info = "ensemble-pin-v1")   // PIN-derived key
          transcript = NA||NB||nonceA||nonceB
5. controller -> node:  { csr_request, tag = HMAC(kp, transcript || "req") }
   node verifies tag (rejects if PIN wrong / MITM) -> returns AEAD_k( CSR )           // encrypted under Z
6. controller signs CSR with cluster CA -> node:
                          AEAD_k( signedCert || caBundle || clusterSecrets )          // confidential
                          + tag2 = HMAC(kp, transcript || "done")
   node verifies tag2, decrypts, installs cert+CA -> joins mTLS mesh.
```
- **Passive eavesdropper:** sees only ECDH publics + ciphertext → cannot derive `Z` →
  cannot read the CA bundle. (A 4-digit PIN *is* offline-brute-forceable from the
  captured HMAC tags → it could then verify guesses offline; this is the known weakness
  of a short non-PAKE secret. Residual risk accepted per [03 §10](./03-adoption-takeover-security-pki.md);
  swap steps 4–5 for **CPace/SPAKE2** to remove it.)
- **Active MITM (no PIN):** cannot produce valid `tag`/`tag2` → both sides abort.
- **Hardening (adoption guard, see A.12):** soft backoff after **3** consecutive fails;
  hard **15-min lockout after 10 fails per 5 min** on `/bootstrap/adopt`, so online PIN
  guessing is infeasible even at 4 digits; single-use nonces; **nonce TTL 30 s**.

---

## A.10 Streaming & FEC — concrete schemes

**XOR-parity (default).** Group `k` source packets; emit 1 repair = XOR of their
payloads (padded to max len). Recovers any **single** lost packet per group. **Interleave**
parity groups across time so a burst hits ≤1 packet per group:
```
k = 8 source packets per parity group
interleave depth D = 4  -> parity for group g covers packets {g, g+D, g+2D, ...}
recoverable burst ≈ D packets ≈ D * FramesPerChunk/rate = 4*10ms = 40 ms
buffer lead must exceed the recovery span: LeadMs (300) >> 40 ms ✓
```
**Duplication (MCU-friendly alt).** Send each source packet twice, the copy offset by
`Ddup` packets; receiver dedupes by `seq`. Zero decode math; cost = 2× the codec bitrate.
`Ddup = 5` packets (~50 ms burst tolerance).

**Receiver pipeline:** udp recv → allowlist check → FEC.Recover → reorder by `seq` (drop
packets already past playout) → Codec.Decode → push to ring at `sampleIndex`. A gap that
FEC can't fill → conceal with silence for that span (and if it exceeds `HardErrSamp`,
the drift loop reseeks).

**Late join:** the master starts a new listener at the next chunk boundary at the current
`sampleIndex`; for Opus it sends a keyframe/independent frame first (`flags` bit1).

**Wire PCM format (m5):** the PCM wire codec is **S16LE** (signed 16-bit little-endian,
interleaved). The internal pipeline is `float32`; convert **f32↔s16 at the wire
boundary** only (encode on the master side, decode on the follower side). There is no
profile field for bit depth.

---

## A.10b Built-in calibration signal (R10)

A **built-in** calibration signal, generated **in-process** at the canonical rate (no
external file), so every node emits a bit-identical waveform. Period = **1 s**, repeated:

```
per 1-second period (canonical rate, e.g. 48000):
  [0,   ~1 ms)    full-scale click   (single full-amplitude sample / ~1 ms impulse)
  [~1 ms, ~201 ms) 1 kHz sine tone   (~200 ms)
  [~201 ms, 1 s)  silence
identical across nodes (same generator, same seed-free deterministic samples)
```
Played synchronously on selected nodes via `POST /api/v1/calibrate/play`
({groupId | nodeIds, durationSec}); the click + tone give a sharp transient for
ear/phone-mic offset judgement (and, later, cross-correlation measurement).

---

## A.11 Pinned dependencies (Go modules)

| Concern | Module | Notes / version intent |
|---|---|---|
| mDNS discovery | `github.com/grandcat/zeroconf` | as in mpvsync |
| Membership/gossip | `github.com/hashicorp/memberlist` | LAN config preset |
| WebSocket (UI live) | `github.com/coder/websocket` | UI status push |
| YAML config | `gopkg.in/yaml.v3` | — |
| Argon2id | `golang.org/x/crypto/argon2` | admin pw + PIN hash |
| HKDF / ChaCha20-Poly1305 | `golang.org/x/crypto/{hkdf,chacha20poly1305}` | adoption (A.9) |
| X25519, TLS, x509 | Go stdlib `crypto/ecdh`, `crypto/tls`, `crypto/x509` | mTLS + adoption |
| ALSA precise sink | Go stdlib `golang.org/x/sys/unix` | **direct kernel ioctl ALSA** on `/dev/snd/pcmC*D*p` (pure-Go, no cgo, no dlopen); ioctls incl. `SNDRV_PCM_IOCTL_DELAY` / `SYNC_PTR`. ⚠️ **precise mode is a verification spike** (prove on a Pi + DAC HAT). |
| MP3 decode | `github.com/hajimehoshi/go-mp3` | pure-Go; master source decode |
| FLAC decode | `github.com/mewkiz/flac` | pure-Go (proven in mpvsync); **decode only** |
| Opus enc/dec | libopus binding **deferred** (optional, off critical path) | optional capability; **purego/dlopen** is the candidate binding when pursued (cgo `gopkg.in/hraban/opus.v2` the fallback). Not committed now. |

> **Codec roles (R2):** the **source decoders** are `go-mp3` (mp3) + `mewkiz/flac` (FLAC)
> + WAV, input from a local **file or an HTTP(S) stream**. The **wire codec set is
> `{PCM, Opus}` only**; PCM is the mandatory baseline (no encode), Opus enc/dec is
> **optional** with its libopus binding **deferred** (off the critical path). **FLAC is
> NOT a wire codec — there is no FLAC encoder and no FLAC-encode spike.**
>
> **No cgo, no dlopen on the default build target (R7):** the precise ALSA sink talks
> **directly to the kernel** via `x/sys/unix` ioctls on `/dev/snd/pcmC*D*p` — pure-Go,
> single binary, cross-compiles to arm64 with no C toolchain. `purego`/dlopen is **not**
> on the ALSA path; it may only resurface later for the *optional* Opus binding.

---

## A.12 Starting parameters

| Domain | Parameter | Start value |
|---|---|---|
| Audio canonical | rate / channels / FramesPerChunk | 48000 / 2 / 480 (10 ms) |
| Buffer | playout lead `LeadMs` | 300 ms |
| Drift PI | Kp / Ki / MaxPPM / IntegralClamp | 0.05 / 0.005 / 200 ppm / 200 |
| Drift PI | tick / HardErrSamp | 20 ms / 2400 samples (50 ms) |
| Clock | window N (wired·WiFi) / alpha (wired·WiFi) / ping | 8·16 / 0.15·0.10 / 1 s (500 ms initial lock) |
| Clock RPC | RPC timeout | 200 ms |
| FEC XOR | k / interleave depth D | 8 / 4 (~40 ms burst) |
| FEC dup | offset `Ddup` | 5 packets (~50 ms) |
| PKI | leaf lifetime / renew-at | 30 days / ⅓ life (day 10) |
| Adoption guard | soft backoff / hard lockout / nonce TTL | backoff after 3 consecutive fails / 15-min lockout after 10 fails per 5 min / nonce TTL 30 s |
| Ports | control mTLS / clock UDP / audio UDP | 8443 / 9000 / 9100 |

All are starting points; the drift PI gains, FEC `k/D`, and `LeadMs` need empirical
tuning on the target WiFi/hardware (see [10](./10-roadmap-and-dumb-nodes.md) P4–P5).

---

## A.13 Per-phase Definition of Ready / acceptance

| Phase | Ready when… | Acceptance test |
|---|---|---|
| P0 skeleton | cmd builds, data dir + identity persist | `go build`, restart keeps node id |
| P1 PKI+auth | CA self-init, mTLS peer dial, login/session/apikey | two procs mTLS-handshake; bad cert rejected; setup wizard sets admin pw |
| P2 cluster | discover + adopt (PIN) + ConfigDoc replicate + allowlist + forget | node B adopted from A; doc converges; forgotten node's packets dropped + cert rejected |
| P3 clock+group | per-group offset < 1 ms (wired); election + failover | kill master → re-elect < 2 s, followers re-lock |
| P4 sync (PCM) | one group plays a PCM stream in sync; drift loop holds | measured inter-node offset stays sub-ms over 10 min (test clip + analyzer) |
| P5 codec+FEC | wire PCM(S16LE)+optional Opus + XOR/dup + profile negotiation | induced 2 % packet loss → no audible dropout |
| P6 multi-group+UI | groups, channel roles, media UI, node calibration | L/R stereo pair forms a stable image; per-node delay trim works |
| P7 takeover+hardening | takeover, cert rotation, partition recovery | takeover supersedes old cert; 24 h soak holds sync |

---

## A.14 Cluster/runtime glue (R8)

The pieces below were previously specified only as "reuse mpvsync" pointers. They are
inlined here as concrete Go contracts + short pseudocode so the spec is self-contained.
Names match [README §6](./README.md); pseudocode is normative-by-intent.

### A.14.1 `PeerStore` / `peers.json`

Persisted seed list for rejoining the gossip mesh across restarts (memberlist itself is
in-memory). Lives in `internal/cluster`.

```go
type PeerStore interface {
    Upsert(addr string)        // record a seen peer "host:7946"; dedup; persist
    JoinSeeds() []string       // all known seed addrs to feed memberlist.Join()
    Clear()                    // wipe (e.g. on leave/forget-self)
}
```
```
Upsert(addr):  if addr not in set { set += addr; atomicWriteJSON(path, sorted(set)) }
JoinSeeds():   return sorted(set)            // caller passes to memberlist.Join, ignores
                                             // partial-join errors as long as >=1 succeeds
Clear():       set = {}; atomicWriteJSON(path, [])
```
On-disk format — `peers.json` (mode `0644`, atomic temp-write+rename):
```json
{ "peers": [ "10.0.0.11:7946", "10.0.0.12:7946" ] }
```

### A.14.2 memberlist wrapper

Wraps `hashicorp/memberlist` with a LAN preset and our delegate. Carries the ConfigDoc
over push/pull anti-entropy and surfaces membership events to the election + allowlist.

`memberlist.Config` fields we set (from `memberlist.DefaultLANConfig()`):
```
Name                 = self node id
BindAddr / BindPort  = 0.0.0.0 / 7946            // gossip/seed port (m2)
AdvertiseAddr/Port   = observed LAN ip / 7946
GossipInterval       = 200 ms                    // LAN preset
ProbeInterval        = 1 s
ProbeTimeout         = 500 ms
Delegate             = &delegate{...}            // meta + user msgs + state push/pull
Events               = &events{...}              // NotifyJoin/Leave/Update -> election+allowlist
```
The `Delegate` methods:
```go
type delegate struct { meta Meta; doc *state.Store; bus *busybox }

func (d *delegate) NodeMeta(limit int) []byte
    // marshal Meta{ctrl, clk, aud ports + node id} (R6 keys), truncated to limit

func (d *delegate) NotifyMsg(b []byte)
    // handle best-effort user broadcasts (none required for MVP; reserved)

func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }

func (d *delegate) LocalState(join bool) []byte
    // return d.doc.Snapshot() marshalled  -> carries the ConfigDoc to the peer

func (d *delegate) MergeRemoteState(buf []byte, join bool)
    // unmarshal peer ConfigDoc -> d.doc.Merge(remote)   (LWW + grow-only RevokedSet, A.6)
```
The `Events` (`memberlist.EventDelegate`) methods:
```go
func (e *events) NotifyJoin(n *memberlist.Node)    // add addr -> allowlist; re-run election
func (e *events) NotifyLeave(n *memberlist.Node)   // drop -> allowlist; re-run election
func (e *events) NotifyUpdate(n *memberlist.Node)  // meta/addr change -> refresh ports + allowlist
```
Each event recomputes the allowlist (A.8) and triggers per-group election (A.5).

### A.14.3 `web.Deps` function-value seam

`web` depends only on a struct of function values (no concrete cluster/group imports), so
handlers are testable and the wiring lives in `cmd`. Fields `web` consumes:

```go
type Deps struct {
    // read-mostly snapshots
    State      func() ConfigDoc                       // current replicated config
    Transcodes func() []TranscodeStatus                // per-group stream/transcode status snapshots
    Discovery  func() []Discovered                     // discovered-but-unadopted nodes

    // cluster mutations (proxied node->node over mTLS as needed)
    Adopt      func(addr, pin string) error            // run A.9 against a target
    Forget     func(nodeID string) error               // revoke + drop from ConfigDoc
    Leave      func() error                             // coordinated self-forget (cluster/leave)

    // node/group config
    SetNodeConfig func(nodeID string, patch NodePatch) error  // channel/gain/HWDelayUs/name

    // calibration
    CalibratePlay func(sel CalibrateSel, durationSec int) error // play A.10b signal synchronously
}
```
Handlers call only these; `cmd` supplies implementations bound to `cluster`, `group`,
`state`, and `pki`.

### A.14.4 `applyRole` loop

Runs per group on every election/membership/config change; idempotently starts or stops
the role-specific subsystems, fenced by `generation` / `streamGen` so stale roles can't
emit on the wire.

```
applyRole(group, electedMaster, generation, streamGen):
    iAmMaster = (electedMaster == self.id)

    if iAmMaster and not running.master:
        stop_follower_parts()                       // clock-follower + receiver, if any
        start clock_server(group)                   // A.1 reply side
        start stream_origin(group, streamGen)       // decode->encode(PCM/Opus)->FEC->unicast (A.10)
        origin.ResumeAt(group.sampleIndex, group.Playing)   // R4: resume timeline + Playing
        running = master

    else if (not iAmMaster) and not running.follower:
        stop_master_parts()                         // clock-server + stream-origin, if any
        start clock_follower(group, master=electedMaster)   // A.1 request side
        start stream_receiver(group)                // recv->FEC.Recover->decode->ring (A.10)
        receiver.FlushAndReprime()                  // R11: ~one buffer on (re)start
        running = follower

    // generation fence: any clock/audio packet emitted carries `generation`/`streamGen`;
    // a subsystem started under an older generation is torn down before the new one starts,
    // so a superseded master cannot keep sending on the group's planes.
```
On a master change the new master bumps `streamGen` (A.5 / R11); receivers seeing a new
`streamGen` flush + re-prime, preserving `Playing` and resuming at the current
`sampleIndex`.
