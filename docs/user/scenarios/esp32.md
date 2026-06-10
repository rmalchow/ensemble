# Setup: ESP32 + class-D amp speaker node

> **You are here:** [User Guide](../README.md) › Scenarios › **ESP32 speaker node**
> Other setups: [NAS / server master](nas-master.md) · [Raspberry Pi player](raspberry-pi.md) · [Desktop / laptop](desktop.md)

> ## 🚧 Under active development
> This is a **preview of work in progress**, not a setup you can deploy today. The
> hardware and firmware are being built now; the wire protocol they speak is
> already specified and there's a working reference client, but the board and its
> flasher aren't released yet. The steps below describe **how it will work** so you
> know what's coming. For the current state, watch the repository.

## What this will be, and why

A tiny custom board — an **ESP32 + I²S DAC + a small class-D amplifier** — that
**is** the speaker. No operating system, no SD card, no computer: just power in and
speaker terminals out. It joins the cluster as a **thin playback node** — visible
and assignable in the UI exactly like a Pi, but with a fraction of the cost, size,
and power draw.

**Why a dedicated board?**

- **The smallest, cheapest synchronized speaker** you can put in a room — where a
  whole Raspberry Pi would be overkill, and where you'd rather not have an OS to
  maintain.
- **Lowest power and instant-on.** It boots in well under a second and sips power,
  so it's happy to live permanently behind a bookshelf or in a ceiling enclosure.
- **One purpose, done well.** It only ever *receives and plays* — it can't host a
  library or master a group — which keeps the firmware tiny and reliable.

In the running examples, this is the future answer to "I want a speaker *there* too"
without adding another Pi: an extra zone in the **family** home or a per-desk
speaker in the **studio**, for the cost of a board.

---

## How it joins a cluster (the design)

The board is a **"thin" node**: it speaks the same audio wire protocol as any
player, but it doesn't run the heavy gossip layer that full nodes use. Instead it
runs **mDNS + a tiny HTTP API**, and the full nodes represent it in the cluster so
it still **shows up in the UI, can be dropped into any group, and has its own volume
and hw-delay**. From your side it behaves like any other player.

What a conforming node does (from the wire spec):

1. **Discovers** the group master (a configured master, or by asking any node's API).
2. **Subscribes** to the master's audio source and keeps the subscription alive.
3. **Clock-syncs** to the master so it plays each frame at the same real-world
   instant as every other speaker.
4. **Plays out** through its I²S DAC, buffering to ride out Wi-Fi jitter.
5. **Decodes Opus** — effectively mandatory on Wi-Fi hardware (small packets that
   don't fragment), which is why **opus** is ensemble's default group codec.

> Because it's Wi-Fi + Opus, keep the group on the **opus** codec (the default) and
> give it a comfortable buffer — the same Wi-Fi advice as for a
> [Raspberry Pi](raspberry-pi.md#picking-the-right-output--tuning-sync).

> **Spotify on the board?** The board never runs go-librespot — like any thin
> playback node it can't advertise a Connect device. It still **plays Spotify**
> whenever it's in a group whose master is sourcing one (start the session on a
> [NAS master](nas-master.md) or [desktop](desktop.md)). To set up the advertised
> Connect device on that master, see **[Spotify Connect](../spotify.md)**.

---

## Planned provisioning

The board targets the **ESP32-S2-WROOM** (native USB), specifically so it can be
flashed and put on your Wi-Fi from a **browser-based flasher/provisioner** — no
toolchain, no serial fiddling. The companion **class-D amplifier board** is an
ESP32-S3 + PCM5102A DAC + TPA3118D2 amp reference design. Once flashed and given
Wi-Fi credentials, it announces itself over mDNS and appears in the UI like any
other node, ready to drop into a room.

---

## Want the detail?

The full firmware, hardware, membership, and provisioning design lives in the
developer docs:

- **[`docs/esp32.md`](../../esp32.md)** — the ESP32-S2 node design spec (firmware,
  hardware, the thin-node membership model, the web flasher).
- **[`docs/DUMB-CLIENT.md`](../../DUMB-CLIENT.md)** — the audio wire protocol any
  thin client implements, with a reference client in
  [`cmd/dumbclient`](../../../cmd/dumbclient).

---

**See also:** [Configuration Reference](../config-reference.md) ·
[UI Reference](../ui-reference.md) ·
[Raspberry Pi player](raspberry-pi.md) (the available-today equivalent)
