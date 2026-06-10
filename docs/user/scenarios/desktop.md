# Setup: Desktop / laptop with speakers

> **You are here:** [User Guide](../README.md) › Scenarios › **Desktop / laptop**
> Other setups: [NAS / server master](nas-master.md) · [Raspberry Pi player](raspberry-pi.md) · [ESP32 node (in dev)](esp32.md)

## What this is, and why

The computer **already on your desk** — a Linux desktop, a laptop, an old machine
in the corner — using **its existing sound card and speakers**. Run ensemble on it
and it becomes a node like any other: it can play out its speakers *and* (because
it has storage) host a library and master a group.

**Why start here?**

- **Zero new hardware.** You already own the computer and the speakers; this is the
  fastest way to hear ensemble working.
- **It's both player and master.** Unlike a thin Pi or a headless NAS, a desktop can
  do everything — so a single desktop is a complete, one-machine ensemble, and a
  room full of desktops needs no separate server at all.
- **Great for an office.** In the **shared studio** every desk is already a capable
  node; group them and you've got whole-floor sound with nothing extra to buy.

This is the heart of the **studio** example, and a fine alternative to a NAS for the
**flatshare** (let one always-on desktop host the shared library).

---

## Quick start: one machine

The fastest way to see ensemble run at all:

```sh
./ensemble                       # no flags: sensible defaults, both roles
```

Then:

1. Open **`http://localhost:8080`**.
2. Drop a few audio files into the library folder it created on first run —
   `./data/media` (default). They appear in the media browser immediately.
3. Select the room, browse to a track, hit **Play here** — it plays out your
   computer's speakers.

That's a complete, if lonely, ensemble. The magic starts with a second node.

---

## Add a second node (the studio floor)

Run ensemble on a colleague's machine on the same network — same command — and the
two **find each other within seconds** over mDNS. Now:

- Open **any** of the machines' addresses (they all serve the same app and proxy to
  each other).
- Drag both desktops into **one room**: they play the same track in lock-step across
  the floor.
- Whoever's at a keyboard can skip, pause, or change the volume from their own
  browser. No pairing, no "whose phone is connected" — it's just a web page.

Three colleagues, three desktops, one soundtrack: that's the studio.

> **Library on one desk.** You don't need a NAS — point one always-on desktop at
> your shared music folder and let it be the master:
>
> ```sh
> ./ensemble --name studio --media /shared/music
> ```
>
> The other desks can stay pure players if you prefer (`--role playback`), or remain
> full nodes and host their own libraries too. See
> [roles](../config-reference.md#3-roles).

---

## Audio output on a desktop

Most Linux desktops run **PipeWire** or **PulseAudio**. Ensemble's `auto` backend
handles this:

- It uses **ALSA** directly where it can.
- If you'd rather route through the desktop's sound server, set the **exec** backend
  so audio is piped to a player command:

  ```sh
  ENSEMBLE_OUTPUT=exec ./ensemble --name desk
  ```

  (`auto` already falls back to `exec`/`pw-play`/`aplay` when direct ALSA isn't the
  right choice.) Full backend details:
  [Config Reference](../config-reference.md#6-audio-output-backend).

- Pick the specific output (headphones vs. monitors vs. an HDMI display) from the
  node's **Settings → output device** in the UI, and confirm it with **♪ test
  tone**.

---

## Spotify & podcasts from a desktop

Want this desktop to be the thing your phone casts Spotify (or podcasts) to? Give it
a **go-librespot** binary and it advertises an `ensemble <name>` Connect device:

```sh
# put go-librespot next to the ensemble binary, then run as usual
ls            # ensemble  go-librespot
./ensemble --name studio
```

The native binary does **not** bundle go-librespot (only the Docker master image
does) — it's a separate, open-source project you add yourself. Where to download it,
which release to match, and exactly how ensemble locates it (working directory
first, then `$PATH`) is in
**[Spotify Connect](../spotify.md#native-install-go-librespot)**. Spotify
**Premium** is required; the login is saved in the data dir.

---

## Keep it running

- **Just trying it:** run it in a terminal; `Ctrl-C` stops it.
- **Always-on desktop / office machine:** run it under your session manager or a
  **user** systemd service so it starts with the machine. The pattern mirrors the
  [Pi service](raspberry-pi.md#run-it-on-boot) — drop the `--role playback` line if
  you want the desktop to be a full node (player **and** master/library).

---

## Verify it works

1. `http://localhost:8080` shows the Rooms page and the node's own card.
2. Files dropped in `./data/media` appear in the media browser.
3. **Play here** produces sound from the chosen output (check with **♪ test tone**).
4. A second machine on the LAN shows up as a node within seconds; grouped, the two
   play in sync.

---

**See also:** [Configuration Reference](../config-reference.md) ·
[UI Reference](../ui-reference.md) · [NAS / server master](nas-master.md) ·
[Raspberry Pi player](raspberry-pi.md)
