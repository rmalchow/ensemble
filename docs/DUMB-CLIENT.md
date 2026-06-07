# Dumb Client — protocol-minimal audio receiver

This is a **self-contained** implementer's spec for a *protocol-minimal* ensemble
audio client: a **receive-only** participant (e.g. ESP32-S3 firmware) that plays
the group's audio in sync, but is **not** a cluster member. You do not need to
read any other document to implement this. The reference implementation is
[`cmd/dumbclient/main.go`](../cmd/dumbclient/main.go) (pure stdlib, no internal
imports — it proves this spec is sufficient).

---

## 1. Scope and what you are NOT

A dumb client:

- **subscribes** to a group master's audio source and **plays** it in sync;
- **follows** the master clock so playout is sample-aligned with the real members;
- is **invisible to the cluster**: it does not gossip, does not appear in
  `GET /api/cluster` `groups[].members`, and **takes no part in codec
  negotiation**. The group picks its codec based only on its real members.

Because you are invisible to negotiation, **you cannot rely on the group running
a codec you support.** The default group codec is **opus**. A protocol-minimal
client typically wants **PCM** (no decoder) — so either:

- pin the group codec to `pcm` out of band (an operator sets
  `POST /api/group/settings {"codec":"pcm"}`), **or**
- implement an **opus** decoder (libopus) — recommended for real Wi-Fi firmware.

**Transport choice.** Raw PCM is `24 + 3840 = 3864` bytes per frame, which
**IP-fragments into ~3 UDP packets**; losing any fragment loses the whole frame,
and the per-frame XOR FEC (one parity per 4 frames) often cannot recover it on
lossy Wi-Fi. Therefore:

- on **wired/reliable** links: PCM over UDP is fine;
- on **Wi-Fi**: use **opus** (a 20 ms packet ≈ 320 B, one datagram, never
  fragments) **or** `--transport tcp` (TCP retransmits; no FEC needed).

This document and the reference client cover the **protocol**. Opus is the codec
concern of real firmware (decode the audio payload with libopus before playout —
everything else in this spec is identical).

---

## 2. Protocol version (v1)

**The magic byte `0xE5` IS the version marker.** Every framed packet begins with
it. Two rules:

1. **Unknown packet `type` values MUST be ignored by receivers** (forward
   compatibility — new optional types may be added within v1).
2. A **future incompatible** revision changes the **magic byte**, not the
   layout. A receiver that sees a leading byte other than `0xE5` MUST drop the
   datagram. So checking `buf[0] == 0xE5` is both your framing sanity check and
   your version gate.

---

## 3. The common 24-byte header

Every packet — audio, FEC, clock, and control — starts with this fixed header.
**All multi-byte fields are big-endian.**

| Offset | Size | Field        | Type   | Meaning                                          |
|-------:|-----:|--------------|--------|--------------------------------------------------|
| 0      | 1    | `magic`      | u8     | always `0xE5`                                    |
| 1      | 1    | `type`       | u8     | packet type (table below)                        |
| 2      | 4    | `gen`        | u32 BE | session generation; drop frames with `gen < yours` |
| 6      | 8    | `seq`        | u64 BE | frame sequence number, 0-based per session       |
| 14     | 8    | `pts`        | i64 BE | presentation timestamp, **master-clock ns**      |
| 22     | 2    | `payloadLen` | u16 BE | payload byte count following the header          |
| **24** |      |              |        | **total header size**                            |

The payload follows immediately. Over UDP, one datagram = one header + payload.
Over TCP, each frame is **length-prefixed**: a `u32 BE` byte-count, then exactly
that many bytes (which themselves begin with the 24-byte header). See §6.

### Packet types

| Type   | Name        | Direction   | Dumb client | Notes                                       |
|--------|-------------|-------------|-------------|---------------------------------------------|
| `0x01` | AUDIO       | src → sub   | **required**| header + PCM (3840 B) or opus payload        |
| `0x02` | FEC         | src → sub   | optional    | XOR parity over 4 audio frames (UDP only)    |
| `0x10` | CLOCK_REQ   | sub → master| **required**| you send these (clock probe)                 |
| `0x11` | CLOCK_RSP   | master → sub| **required**| you receive these (clock reply)              |
| `0x20` | HELLO       | sub → src   | **required**| subscribe / keepalive; payload flag prime-me |
| `0x21` | BYE         | sub → src   | optional    | "leaving, stop sending"                      |
| `0x22` | RESTART     | sub → src   | recommended | "I got lost, re-prime me"                    |
| `0x23` | RECONFIG    | src → sub   | **required**| "session/settings changed"; flag = stop      |

