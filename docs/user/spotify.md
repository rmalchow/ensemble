# Spotify Connect

> **You are here:** [User Guide](README.md) › **Spotify Connect**
> Scenarios: [NAS / server master](scenarios/nas-master.md) · [Raspberry Pi player](scenarios/raspberry-pi.md) · [Desktop / laptop](scenarios/desktop.md) · [ESP32 node](scenarios/esp32.md)

A node that can find a **go-librespot** binary advertises a Spotify Connect device
named **`ensemble <name>`**. On your phone: Spotify → **Connect to a device** → pick
it → press play, and the node's master switches that group to the Spotify source and
streams it to every speaker in the group. Works for **podcasts** too — anything you
play in the Spotify app. Pausing/stopping in Spotify returns the group to idle.

- **Spotify Premium is required** — a Spotify Connect / go-librespot constraint.
- **Login persists** in the data dir (`/data` in Docker, `--data` natively) —
  authenticate once, keep the dir, and it survives restarts and upgrades.
- You can advertise **multiple endpoints** (e.g. an `all` device that groups every
  speaker, vs. the default that plays wherever the node currently masters). Edit
  them in the UI — see [Spotify endpoints](ui-reference.md#spotify-endpoints).

ensemble does **not** bundle go-librespot in the native binary — it's a separate
open-source project that you add alongside ensemble. (The
[Docker master image](#docker-nothing-to-install) is the exception: it already
includes it.)

---

## Docker: nothing to install

The official master image **bakes in go-librespot** — Spotify Connect works out of
the box, nothing to download. It's built on the upstream
[`ghcr.io/devgianlu/go-librespot:v0.7.3`](https://github.com/devgianlu/go-librespot/pkgs/container/go-librespot)
image, so the binary is already on `PATH` inside the container. Just persist `/data`
so the Spotify login survives upgrades. See the
[NAS / master scenario](scenarios/nas-master.md#the-fastest-path-docker).

---

## Native: install go-librespot

go-librespot is a separate, open-source project:

- **Source & docs:** [`github.com/devgianlu/go-librespot`](https://github.com/devgianlu/go-librespot)
- **Prebuilt binaries:** [**releases → latest**](https://github.com/devgianlu/go-librespot/releases/latest)
  (Linux `amd64` / `arm64` / `armv6`, macOS, …)

ensemble is verified against **go-librespot 0.7.3** — match that release or newer.

### 1. Get the binary

```sh
# Linux arm64 (64-bit Raspberry Pi OS) — adjust the asset name for your platform:
curl -L -o go-librespot.tar.gz \
  https://github.com/devgianlu/go-librespot/releases/latest/download/go-librespot_linux_arm64.tar.gz
tar xzf go-librespot.tar.gz go-librespot      # extracts the `go-librespot` binary
chmod +x go-librespot
```

Or, on macOS / Linux with Homebrew:

```sh
brew install go-librespot
```

Or build it from source — see the project's README.

### 2. Put it where ensemble looks

At startup ensemble looks for an executable named `go-librespot` (then `librespot`)
**in its working directory first, then on `$PATH`.** The simplest deployment is to
drop the binary right next to the `ensemble` binary:

```sh
ls
# ensemble  go-librespot
./ensemble --name house        # the startup banner reports the resolved go-librespot path
```

(If you installed via `brew` or dropped it in `/usr/local/bin`, it's already on
`$PATH` — no need to copy it next to `ensemble`.)

### 3. Authenticate once

Start ensemble, open your phone's Spotify app, and pick the **`ensemble <name>`**
device under **Connect to a device**. The login token is written into the data dir
and reused on every restart.

---

## How it works (and what to expect)

- ensemble launches and supervises the `go-librespot` process itself; you don't run
  it separately. Audio is taken from it as **s16le 44.1 kHz over a local pipe**, and
  its localhost event API stays internal to ensemble.
- go-librespot advertises the Connect device over **zeroconf**, so the device needs
  real LAN presence — under Docker that means `--network host` (see
  [why host networking](scenarios/nas-master.md#why-host-networking)).
- **If no binary is found, Spotify is simply disabled** and a line in the startup
  banner says so — everything else keeps working.

> **On a pure playback node** (e.g. a [Raspberry Pi player](scenarios/raspberry-pi.md)
> or an [ESP32 node](scenarios/esp32.md)) you **don't** need go-librespot — it just
> plays whatever its group's master is sourcing, including a Spotify session started
> on the master. Only install go-librespot on a node you want to *advertise its own*
> Connect device.

---

**See also:** [UI Reference — Spotify endpoints](ui-reference.md#spotify-endpoints) ·
[Configuration Reference](config-reference.md) · [User Guide](README.md)
