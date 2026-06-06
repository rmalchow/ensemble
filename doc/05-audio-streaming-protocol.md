# 05 — Audio streaming protocol

> **Scope.** The end-to-end realtime audio pipeline of a group: how the **master**
> (group **stream origin**) turns one decoded media stream into timestamped,
> error-protected packets and pushes them **unicast** to every listener, and how each
> listener's **stream receiver** turns those packets back into sample-accurate PCM in
> its jitter buffer. This document elaborates locked decisions **D2** (UDP transport +
> TCP fallback), **D3** (pluggable codec), **D4** (light FEC), **D5** (decode-once,
> unicast-per-listener) and the wire format **§6.4** and interfaces **§6.3** from the
> [spine](./README.md). It does **not** redefine those contracts.
>
> **Boundaries (read alongside).**
> - **Upstream:** the group engine, profile negotiation, per-group **`Timeline`**
>   (`NowSample`) and clock offset come from [04-clock-and-groups.md](./04-clock-and-groups.md).
>   This document consumes `Timeline`/`ClockSource` (§6.2); it does not implement them.
> - **Downstream:** what happens *after* a sample lands in the ring — hardware-buffer
>   scheduling, the content-domain drift PI loop, channel/gain/`HWDelayUs` — is
>   [06-audio-output-scheduling.md](./06-audio-output-scheduling.md). This document hands
>   off at "push to ring with sampleIndex" and never touches the `AudioSink`.
> - **Security:** the **allowlist** source-IP gate on the audio socket is specified in
>   [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md) and
>   derived from `ConfigDoc.Nodes[].Addrs` per [07-config-and-replication.md](./07-config-and-replication.md)
>   §6.5. This document only states *where* the gate sits in the receive path.

---

## 5.0 Pipeline at a glance

```
            MASTER (group stream origin, internal/stream/origin)
 ┌───────────────────────────────────────────────────────────────────────────┐
 │  data/*.mp3 / *.flac / *.wav  (or HTTP(S) stream)                           │
 │     │  source.Reader (decode → PCM @ canonical rate, loop at EOF)           │
 │     ▼                                                                       │
 │  [ float32 PCM stream, interleaved, canonical rate ]                        │
 │     │  chunker: slice FramesPerChunk (=480 → 10 ms @48k)                     │
 │     ▼                                                                       │
 │  Codec.Encode(pcm) ──► payload bytes        (PCM | Opus, §6.3)              │
 │     │                                                                       │
 │     │  wire.Marshal(header{seq, sampleIndex, masterMono, streamGen,…})      │
 │     ▼                                                                       │
 │  FEC.Protect(seq, pkt) ──► [source pkt, (repair pkt)]   (XOR | Dup, §6.3)   │
 │     │                                                                       │
 │     ▼   per-listener unicast UDP sockets, paced to playout − bufferLead     │
 │   ┌──────────────┬──────────────┬──────────────┐                           │
 └───┼──────────────┼──────────────┼──────────────┼───────────────────────────┘
     │ unicast UDP  │ unicast UDP  │ unicast UDP  │
     ▼              ▼              ▼              ▼
  listener N2    listener N3    listener N4    (… one socket each, D5)
 ┌───────────────────────────────────────────────────────────────────────────┐
 │  FOLLOWER (stream receiver, internal/stream/sink_net)                       │
 │  udp recv → allowlist gate (03) → wire.Unmarshal                            │
 │     │                                                                       │
 │     ▼  FEC.Recover(p) ──► recovered source packets                          │
 │     │                                                                       │
 │     ▼  reorder/dedupe by seq (bounded window)                               │
 │     │                                                                       │
 │     ▼  Codec.Decode(payload) ──► float32 PCM                                │
 │     │                                                                       │
 │     ▼  ring.PushAt(sampleIndex, pcm)   →  hand off to 06 (render/sink)       │
 └───────────────────────────────────────────────────────────────────────────┘
```

The defining property (D5): the master decodes the compressed media **exactly once**
and encodes it **once per codec**, then performs a cheap copy/send per listener. CPU
on the master is O(codecs in use), not O(listeners); only the kernel send and the FEC
copy are per-listener. There is no mixing and no per-listener transcoding.

---

## 5.1 Canonical rate, chunking, and the unit of transport

The group operates at a single **canonical sample rate** (default **48000 Hz**),
negotiated once per group (see [04](./04-clock-and-groups.md) profile negotiation) and
carried for sanity in the wire header field `rate/100` (§6.4: value `480` ⇒ 48000 Hz).
All sample indices on the group `Timeline` (§6.2) are expressed in **canonical-rate
frames** regardless of codec.

A **chunk** is the atomic unit of the protocol: exactly `FramesPerChunk` frames
(default **480**, i.e. **10 ms @ 48 kHz**) of PCM, which become **one codec frame**,
which become the payload of **one source packet** (§6.4). `FramesPerChunk` is fixed for
the lifetime of a `streamGen`; it does not vary per chunk.