**Mandatory:** AUDIO, CLOCK_REQ/RSP, HELLO, RECONFIG.
**Recommended:** RESTART (robustness after loss).
**Optional:** FEC (ignore it — gaps just play silence), BYE (politeness).

**Control payload (types `0x20`/`0x22`/`0x23`):** a single byte flag.
- HELLO / RESTART: bit0 = **prime-me** (`0x01`) → request a burst of recent
  frames so you can start immediately.
- RECONFIG: bit0 = **stop** (`0x01`) → the session ended.

---

## 4. Ports and addressing

A node advertises (in `GET /api/cluster`, §7) two ports you care about:

- **`sourcePort`** — the master's audio source. You send HELLO/BYE/RESTART here
  and (TCP) dial here. (Default `9200`.)
- **`streamPort`** — the master's clock server. You send CLOCK_REQ here and
  receive CLOCK_RSP. (Default `9090`.)

**Key UDP detail (observed-by-construction):** over UDP, the master streams
audio back to **the exact source address your HELLO came from**. So you must
**send your HELLO and your clock probes from the SAME UDP socket you read audio
on** — one socket, bound to an ephemeral port. The reference client uses a
single `udp4` socket for clock-out, clock-in, and audio-in.

Over TCP everything (control + audio) rides the one connection to `sourcePort`;
the clock still uses UDP to `streamPort`.

---

## 5. Clock sync (required before playout)

Master-anchored, NTP-style, over the master's **streamPort (UDP)**. Run this
continuously from boot.

**Probe (1 Hz).** Send a CLOCK_REQ to the master's streamPort:
- header: `type=0x10`, `gen` = your current session gen (echoed back; the master
  does not filter on it), `seq` = a per-probe counter, `pts=0`, `payloadLen=24`;
- payload: 24 bytes = three `i64 BE` `t1|t2|t3`. On a request these are unused by
  the master (it overwrites t2/t3). Record your local send time **t1** (your
  monotonic clock) keyed by `seq`.

**Reply.** The master answers with a CLOCK_RSP (`type=0x11`), echoing your `seq`,
with payload `t1|t2|t3` where **t2** = master receive time, **t3** = master send
time. **Stamp t4 = your local receive time the instant the datagram arrives**
(before any parsing). Match the reply to your pending probe by `seq`; drop
unknown/duplicate/late replies and replies whose `gen` ≠ your current gen.

**Per-sample math** (all nanoseconds):

```
offset = ((t2 - t1) + (t3 - t4)) / 2      // master_ns - local_ns
rtt    = (t4 - t1) - (t3 - t2)            // >= 0; smaller is better
```

**Estimate.** Keep the **last 30** samples. The offset you use is the **median
of the 5 smallest-RTT samples** in that window (best-RTT filtering rejects
scheduling jitter). Until you have ≥ 1 sample you are **unsynced** and **MUST
NOT** start playout.

**Conversions:**
```
masterToLocal(t_master) = t_master - offset
localToMaster(t_local)  = t_local  + offset
```

**Monotonic-clock requirement.** t1 and t4 — and every local time you compare a
deadline against — **MUST come from one monotonic clock** (never wall-clock /
NTP-stepped time). Mixing clocks injects the inter-process start delta into
playout and makes you lag by `|offset|`. On the ESP32 use `esp_timer_get_time()`
(µs since boot) consistently.

---

## 6. Subscribe + playout flow

### Subscribe (UDP)

1. Pick the master endpoints (§7).
2. Send a **HELLO with prime-me** (`type=0x20`, payload `[0x01]`) from your audio
   socket to the master's `sourcePort`.
3. Because the initial HELLO can be lost, **retry up to 3 times at 500 ms** while
   no audio frame has yet arrived, re-requesting prime each time.
