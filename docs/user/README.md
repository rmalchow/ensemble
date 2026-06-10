# Ensemble — User Guide

**Ensemble turns the speakers scattered around your home, office, or studio into
synchronized, groupable "rooms" you steer from any phone or browser.** Put music
in one place, run one small program on each device with a speaker, and they find
each other and play in perfect sync — no cloud, no accounts, no config files.

<p align="center">
  <img src="images/overview.png" width="320" alt="The ensemble web app on a phone: a playing room group with cover art, now-playing track, group volume and per-speaker volumes" />
</p>

This guide gets you from zero to music:

1. **[The mental model](#the-mental-model)** — the handful of ideas everything is built on.
2. **[Three homes, one app](#three-homes-one-app)** — real setups that show what ensemble is *for*.
3. **[Pick your setup](#pick-your-setup)** — install instructions for each kind of device.
4. **[Running ensemble](running.md)** — every way to start a node and keep it running (foreground, `nohup`, systemd, Docker, Compose).
5. **[What can it play?](#what-can-it-play)** — local files, Spotify/podcasts, radio, line-in.
6. **[Spotify Connect](spotify.md)** — play Spotify & podcasts to any group (bundled in Docker; one binary to add natively).
7. **Reference** — the [UI Reference](ui-reference.md) (every screen and control) and the [Configuration Reference](config-reference.md) (every knob, explained).

---

## The mental model

Five ideas and you understand the whole system:

- **A node** is one device running the ensemble program — a Raspberry Pi, a NAS, a
  desktop, a laptop. Every node runs the *identical* binary.
- **Every node serves the same web app** at `http://<that-node>:8080` and proxies
  to all the others. Open *any* node's address and you can control the whole house.
- **A node has roles.** A **master** owns the music library and streams audio; a
  **player** receives a stream and pushes it out a speaker. Most nodes are both.
  A headless NAS is a master with no speakers; a tiny Pi in the bathroom is a
  player. (See [roles](config-reference.md#3-roles).)
- **A room (group)** is one master plus the players following it. By default every
  node is a group of one. Tell a player to follow a master and they merge into a
  room that plays in lock-step. Group and ungroup freely from the UI.
- **Sources are what's playing**: a file from the master's library, a Spotify
  Connect session, an internet-radio URL, or a line-in capture.

That's it. Discovery, sync, and failover all happen automatically — you spend your
time grouping rooms and picking music, not configuring software.

---

## Three homes, one app

Ensemble looks different in every home. Three sketches, which we'll return to as
running examples throughout the guide:

### 🎧 The shared studio
*You and two colleagues share a workspace and (mercifully) the same taste in
music.* You want **one soundtrack across the whole floor** that **any** of you can
skip or turn down from your own laptop — without a Bluetooth speaker that only
pairs with one phone at a time. Each desk computer is a node; you group them into
one room and whoever's nearest the keyboard is the DJ.
→ mostly [desktops & laptops](scenarios/desktop.md), often with a
[NAS](scenarios/nas-master.md) holding the shared library.

### 👨‍👩‍👧‍👦 The family with two kids
*Bedtime audiobooks in the kids' rooms; jazz in the kitchen while you cook.* The
whole point is **different content in different zones at the same time** — and the
flexibility to merge them ("everyone in the living room for the movie"). A cheap
Pi behind each set of speakers, a NAS in the cupboard with the family library,
and Spotify for the grown-ups.
→ [Raspberry Pi players](scenarios/raspberry-pi.md) per room, a
[NAS master](scenarios/nas-master.md), and [Spotify](scenarios/nas-master.md#spotify-connect--podcasts).

### 🏠 The three-flatmate flatshare
*Everyone wakes up at a different time — and on Sunday you cook dinner together.*
On weekday mornings each room is its **own** group: three alarms, three podcasts,
no collisions. Come evening you tap two rooms into **one** group and the same
playlist follows you from kitchen to table.
→ a [player](scenarios/raspberry-pi.md) (or [desktop](scenarios/desktop.md)) per
room, grouped and ungrouped on demand from the [UI](ui-reference.md).

The hardware overlaps more than it differs. The rest of the guide is about
choosing the right *kind* of node for each spot and wiring it up.

---

## Pick your setup

Most homes mix two or three of these. Start with whichever you have hardware for
today — you can always add nodes later, and they'll discover each other on their
own.

### 🗄️ Headless NAS / server — the always-on brain
A box with **no speakers**: your NAS, a mini-PC, an old server. It holds the music
library and does the streaming, but never makes a sound itself. **Why:** keep all
your music in one place and keep it playing even when every laptop in the house is
asleep — the library and the "brain" live on the one device that's always on. This
is also the home for **Spotify Connect**. Runs beautifully in **Docker**.
→ **[Set up a NAS / server master](scenarios/nas-master.md)**

### 🍓 Raspberry Pi — a permanent speaker in every room
A cheap Pi (even a Pi Zero 2) wired to **active speakers** or an amp, tucked behind
the kitchen counter or on the bathroom shelf. **Why:** a dedicated, always-ready
speaker in every room for the price of a Pi — no computer to boot, no phone to
pair. This is the workhorse of the family and flatshare setups.
→ **[Set up a Raspberry Pi player](scenarios/raspberry-pi.md)**

### 💻 Desktop / laptop — the speakers you already own
The computer already on your desk, using **its existing sound card and speakers**.
**Why:** zero new hardware. It's both a player *and* a perfectly good master (it
can host the library too), which makes it the natural starting point — especially
in the shared studio, where every desk is already a node.
→ **[Set up a desktop or laptop](scenarios/desktop.md)**

### 🔌 ESP32 + class-D amp — a speaker node with no computer at all
A tiny custom board (ESP32 + DAC + a small class-D amplifier) that *is* the
speaker — no operating system, no SD card, just power and a pair of speaker
terminals. **Why:** the smallest, cheapest, lowest-power way to put a synchronized
speaker somewhere a Pi would be overkill. **Status: under active development** —
the hardware and firmware are being built now; this page is a preview of the
design and how it will join a cluster.
→ **[About the ESP32 speaker node (in development)](scenarios/esp32.md)**

> **Just want to try it on one machine first?** Download a build, run `./ensemble`,
> open `http://localhost:8080`, and drop a few audio files in `./data/media`. Then
> run it on a second machine on the same network and watch them find each other.
> The [desktop guide](scenarios/desktop.md) covers this in full.

---

## What can it play?

A room's **master** opens one **source** and streams it to the whole group. Four
kinds:

- **🎵 Local files** — `wav`, `mp3`, `flac` in the master's library folder, browsed
  as folders right in the UI. This is the family's audiobook collection on the NAS.
  Set up per scenario; details in the [NAS guide](scenarios/nas-master.md#the-music-library).
- **🟢 Spotify & podcasts (Spotify Connect)** — a node running **go-librespot**
  shows up as a device in your Spotify app; pick it and the whole group plays your
  Spotify music *or podcasts*. Premium required. **Bundled in the Docker master
  image**; on native nodes you add the binary yourself — full setup, release links,
  and how ensemble finds it in **[Spotify Connect](spotify.md)**.
- **📻 Internet radio** — paste an `http(s)://…` stream URL in the media browser.
- **🎚️ Line-in** — capture a sound card's input (a turntable, a TV) on nodes where
  the **input** feature is enabled, and stream *that* to the group.

You mix and match freely: the kids' Pi can play a local audiobook while the
kitchen plays Spotify, all from the same app.

---

## Reference

- **[Running ensemble](running.md)** — every way to launch and supervise a node:
  foreground, `nohup`, systemd (system **and** user services), Docker, and Compose,
  with the exact commands and unit/yml files.
- **[UI Reference](ui-reference.md)** — a screen-by-screen tour of the web app:
  the Rooms page, room controls, the Nodes page, and the Spotify endpoints editor,
  with screenshots of every control.
- **[Configuration Reference](config-reference.md)** — every flag, environment
  variable, and persisted setting, with verbose explanations: ports and
  bind-or-increment, roles, output backends, the data/media directories, per-node
  volume/delay/device, and per-group codec/transport/buffer.

For developers and the deeper protocol/architecture docs, see the
[repository docs](../README.md).