| Quantity | Default | Notes |
|---|---|---|
| Canonical rate | 48000 Hz | group-negotiated; in header as `rate/100` |
| `FramesPerChunk` | 480 frames | 10 ms; one codec frame; one packet payload |
| Channels | 2 (stereo, interleaved) | master always sources stereo; per-node channel select is in [06](./06-audio-output-scheduling.md) |
| Packet cadence | 100 packets/s/listener | 1000 ms ÷ 10 ms |

**Why 10 ms.** It is the natural Opus frame size, it is a clean PCM block, and at
100 pkt/s it keeps per-packet header overhead (44 B, §6.4) tolerable while keeping the
recovery/concealment granularity small. Larger chunks lower overhead but raise loss
granularity and FEC latency; smaller chunks raise packet rate and header tax. 480 is the
locked default; the field is in the header so a future profile can renegotiate it.

### Frame index arithmetic

For a source packet with sequence `seq` whose first frame is `sampleIndex`:

```
sampleIndex(seq)      = sampleIndex(0) + seq * FramesPerChunk
nextSampleIndex       = sampleIndex + FramesPerChunk
playoutInstant(idx)   = Timeline maps idx → master-mono ns (see 04/06)
```

The receiver never *computes* `sampleIndex` from `seq`; it **reads it from the header**
(§6.4 offset 24). This makes the stream robust to the master skipping or restarting
(`streamGen` change, §5.8): the index is authoritative, the sequence is only for FEC and
reordering.

---

## 5.2 Master origin — the decode/encode/send loop (`internal/stream/origin`)

The origin runs only while this node is the group **master** and the group has selected
media that is playing (D14: pick an mp3 from `data/`, loop). It owns three cooperating
stages connected by bounded channels:

1. **Source/decode** (`internal/stream/source`) — produces canonical-rate PCM.
2. **Encode/frame/protect** — turns PCM into wire packets (+ repair).
3. **Paced unicast send** — fans packets out to each listener at the right time.

```go
// internal/stream/origin
type Origin struct {
    tl     clock.Timeline   // §6.2 — group playout timeline (from 04)
    codec  codec.Codec      // §6.3 — negotiated for the group (D3)
    fec    fec.FEC          // §6.3 — negotiated for the group (D4)
    src    source.Reader    // §5.3 — source decode (mp3/FLAC/WAV) → PCM, looped
    gen    uint64           // current streamGen (§6.4, §5.8)
    lead   time.Duration    // buffer lead (default ~300 ms, §5.7)
}

// Run drives the loop until ctx is cancelled or the node loses mastership.
func (o *Origin) Run(ctx context.Context) error

// Listener registry is mutated by the group engine as members join/leave.
func (o *Origin) AddListener(id string, addr *net.UDPAddr) error
func (o *Origin) RemoveListener(id string) error
```

### 5.2.1 Decode loop and looping (D14)

`source.Reader` (next section) yields a continuous interleaved `float32` PCM stream at
the canonical rate. At media EOF it **loops** seamlessly back to the first frame. Looping
does **not** reset `sampleIndex` and does **not** bump `streamGen`: the timeline is a
single monotonic counter, so a loop boundary is invisible to listeners (it is just more
PCM). `streamGen` only changes on an *operator* media change or seek (§5.8).

### 5.2.2 Chunk, encode, frame, protect

For each successive `FramesPerChunk`-frame slice of PCM, in order:

```
pcm   := next FramesPerChunk frames (interleaved float32)
payload, _ := codec.Encode(pcm)              // §6.3 ; may set keyframe (§5.4)
hdr   := wire.Header{
    Magic: 'ESND', Version: 1,
    Flags: keyframe?,                        // bit1 keyframe (§6.4)
    CodecID: codec.ID(), FECID: fec.ID(),
    StreamGen: o.gen,
    Seq: seq,                                // monotonic source seq
    SampleIndex: idx,                        // canonical-rate frame index
    MasterMono: nowMonoNs(),                 // master monotonic clock at source
    PayloadLen: len(payload), Rate100: rate/100,
}
pkt   := wire.Marshal(hdr, payload)
out   := fec.Protect(seq, pkt)               // §6.3 → [source, (repair…)]
enqueue(out, playoutInstant(idx) - o.lead)   // pace to send time (§5.6)
seq++; idx += FramesPerChunk
```

`MasterMono` is the master's monotonic timestamp **at the moment the chunk was sourced**,
not when it is sent; together with the clock plane (D6) it lets the receiver and the
drift loop ([06](./06-audio-output-scheduling.md)) relate stream content to the shared
timebase. `SampleIndex` is the playout anchor (§5.1).

### 5.2.3 Single encode, fan-out copy (D5)

`codec.Encode` and `fec.Protect` run **once per chunk**, not per listener. The resulting
`[][]byte` (source + repair packets) is then written to each listener's unicast socket.
Only the per-socket `WriteToUDP` (a copy into the kernel) is O(listeners). This is the
crux of D5's CPU claim. Repair packets (XOR) and duplicate packets (Dup) are likewise
produced once and fanned out.

---

## 5.3 Source / decode (`internal/stream/source`)