4. Once flowing, send a **keepalive HELLO every 5 s** (payload `[0x00]`, no
   prime). The master **expires** any subscriber unseen for **15 s** — so the
   5 s keepalive is mandatory.

### Subscribe (TCP)

Dial the master's `sourcePort` (TCP). Send a HELLO-with-prime as the first
length-prefixed control frame, then read length-prefixed frames (each is a full
header+payload). Keepalive HELLO every 5 s on the same connection. No FEC.

### Receiving audio

For each AUDIO frame (`type=0x01`):
- if `gen < your session gen` → **drop** (stale generation);
- if `gen > your session gen` → a **new/replaced session**: re-arm to that gen
  (reset your jitter buffer, set origin on the first new frame);
- buffer the payload keyed by `seq`. Record the frame's `pts` (and the
  `(seq, pts)` of the first frame as the session **origin**).

You may ignore FEC entirely; a missing `seq` just becomes a silence frame.

### Playout scheduling

Maintain a small **jitter buffer** (a `seq → {pts, payload}` map) and a scheduler
that emits **one 20 ms frame per output write, in `seq` order**.

For the next `seq` to play, compute its master-clock deadline from the session
origin (so gaps still schedule at the right instant):

```
slotPts  = originPts + (seq - originSeq) * 20_000_000      // ns, 20 ms steps
deadline_master = slotPts + bufferMs * 1_000_000           // add the playout lead
deadline_local  = masterToLocal(deadline_master)           // needs clock sync
```

- `bufferMs` is a **group setting** (default **150**). It is the playout lead:
  every member delays each frame by `bufferMs` past its pts so late/jittered
  frames still arrive in time. All members (and you) use the same value, so you
  all emit the same sample at the same wall instant → in sync.
- Sleep until `deadline_local` (on your monotonic clock). Then:
  - if the frame is present, write its payload to the output;
  - if it is **missing** (a gap), write a **frame of silence** (keeps the device
    cadence) — do **not** stall;
  - if the slot is **already a full frame late** (`now > deadline_local + 20 ms`),
    **skip it instantly without writing** (writing would push every later frame
    late forever); count it and move on.
- Advance to the next `seq`.

**Jitter-buffer sizing.** Hold roughly `bufferMs` worth of frames
(`bufferMs / 20` ≈ 7–10 frames at the defaults), plus a little slack. A few tens
of slots is plenty; bound it so a burst can't grow it without limit. At a fresh
session start the prime burst may briefly fill it to ~`2 × bufferMs`; that drains
to steady state within a second.

### Getting lost / starvation (RESTART)

If **no audio frame arrives for > 2 s**, send a **RESTART with prime-me**
(`type=0x22`, payload `[0x01]`) to the master's source — "I got lost, re-prime
and resume." If audio still does not return (the master is gone), give up and
re-discover (§7). The reference client sends one RESTART per starvation episode
and resumes when frames return.

### Generation handling on RECONFIG

When a RECONFIG (`type=0x23`) arrives:
- **stop flag set** (payload bit0 = `0x01`): the session ended. Drop buffered
  audio, go idle, and await the next session (a new HELLO cycle / discovery).
- **stop flag clear**: settings or generation changed. **Re-arm to the new
  `gen`** (reset jitter buffer, re-establish origin on the first new frame) and
  **re-subscribe**: send a fresh HELLO-with-prime so the master re-primes you
  under the new generation. Re-read group settings (codec/transport/bufferMs)
  from `GET /api/cluster` if you are in discovery mode.

The `gen` is a **per-master** counter. After a master change it may even be
*lower* than your last one — so on a master change, reset your floor to 0 and let
the first frame / RECONFIG establish the new master's gen.

### Opus specifics (real firmware)

If you decode opus instead of consuming raw PCM:
- the stream is **48 kHz, stereo, 20 ms** opus packets (960 samples/ch/frame);
  the audio payload is one opus packet (not 3840 B).
- decode each packet to s16le before the jitter/playout stage; everything else
  (timing, gen, buffer) is identical.
