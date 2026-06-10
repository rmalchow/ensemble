# Setup: Raspberry Pi player

> **You are here:** [User Guide](../README.md) › Scenarios › **Raspberry Pi player**
> Other setups: [NAS / server master](nas-master.md) · [Desktop / laptop](desktop.md) · [ESP32 node (in dev)](esp32.md)

## What this is, and why

A **Raspberry Pi wired to active speakers** (or a small amp), tucked behind the
kitchen counter, on a bathroom shelf, or in a kid's room. The Pi runs ensemble as a
**playback node**: it joins the cluster, shows up in the UI, and plays whatever
group it's in — no screen, no keyboard, no interaction at the device itself.

**Why a Pi?**

- **A permanent speaker in every room** for the price of a Pi — it's always on and
  always ready, unlike a laptop you have to open or a phone you have to pair.
- **Cheap and low-power** enough to scatter several around the house. Even a **Pi
  Zero 2 W** is plenty for a playback node.
- **Set-and-forget.** Once provisioned it auto-rejoins its last room after a power
  cut, so the bathroom speaker is just *there* every morning.

This is the workhorse of the **family** (a Pi in each kid's room and the kitchen)
and the **flatshare** (a Pi per bedroom, grouped with the kitchen for dinner). It
pairs naturally with a [NAS master](nas-master.md) holding the library — though it
can follow *any* master, including a [desktop](desktop.md).

---

## Hardware

- A Raspberry Pi (Zero 2 W, 3, 4, or 5) with Raspberry Pi OS (or any Linux).
- **Audio out**, one of:
  - **Active speakers** on the 3.5 mm jack (Pi 3/4 — note the Pi Zero and Pi 5 have
    no analog jack), or
  - A **USB DAC** / USB sound card (best quality, works on any Pi), or
  - An **I²S DAC HAT** (e.g. a HiFiBerry-style board).
- Power and Wi-Fi (or Ethernet — wired is steadier for sync, but Wi-Fi is fine with
  the default opus/FEC settings).

---

## Install

Ensemble is a **single static binary** — no runtime to install, pure Go, same build
for every Pi from the Zero 2 up.

1. **Get the binary** for the Pi's architecture (`linux/arm64` for a 64-bit Pi OS;
   `linux/arm` for the older 32-bit / Pi Zero). Grab it from a
   [release](../../../RELEASING.md), or
   [build it](../../../README.md#build) and copy it over:

   ```sh
   scp bin/ensemble-linux-arm64  pi@kitchen.local:~/ensemble
   ```

2. **Run it as a player:**

   ```sh
   ssh pi@kitchen.local
   ./ensemble --role playback --name kitchen
   ```

   `--role playback` keeps it a pure speaker (no library, never a master) — see
   [roles](../config-reference.md#3-roles). The name is what you'll see in the UI; set
   it once here, rename later from the Nodes page if you like.

3. **Open any node's UI** (`http://<nas-or-pi-ip>:8080`), find the `kitchen` node,
   drop it into a room, and **Play here**.

> No library folder, no ports, no flags beyond the role and name. It discovers the
> rest of the cluster over mDNS on its own.

### Run it on boot

So the Pi is a speaker the moment it powers up, install a tiny systemd service:

```ini
# /etc/systemd/system/ensemble.service
[Unit]
Description=ensemble player
After=network-online.target sound.target
Wants=network-online.target

[Service]
ExecStart=/home/pi/ensemble --role playback --name kitchen
Restart=always
User=pi
WorkingDirectory=/home/pi

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now ensemble
journalctl -u ensemble -f      # watch the startup banner / logs
```

Because the node persists its `following` field, it **auto-rejoins its last room**
after a reboot.

---

## Picking the right output & tuning sync

- **Choose the output device** from the node's **Settings → output device** picker
  in the UI when the Pi has more than one (e.g. a USB DAC alongside HDMI). Use the
  **♪ test tone** button to confirm sound is coming out of the speaker you think it
  is. (See the [UI Reference](../ui-reference.md#the-nodes-page--gear).)
- **Crackling or dropouts on Wi-Fi?** Two dials, in order:
  1. Raise the Pi's ALSA buffer: start it with `ENSEMBLE_ALSA_LATENCY_MS=400`.
  2. Raise the **group's buffer** in the room's Advanced settings, and keep the
     codec on **opus** (small packets + FEC ride out Wi-Fi loss).

  Both are explained in the
  [Config Reference](../config-reference.md#6-audio-output-backend).
- **One room a hair behind another?** Nudge its **hw delay** on the Nodes page to
  compensate the speaker/amp's fixed latency until the rooms line up —
  [how hw delay works](../config-reference.md#hw-delay--alignment).

---

## Living the examples

- **Family:** a Pi in each kid's room plays a local audiobook from the
  [NAS](nas-master.md) while the kitchen Pi plays jazz — different sources, same app,
  at the same time. Movie night? Drag both kids' Pis into the living-room group.
- **Flatshare:** each bedroom Pi is its own group on a weekday morning (three alarms,
  three podcasts). At dinner, tap the kitchen and living-room Pis into one group and
  the same playlist follows everyone to the table. Grouping/ungrouping is all in the
  [UI](../ui-reference.md#room-controls-a-selected-room).

> **Spotify on a Pi?** A playback Pi doesn't need go-librespot — it just plays
> whatever its group's master is sourcing, including a Spotify session started on
> the NAS or a desktop. If you *want* the Pi itself to advertise a Spotify device,
> install the separate `go-librespot` binary next to its `ensemble` binary — full
> steps and release links in **[Spotify Connect](../spotify.md#native-install-go-librespot)**.
> (The native binary does **not** bundle it; only the Docker master image does.)

---

## Verify it works

1. The service is running (`systemctl status ensemble`) and the banner shows the
   bound ports and the chosen audio backend (`alsa`).
2. The Pi appears as a node in the UI within seconds of boot.
3. **♪ test tone** plays out of the intended speaker.
4. Added to a playing room, it plays in sync with the other speakers.
5. After a reboot it rejoins its last room on its own.

---

**See also:** [Configuration Reference](../config-reference.md) ·
[UI Reference](../ui-reference.md) · [NAS / server master](nas-master.md)