```go
// internal/stream/source
type Reader interface {
    // Read fills dst with up to len(dst) interleaved float32 frames at the
    // canonical rate; loops at EOF; never returns io.EOF while looping.
    Read(dst []float32) (frames int, err error)
    Rate() int           // canonical rate (e.g. 48000)
    Channels() int       // 2
}

// Open decodes a source file (mp3/FLAC/WAV) from the data/ folder — or an
// HTTP(S) stream URL — and returns a looping Reader.
func Open(path string, canonicalRate, channels int) (Reader, error)

// List enumerates playable media in the data/ folder (drives the Media UI, 09).
func List(dataDir string) ([]MediaInfo, error)
```

**Source-decode formats vs. wire codecs.** The source layer is the **master's input**
decode stage, entirely separate from the wire `Codec` interface (§5.4). It accepts the
**source-decode set `{mp3, FLAC, WAV/PCM}`** — master input from a local file in `data/`
**or an HTTP(S) stream** — and always emits canonical-rate `float32` PCM. FLAC, mp3 and
WAV are therefore **source formats only**, never wire codecs; FLAC **decode** here
(`mewkiz/flac`, pure-Go) is unrelated to the wire codec set `{PCM, OPUS}` (R2).

Responsibilities:

- **Decode source → PCM.** Decode mp3/FLAC/WAV to interleaved `float32`. Sources are
  typically 44.1 kHz; the reader resamples to the **canonical** rate so the rest of the
  pipeline is rate-uniform. (This is content resampling at the source, distinct from the
  per-node drift resampler in [06](./06-audio-output-scheduling.md), which corrects clock
  skew at playout.)
- **Loop at EOF** (D14) by seeking to frame 0 and continuing; partial final chunks are
  padded with the loop's leading frames so every emitted chunk is exactly
  `FramesPerChunk` (no short frames on the wire).
- **`data/` browsing** via `List` for the Media screen ([09](./09-ui-screens.md)); the
  selected file + loop flag live in `ConfigDoc.Groups[].media` ([07](./07-config-and-replication.md)).

The source is wire-codec-agnostic: it always emits canonical-rate stereo PCM. The wire
codec layer (§5.4) is the only thing that knows about PCM/Opus framing; source decode
formats (mp3/FLAC/WAV) live entirely here and never reach the wire.

---

## 5.4 Codecs (`internal/stream/codec`) — D3

All codecs implement the spine interface verbatim (§6.3):

```go
type Codec interface {
    ID() CodecID                          // PCM=0, OPUS=1
    Encode(pcm []float32) ([]byte, error) // master side; one chunk in, one frame out
    Decode(payload []byte) ([]float32, error) // follower side
}
```

The **wire codec set is `{PCM, OPUS}`** (R2): PCM is the **mandatory baseline** (no
encode, always works), Opus is **optional and capability-gated** for low-bandwidth
links. There is **no FLAC wire codec** — FLAC is a source-decode format only (§5.3). The
group's codec is **negotiated to the least-capable member** (D3) using `NodeRecord.Caps`
(§6.5); the baseline is **PCM**, with Opus selected when every member advertises it.
Negotiation itself is in [04](./04-clock-and-groups.md); this section specifies each
codec's framing and the decode-once tradeoffs (D3).

### 5.4.1 PCM (`CodecID = 0`) — no compression, mandatory baseline

- **Framing.** PCM on the wire is **`S16LE` (16-bit signed little-endian), fixed** (m5):
  `Encode` writes the 480 stereo frames as little-endian interleaved `int16` samples.
  There is **no profile field for bit depth** and no f32-vs-int16 selection. The internal
  pipeline stays `float32`; the **f32↔s16 conversion happens at the wire boundary**
  (`Encode` narrows f32→s16, `Decode` widens s16→f32). No inter-frame state.
- **Independence.** Every chunk is self-contained: every packet is implicitly a keyframe
  (the keyframe flag is set on every PCM packet so the join path §5.6.4 is uniform).
- **Use.** Lowest CPU, highest bandwidth; the mandatory safe floor and the dumb-node
  baseline (a microcontroller can `memcpy` S16LE PCM). Also the fallback when Opus
  negotiation fails.

### 5.4.2 Opus (`CodecID = 1`) — lossy, low bandwidth, optional

- **Framing.** One Opus frame per chunk at 10 ms. Opus is constant-frame-duration here;
  the encoder is configured for **`OPUS_APPLICATION_AUDIO`**, the negotiated bitrate, and
  **no inter-frame dependency we cannot resync** — but Opus *does* carry encoder state
  (prediction, PLC warm-up). To bound the cost of loss and enable join, the origin marks a
  **keyframe** (§6.4 bit1) periodically and on join: at a keyframe the encoder is reset so
  the frame is decodable cold. Default keyframe interval **every 50 chunks (~500 ms)**;
  forced immediately for a late joiner (§5.6.4).
- **Lossy / low-bw.** Smallest packets by far (tens of kbit/s); best for constrained WiFi
  or future low-bandwidth links. Higher CPU than PCM on both encode and decode.
- **Concealment.** Opus has native PLC; on an unrecovered loss the receiver may call the
  decoder's PLC for one frame (§5.6.3) instead of inserting silence.

### 5.4.3 Codec tradeoffs (D3)

| Codec | Loss / fidelity | Bandwidth | Master CPU | Decoder CPU | Join warm-up | Inter-frame state |
|---|---|---|---|---|---|---|
| **PCM** (S16LE) | lossless | highest | ~none | ~none | none (always keyframe) | none |
| **Opus** | lossy | lowest | high | medium | until next keyframe (forced on join) | yes (reset at keyframe) |