- **Reset the opus decoder on every generation change** (RECONFIG non-stop, or a
  jump in `gen`): a new session is a new encoder, so carrying decoder state
  across the boundary corrupts the first packets. On a **lost** packet, prefer
  opus packet-loss concealment (decode with a null packet) over hard silence;
  the reference PCM client simply plays silence on a gap. (The full ensemble
  member decodes per frame and re-creates its decoder at each session/gen change
  — mirror that.)

---

## 7. Discovery — `GET /api/cluster`

Any node serves `GET http://<host>:<httpPort>/api/cluster` returning the resolved
cluster as JSON. You only need a few fields. Poll it every **5 s** and
**re-subscribe whenever the master's endpoint changes**.

Fields a dumb client needs:

```jsonc
{
  "nodes": [
    {
      "id": "4ed795d4...",            // 32-hex node id
      "name": "n1",
      "addrs": ["10.0.0.5/24"],       // self-reported CIDRs (fallback dial host)
      "streamPort": 9090,             // clock server  (CLOCK_REQ here)
      "sourcePort": 9200,             // audio source  (HELLO/BYE/RESTART here)
      "observed": { "<peerId>": { "ip": "10.0.0.5" } }  // observed IPs (prefer these)
    }
  ],
  "groups": [
    {
      "id": "4ed795d4...",            // = the master's node id
      "name": "the-lab",
      "master": "4ed795d4...",        // node id of the master to subscribe to
      "members": ["<id>", "<id>"],    // real cluster members (you are NOT here)
      "settings": { "codec": "pcm", "transport": "udp", "bufferMs": 150 }
    }
  ]
}
```

**Resolution algorithm:**
1. Pick a group: by your `--group <id|name>`, else the first group with members.
2. Find the node whose `id == group.master`.
3. **Dial host:** prefer any `observed[*].ip`; else the host part of the first
   `addrs` CIDR. Use the same host for both ports.
4. Subscribe to `host:sourcePort`; clock-follow `host:streamPort`.
5. Read `settings.codec` (refuse/ warn if it is a codec you can't decode),
   `settings.transport` (udp/tcp), `settings.bufferMs` (your playout lead).

**Re-resolution:** on each 5 s poll, if the master id or its host/ports changed,
tear down and re-subscribe to the new endpoints (reset clock window on an
endpoint change). The reference client does exactly this.

### Simpler alternative — follow one node IP

If you don't want to poll HTTP at all, hard-code the master's IP and ports and
pass `--source <ip:sourcePort> --clock <ip:streamPort>`. You lose automatic
master-change handling, but the subscribe/clock/playout protocol is identical.
Good for a fixed installation pointed at a known always-master node.

---

## 8. Conformance checklist

A conforming dumb client:

- [ ] Treats `0xE5` as magic+version; **ignores unknown packet types**; drops
      datagrams whose first byte ≠ `0xE5`.
- [ ] Parses the **24-byte big-endian** header exactly (offsets in §3).
- [ ] Sends/reads from **one UDP socket** so audio returns to the HELLO source
      addr; uses the master's **sourcePort** for control, **streamPort** for clock.
- [ ] Runs a **1 Hz clock follower**, computes `offset = ((t2−t1)+(t3−t4))/2`,
      keeps the **median of the 5 best-RTT of the last 30**, and **withholds
      playout until synced**.
- [ ] Uses a **single monotonic clock** for t1/t4 and all deadlines.
- [ ] Subscribes with **HELLO+prime**, **retries 3× at 500 ms** until the first
      frame, then **keepalives every 5 s** (server expiry 15 s).
- [ ] Schedules playout at `deadline = masterToLocal(pts + bufferMs)`, plays
      **silence on gaps**, **skips frames already a full frame late**.
- [ ] Handles **RECONFIG**: stop → go idle; non-stop → re-arm to new `gen` and
      re-HELLO. Drops `gen < current`; re-arms up on `gen > current`.
- [ ] (Recommended) Sends **RESTART+prime** after **> 2 s** starvation.
- [ ] (Discovery mode) Polls `GET /api/cluster` every **5 s** and re-subscribes
      on master-endpoint change.
- [ ] Never gossips, never joins `groups[].members`, never affects negotiation.