> **No Reed–Solomon.** Per D4, the system never uses Reed–Solomon FEC. Likewise the codec
> layer avoids any scheme that needs heavy DSP to *recover*; this is a deliberate
> constraint so the protocol can admit future microcontroller dumb nodes
> ([10](./10-roadmap-and-dumb-nodes.md), D15). See §5.5.4.

---

## 5.5 Forward error correction (`internal/stream/fec`) — D4

UDP (D2) drops and reorders packets, especially on WiFi. FEC trades a little bandwidth and
latency to recover losses **without retransmission** (which would blow the real-time
budget). Both schemes implement §6.3 verbatim:

```go
type FEC interface {
    ID() FECID                                  // None=0, XORParity=1, Duplicate=2
    Protect(seq uint64, pkt []byte) (out [][]byte) // master: source(+repair) packets
    Recover(p Packet) (recovered []Packet)         // follower: newly-recoverable packets
}
```

The master calls `Protect` per source packet and unicasts every returned packet (§5.2.3).
The receiver calls `Recover` per received packet; it returns zero or more source packets
that became decodable (including the just-received one if it is itself a source packet).
`FECID = None` (0) is the identity: `Protect` returns `[pkt]`, `Recover` returns the
source packet as-is.

### 5.5.1 XOR-parity (`FECID = 1`) — the default

Group **k** consecutive source packets; emit **one repair packet** that is the byte-wise
**XOR of those k payloads** (with the repair header's `flags` bit0 set, §6.4). If **any
one** of the k+1 packets in a group is lost, the receiver reconstructs it by XOR-ing the
other k. Two losses in the same group are unrecoverable by XOR.

```
group g (k=8):  S0 S1 S2 S3 S4 S5 S6 S7   repair R = S0⊕S1⊕…⊕S7
                 │  │  │  │  │  │  │  │          │
   send: ─ S0 … S7 ─ R ─                   (R after the group, or interleaved §5.5.3)
   loss of S2  ✗  → recover S2 = S0⊕S1⊕S3⊕S4⊕S5⊕S6⊕S7⊕R
```

Repair payloads must be equal length to XOR; the encoder zero-pads the shorter payloads
within a group to the group's max payload length and stores `payloadLen` per source
packet so the receiver can trim after reconstruction. The repair packet's own
`payloadLen` is the padded length; the repair carries the **list of covered seqs**
implicitly (contiguous: `[firstSeq … firstSeq+k-1]`, derivable from the repair's `seq`
and the group size, which is fixed for the `streamGen`).

**Overhead** = 1/k extra packets (e.g. k=8 ⇒ +12.5% packets and bandwidth). **Recovery
latency** is governed by the **interleave depth** `D` (§5.5.3): the receiver must wait
until enough of an interleaved group has arrived, ≈ `D × 10 ms` of added buffer lead. So
**k trades overhead** while **D trades burst tolerance against recovery latency**:

| k | Repair overhead | Recovers |
|---|---|---|
| 2 | +50% | any 1 loss / 2 src |
| 4 | +25% | any 1 loss / 4 src |
| **8** (default) | **+12.5%** | any 1 loss / 8 src |

Canonical **k = 8, D = 4** (Appendix A.12): +12.5% bandwidth, recovery span ≈ `D × 10 ms`
≈ **40 ms** — comfortably inside the ~300 ms buffer lead (§5.7).

### 5.5.2 Why XOR is the default and not duplication

XOR recovers a loss at **+1/k** bandwidth (default +25%) versus duplication's **+100%**,
while still recovering **single** losses — the dominant WiFi loss mode after a good AP. It
costs only XOR (one pass over k payloads) to both protect and recover — trivial compute,
dumb-node-friendly (§5.5.4).

### 5.5.3 Time-interleaving the parity groups (burst tolerance)

A naive XOR layout (`S0 … S7 R0  S8 … S15 R8 …`) is defeated by a **burst** loss:
losing `S2 S3` (two packets in one group, adjacent in time) is unrecoverable. The fix is
**time-interleaving**: assign source packets to **D** interleaved parity groups
round-robin by sequence, so temporally adjacent packets belong to **different** groups.

```
interleave depth D = 4, k = 8 per group:

 seq:   0   1   2   3   4   5   6   7   8   9  10  11  …  RA RB RC RD
 group: A   B   C   D   A   B   C   D   A   B   C   D   …  A  B  C  D
                                                          ▲ repair for each group
 burst loss of seq 4,5  →  hits group A (seq0,4,8,…) and group B (seq1,5,9,…)
                           = ONE loss per affected group  →  both recoverable
```

With depth **D**, a contiguous burst of up to **D** lost packets touches each group at
most once, so all are recoverable (as long as no single group loses ≥2). The cost is
**latency**: the receiver must hold packets spanning the interleave depth before a group
is complete enough to repair, so the recovery span is governed by **D**:

```
required FEC lead  ≈  recovery span  ≈  D × FramesPerChunk / rate
e.g. D=4, 10 ms chunks  →  ~40 ms of the buffer lead is FEC span
```

This is the direct link between **interleave depth and the buffer lead** (§5.7): deeper
interleave survives longer bursts but consumes more of the 300 ms lead. Canonical
defaults (Appendix A.12): **k = 8, D = 4** — +12.5% overhead, burst tolerance ~40 ms,
recovery span ~40 ms — leaving ample headroom under the 300 ms lead for jitter. The repair
packets for a group are emitted once the group's k source packets have been produced and
are themselves interleaved into the send order.

```go
// internal/stream/fec — illustrative config carried in the group profile (04/07)
type XORConfig struct {
    K          int // source packets per parity group (default 8)
    Interleave int // D: number of concurrent groups, depth (default 4)
}
```

### 5.5.4 Packet duplication (`FECID = 2`) — the MCU-friendly option

Send **each source packet twice**, the duplicate offset by **D packets** in the send
order; the receiver **dedupes by `seq`**. If either copy arrives, the packet is delivered.
**Zero decode math** — no XOR, no reconstruction, no group bookkeeping: a microcontroller
just discards a `seq` it already has.

```
D = 2 offset:
 send:  S0  S1  S0' S2  S1' S3  S2' S4  S3' …      (S0' is the duplicate of S0)
 burst loss of S1,S2 (adjacent)  →  S1' and S2' arrive D slots later  →  both recovered
```

- **Overhead.** +100% bandwidth (each packet sent twice). The price of zero compute.
- **Burst tolerance.** A burst shorter than the **D-packet offset** is fully covered,
  because the duplicate is displaced by `D × 10 ms` in time. Default **D = 4** ⇒ tolerates
  ~40 ms bursts; added lead ≈ `D × FramesPerChunk / rate` (~40 ms).
- **Dedupe.** The receiver keeps a small seen-`seq` window (§5.6.2) and drops duplicates;
  the *first* copy to arrive wins, so duplication also slightly reduces effective jitter.
- **Use.** The dumb-node / MCU default and the "I have bandwidth, not CPU" choice.

```go
type DupConfig struct {
    Offset int // D: packets between a source packet and its duplicate (default 4)
}
```

### 5.5.5 Why no Reed–Solomon (D4)

Reed–Solomon would recover multiple losses per group at lower overhead than duplication,
but its encode/decode needs Galois-field arithmetic and matrix inversion — far too heavy
for the **microcontroller dumb nodes** the protocol must admit (D15,
[10](./10-roadmap-and-dumb-nodes.md)). XOR-parity (single XOR pass) and duplication (a
hash-set membership test) are both implementable on an MCU with no DSP. The system
therefore caps recovery ambition at "any single loss per group" (XOR) or "any loss whose
duplicate survives" (Dup), and leans on the generous buffer lead and interleaving for
bursts. This is a locked-by-rationale (🟡 default) decision; if dumb nodes are dropped, RS
could be added behind the same `FEC` interface without touching the wire format.

### 5.5.6 FEC scheme comparison

| Scheme | Overhead | Recovers | Compute (protect/recover) | Added lead | MCU-friendly |
|---|---|---|---|---|---|
| **None** | 0% | nothing | none | 0 | yes |
| **XOR-parity** (k=8, D=4) | +12.5% | any 1 / group; bursts ≤ D | one XOR pass | ~40 ms span | yes |
| **Duplication** (D=4) | +100% | any loss whose dup survives; bursts < D | none (dedupe) | ~40 ms | yes (best) |
| ~~Reed–Solomon~~ | low | multi-loss | GF matrix math | — | **no — excluded** |

---

## 5.6 Send loop and the receiver

### 5.6.1 Per-listener unicast send (D5)

The master keeps **one UDP socket per listener** (or one socket and a destination map; the
contract is *unicast per listener*, not multicast — D5). On `AddListener`/`RemoveListener`
the group engine ([04](./04-clock-and-groups.md)) mutates the registry. Each protected
packet bundle (source + repair/duplicate) is written to **every** listener's address.

**Pacing.** Packets are *not* blasted as fast as encoded. Each chunk is scheduled to be
**sent** at `playoutInstant(sampleIndex) − bufferLead` (§5.7), so the master streams
roughly one chunk every 10 ms, ahead of playout by the lead. Pacing keeps the receiver's
jitter buffer near its target fill and avoids bursting the WiFi link. A monotonic ticker
(or a min-heap keyed by send-time for the interleaved repair packets) drives the writes.

Every packet stamps, from the spine header (§6.4): `seq` (monotonic source sequence),
`sampleIndex` (playout anchor), `masterMono` (source instant), `streamGen` (current
generation, §5.8), plus `codecID`/`fecID`/`rate100` and the keyframe flag where relevant.

### 5.6.2 Receiver pipeline (`internal/stream/sink_net`)

```go
// internal/stream/sink_net
type Receiver struct {
    codec codec.Codec
    fec   fec.FEC
    ring  *ring.Ring     // jitter buffer (audio/ring), pushed by sampleIndex
    allow allowlist.Gate // §6.5 / 03 — source-IP gate
    gen   uint64         // accepted streamGen; mismatches flush+reset (§5.8)
}
func (r *Receiver) Run(ctx context.Context, conn *net.UDPConn) error
```

Per received datagram, in order:

```
1. recvfrom(conn) → (buf, srcIP)
2. allowlist.Allow(srcIP)?  ──no──► DROP   (03; derived from ConfigDoc.Nodes[].Addrs)
3. wire.Unmarshal(buf) → (hdr, payload); validate magic 'ESND', version, payloadLen
4. hdr.StreamGen == r.gen?   ──no──► adopt-or-ignore (§5.8)
5. p := Packet{hdr, payload}
6. recovered := fec.Recover(p)          // §6.3 — source pkts now available
7. for each recovered source packet:
      a. dedupe by seq (drop if already delivered)        // §5.6.2 window
      b. reorder: insert into a small seq-ordered window
      c. when in-order or window slides past it:
           pcm := codec.Decode(payload)
           ring.PushAt(sampleIndex, pcm)                  // hand off to 06
```

**Reorder/dedupe window.** A bounded window keyed by `seq` (a few × the FEC interleave
span, default ~32 packets ≈ 320 ms) absorbs network reordering and FEC/duplication
deduping. Packets are released to decode in `seq` order; the window also records delivered
`seq`s so duplicates (Dup mode, or accidental dups) are dropped. The window size must
exceed `D × k` so interleaved groups can complete before they age out.

### 5.6.3 Late / duplicate / missing packet policy

| Condition | Detection | Policy |
|---|---|---|
| **Duplicate** | `seq` already delivered / in window | **Drop** silently (expected in Dup mode). |
| **Reordered (early)** | `seq` > expected, within window | Hold in window; release when gap fills or window slides. |
| **Late** (arrives after its playout time / past the window) | `sampleIndex` already played, or `seq` aged out | **Drop** — too late to use; never push to ring behind the play cursor. |
| **Missing / unrecoverable** | gap in `seq` survives FEC, window slides past it | **Conceal:** insert one chunk of concealment for that `sampleIndex` — Opus PLC if available (§5.4.2), else short-fade **silence** for PCM. The ring is filled at the right index so the timeline stays aligned. |

The cardinal rule: **the ring is always filled at the correct `sampleIndex`** — a missing
chunk becomes concealment/silence at that index, never a shift of subsequent audio.
Alignment (sample-accuracy, D5) is preserved at the cost of a 10 ms glitch.

### 5.6.4 Late-joining listener startup

When a node joins a playing group (becomes a listener mid-stream), the group engine
([04](./04-clock-and-groups.md)) registers it via `AddListener`. The origin then:

1. Reads the **current** group play position from `Timeline.NowSample()` (§6.2).
2. Begins unicasting to the new listener **at the next chunk boundary** at/after the
   current `sampleIndex` (it does not rewind; it joins the live timeline).
3. **Forces a keyframe** for codecs with inter-frame state so the joiner can decode cold:
   - **PCM:** every chunk is already independent — the very next chunk works; the
     keyframe flag is set so the join path is uniform.
   - **Opus:** the encoder is reset and the next chunk emitted as a keyframe (§5.4.2);
     until that keyframe the joiner cannot decode, so the origin emits the keyframe
     immediately on join rather than waiting for the periodic one.
4. The joiner fills its ring from that first decodable chunk and begins playout once it
   has accumulated the buffer lead (§5.7); the group `Timeline` (not the joiner) defines
   *which* `sampleIndex` is "now," so the new node is sample-aligned from its first
   rendered frame.

A late joiner therefore experiences at most one keyframe-interval of startup delay (Opus)
or none (PCM), plus the buffer-lead fill.

---

## 5.7 Buffering and the lead budget

The listener holds a **jitter buffer** (`internal/audio/ring`, the mpvsync ring design)
ahead of playout. The **buffer lead** (default **~300 ms**) is how far the master streams
*ahead* of each chunk's playout instant (§5.6.1), and equivalently how full the receiver's
ring is at steady state. The lead must cover the sum of:

```
bufferLead  ≥  FEC recovery span          (§5.5.3: D×chunk ≈ 40 ms @ defaults)
            +  network jitter (P99 one-way variation on WiFi)
            +  decode + ring push slack
            +  the drift-loop correction headroom (06)
```

```
 master sends ──────────────────────────────────────────────► time
   chunk for sampleIndex idx  sent at  playout(idx) − 300 ms
                                                  │
   receiver ring fill:  [■■■■■■■■■■■■■■■■■■■■■■■]   ≈ 300 ms of audio queued
                         ▲ play cursor (06)        ▲ newest pushed chunk
   FEC span (~40 ms) ────┤
   jitter + slack ───────┴──────────┐
```

| Lead component | Default | Source |
|---|---|---|
| FEC recovery span | ~40 ms | §5.5.3 (k=8, D=4) |
| WiFi jitter headroom | ~100 ms | empirical P99 |
| Decode + push slack | ~20 ms | implementation |
| Total **buffer lead** | **~300 ms** | locked default; configurable per profile |

**Relationship to FEC.** A deeper interleave or larger k *requires* a larger lead because
a group cannot be repaired until enough of it has arrived (§5.5.3). The 300 ms default is
chosen to comfortably contain the default FEC span plus WiFi jitter; if a profile picks a
deeper interleave the lead must grow with it.

**Underrun handling.** If the ring runs dry at the play cursor (lead exhausted — severe
loss burst, master stall, or clock glitch):

- The renderer ([06](./06-audio-output-scheduling.md)) outputs **silence** (or holds the
  last sample with a short fade) for the missing samples at their `sampleIndex` — again
  never shifting the timeline.
- The receiver does **not** try to "catch up" by playing faster; it lets the buffer refill
  to the lead. The drift PI loop (06) handles small, sustained skew; underrun is the gross
  failure path and is logged/metric'd for the UI.
- Persistent underruns are a signal to the group engine to consider TCP fallback (§5.9) or
  a more robust codec/FEC profile (04 renegotiation).

---

## 5.8 `streamGen` semantics (media change / seek)

`streamGen` (§6.4, offset 8) is the **group stream generation**: a counter that the master
**bumps** whenever the audio content's relationship to the timeline changes
discontinuously — i.e. on an **operator media change or seek** (the Media screen, 09;
stored in `ConfigDoc.Groups[].media`, 07), **and on every master change / failover** (R11,
aligned with 01/04). It is **not** bumped on:

- a **loop** boundary (§5.2.1) — the timeline is continuous, just more PCM.

**Master failover always bumps `streamGen`** (R11). There is **no "continue the same
generation if reconstructable" path** — a new master, even one resuming the identical
media, starts a fresh generation. The new master reads the replicated `Playing` flag and
the current timeline `sampleIndex`, and **resumes the timeline at that `sampleIndex` with
`Playing` preserved** (see 04 failover); receivers **flush + re-prime** (~one buffer)
exactly as for any generation change below. This keeps failover unconditional and
split-brain-safe rather than depending on whether the new master can reconstruct the old
`seq`/FEC state.

**Master behavior on a generation change (media change, seek, or failover):**

1. Bump `streamGen` (`gen++`).
2. Reset the `seq` space (new generation starts at `seq = 0`) and the FEC state (parity
   groups / dedupe offsets restart cleanly).
3. Set `sampleIndex` to the timeline position the operator selected (seek target), the
   continuing position (media change keeping the timeline), or — on failover — the current
   timeline `sampleIndex` with `Playing` preserved.
4. Force a **keyframe** on the first chunk of the new generation (so all listeners,
   including Opus, resync cold — §5.6.4).

**Receiver behavior on a generation change (`hdr.StreamGen != r.gen`):**

```
if hdr.StreamGen > r.gen:
    flush reorder window + FEC state + (the stale tail of) the ring beyond the new idx
    r.gen = hdr.StreamGen
    resume decoding from this (keyframe) packet at its sampleIndex
elif hdr.StreamGen < r.gen:
    DROP   // stale packets from a previous generation (reordered late)
```

A newer generation **invalidates** any buffered audio from the old one (a seek means the
old queued samples are wrong), so the receiver flushes the ring ahead of the new
`sampleIndex` before pushing the new content. This keeps a seek crisp instead of playing
out 300 ms of stale audio first. `streamGen` monotonicity also disambiguates reordered
stragglers from the previous generation (dropped).

---

## 5.9 TCP fallback transport (D2)

UDP (D2) is the default audio plane. A **TCP fallback** carries the **same chunks** —
identical `wire` packets (§6.4), same `seq`/`sampleIndex`/`streamGen` — over a reliable
stream instead of datagrams, selected per group via `ConfigDoc.Groups[].transport`
([07](./07-config-and-replication.md)).

**When / why:**

- Networks that drop or rate-limit UDP, or where the allowlist/UDP path is blocked.
- Persistent underruns (§5.7) despite FEC, indicating loss beyond FEC's single-loss budget.
- Diagnostics / known-bad WiFi, where reliable-but-higher-latency beats glitching.

**How:**

- The origin opens a **TCP connection per listener** and writes length-prefixed `wire`
  packets back-to-back (the same marshaled bytes; TCP already frames the stream so a
  2-byte length prefix delimits packets). The header `payloadLen` is also present, but the
  stream prefix avoids relying on it for framing.
- **FEC is disabled** (`FECID = None`) over TCP — TCP's retransmission already guarantees
  delivery, so XOR/duplication would be pure waste. `Protect` is the identity in this mode.
- The receiver runs the same pipeline (§5.6.2) minus FEC and minus UDP reordering (TCP
  delivers in order); it still keys the ring by `sampleIndex` and still honors `streamGen`.
- **Tradeoff:** TCP head-of-line blocking and retransmit can *increase* latency under loss,
  so the buffer lead (§5.7) may need to grow; TCP trades glitch-freedom for tail latency.
  It is a fallback, not the default — the realtime budget favors UDP+FEC in the common case.

Pacing still applies: the origin writes chunks paced to `playout − bufferLead` so it does
not flood the TCP socket and inflate the queue.

---

## 5.10 Worked packet diagram (annotated)

A single source chunk on the wire (§6.4), PCM codec, XOR-parity FEC, mid-stream:

```
 ┌── ESND audio packet (header 44 B, big-endian) ──────────────────────────────┐
 │ off 0  'E''S''N''D'        magic                                            │
 │ off 4  01                  version = 1                                      │
 │ off 5  02                  flags: bit0 repair=0, bit1 keyframe=1 (PCM)       │
 │ off 6  00                  codecID = PCM (D3; S16LE)                         │
 │ off 7  01                  fecID   = XORParity (D4)                          │
 │ off 8  00000000 00002A3F   streamGen = 0x2A3F  (current generation, §5.8)    │
 │ off 16 00000000 0001E240   seq       = 123456  (monotonic source seq)        │
 │ off 24 00000000 03857A00   sampleIndex = 58982400 frames on group timeline   │
 │ off 32 00000C2F A1B2C3D4   masterMono = master monotonic ns at source        │
 │ off 40 0780                payloadLen = 1920 bytes (480 × 2ch × 2B S16LE)    │
 │ off 42 01E0                rate/100 = 480  → 48000 Hz                        │
 │ off 44 [ 1920 bytes: 480 stereo S16LE interleaved samples ]                  │
 └──────────────────────────────────────────────────────────────────────────────┘

 …followed (interleaved per §5.5.3) by the group's repair packet:
 ┌── repair packet for parity group A ─────────────────────────────────────────┐
 │ off 5  01                  flags: bit0 repair=1                              │
 │ off 16 …                   seq = repair seq (covers A's k source seqs)       │
 │ off 44 [ payload = S0⊕S1⊕…⊕S7 of group A, zero-padded to max len ]           │
 └──────────────────────────────────────────────────────────────────────────────┘
```

Send order for k=8, D=4 (round-robin groups A/B/C/D, repairs interleaved after each group
completes):

```
 S0(A) S1(B) S2(C) S3(D) S4(A) S5(B) S6(C) S7(D) … S28(A) S29(B) S30(C) S31(D) RA RB RC RD …
   ▲ each Sn paced to playout(sampleIndex_n) − 300 ms ; each fanned out to every listener (D5)
```

---

## 5.11 Per-codec bandwidth table (per node)

Steady-state **payload + header** bandwidth for one **stereo** group at the canonical rate
(48 kHz, `FramesPerChunk`=480 ⇒ 100 packets/s), **per listener / per node** (D5: this is
the cost of one unicast destination; multiply by listener count for total master egress).
Header overhead is the fixed **44 B/packet** (§6.4) ⇒ 44 B × 100 = **4400 B/s = 35.2
kbit/s** of header per stream, added to payload. FEC multiplies the whole figure.

Payload sizes: PCM is **S16LE, fixed** (m5) = 480 frames × 2 ch × 2 B = **1920 B/chunk**;
Opus is content-dependent (typical music estimates shown).

| Codec | Payload/chunk | Payload bitrate | + header (35.2 kbit/s) | **No FEC** total | **XOR k=8 (+12.5%)** | **Dup (+100%)** |
|---|---|---|---|---|---|---|
| **PCM** (S16LE) | 1920 B | 1536 kbit/s | 1571 kbit/s | **~1.57 Mbit/s** | ~1.77 Mbit/s | ~3.1 Mbit/s |
| **Opus @128k** | ~160 B | 128 kbit/s | ~163 kbit/s | **~163 kbit/s** | ~183 kbit/s | ~326 kbit/s |
| **Opus @64k** | ~80 B | 64 kbit/s | ~99 kbit/s | **~99 kbit/s** | ~111 kbit/s | ~198 kbit/s |

> **Reading the table.** PCM (S16LE) is the mandatory baseline — ~1.57 Mbit/s, comfortable
> on WiFi for a handful of listeners. Opus is an order of magnitude cheaper and is the
> optional, capability-gated choice for many listeners or constrained links, at the cost
> of lossy compression and decoder CPU. (FLAC is **not** a wire codec — it is a
> source-decode format only, §5.3.) Header overhead (35 kbit/s) is negligible for PCM but a
> *third* of an Opus@64k stream — another reason 10 ms chunks (not 5 ms) are the default.
> **Total master egress = per-listener row × number of listeners** (D5 unicast), so codec
> choice scales the master's uplink linearly with group size.

---

## 5.12 Cross-references

| Topic | Document |
|---|---|
| Allowlist source-IP gate (recv step 2) | [03-adoption-takeover-security-pki.md](./03-adoption-takeover-security-pki.md) |
| Allowlist derivation from `Nodes[].Addrs`; `Groups[].media`/`transport` | [07-config-and-replication.md](./07-config-and-replication.md) |
| `Timeline.NowSample`, `ClockSource.Offset`, profile negotiation, failover | [04-clock-and-groups.md](./04-clock-and-groups.md) |
| Ring playout, drift PI loop, `AudioSink`, channel/gain/`HWDelayUs`, concealment render | [06-audio-output-scheduling.md](./06-audio-output-scheduling.md) |
| Media browse/select/play UI | [09-ui-screens.md](./09-ui-screens.md) |
| Dumb-node profile, RS exclusion rationale | [10-roadmap-and-dumb-nodes.md](./10-roadmap-and-dumb-nodes.md) |
| Wire header §6.4, Codec/FEC ifaces §6.3, Timeline §6.2, ConfigDoc §6.5 | [README.md](./README.md) |
