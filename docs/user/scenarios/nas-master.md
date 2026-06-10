# Setup: Headless NAS / server master

> **You are here:** [User Guide](../README.md) › Scenarios › **NAS / server master**
> Other setups: [Raspberry Pi player](raspberry-pi.md) · [Desktop / laptop](desktop.md) · [ESP32 node (in dev)](esp32.md)

## What this is, and why

A **master** with **no speakers**: a NAS in the cupboard, a mini-PC under the TV, an
old server in the storage room. It holds your music library and does all the
streaming work, but never makes a sound itself. It's the **always-on brain** of the
house.

**Why run one?**

- **Your library lives in one place.** No copying folders onto every device; every
  room browses the same collection.
- **It keeps playing when everything else sleeps.** Laptops close, phones leave the
  house — the NAS stays up, so music (and the kids' audiobooks) never depends on a
  particular computer being awake.
- **It's the natural home for Spotify Connect.** One always-on device advertising
  `ensemble <name>` to every phone in the house.

In our running examples this is the **family's** library box in the cupboard and the
**studio's** shared library on the office NAS. (The three flatmates may skip it and
let one of their desktops host the library instead — see
[desktop](desktop.md).)

> **Roles recap.** This box is `--role master`: cluster brain + library + audio
> source, but not a speaker. Your speakers are separate **playback** nodes —
> [Raspberry Pis](raspberry-pi.md) or [desktops](desktop.md). A master with no
> sound card is exactly the intended shape. See
> [roles](../config-reference.md#3-roles).

---

## The fastest path: Docker

The official image is **master-only**, multi-arch (amd64 + arm64), and **bakes in
go-librespot** so Spotify Connect works out of the box. It deliberately publishes no
ports and expects host networking (see [why](#why-host-networking)).

```sh
docker run -d --name ensemble-master \
  --network host \
  -v /srv/music:/media:ro \
  -v ensemble-data:/data \
  harbor.rand0m.me/public/ensemble:latest --name house
```

That's the whole master. Open `http://<nas-ip>:8080`, then bring up a
[Raspberry Pi](raspberry-pi.md) or [desktop](desktop.md) player on the same network
and they find each other within seconds. Group them in the UI, pick a track from the
library, and hit **Play here**.

Prefer Compose? An example lives at [`docker/docker-compose.yml`](../../../docker/docker-compose.yml):

```sh
docker compose -f docker/docker-compose.yml up -d
```

### Running natively instead

If your NAS/server runs the binary directly (no Docker), it's the same idea —
restrict it to the master role so it never tries to grab a sound card:

```sh
ENSEMBLE_ROLE=master ENSEMBLE_MEDIA_DIR=/srv/music ./ensemble --name house
```

Prebuilt binaries are attached to each [release](../../../RELEASING.md); building
from source is covered in the [project README](../../../README.md#build).

---

## The music library

| Container path | Native equivalent | Notes |
|----------------|-------------------|-------|
| `/media` (mount **read-only**, `:ro`) | `--media <dir>` / `ENSEMBLE_MEDIA_DIR` | Your music. Browsed **recursively** — folders become navigable folders in the UI. Read-only on purpose: ensemble only ever reads it. |
| `/data` (mount **read-write**) | `--data <dir>` / `ENSEMBLE_DATA_DIR` | `node.json` (stable ID + name), `cluster.json`, and go-librespot's credentials + audio FIFO. **Persist this** (a named volume or host dir) so the node keeps its identity and Spotify login across upgrades. |

Supported file types: `wav`, `mp3`, `flac`. Point `/media` (or `--media`) at your
existing collection — for the family, that's the shared audiobooks + music folder;
for the studio, the office music share. See the
[Config Reference](../config-reference.md#2-identity--directories) for the full
directory semantics.

---

## Spotify Connect & podcasts

A node that can find a **go-librespot** binary advertises a Spotify Connect device
named **`ensemble <name>`**. On your phone: Spotify → **Connect to a device** → pick
it → press play, and the master switches that group to the Spotify source and
streams it to every speaker in the group. Works for **podcasts** too — anything you
play in the Spotify app. Pausing/stopping in Spotify returns the group to idle.

- **Spotify Premium is required** (a Spotify Connect / go-librespot constraint).
- **Login persists** in the data dir — authenticate once, keep `/data`, and it
  survives restarts and upgrades.
- You can advertise **multiple endpoints** (e.g. an `all` device that groups every
  speaker, vs. the default that plays wherever the node currently masters). Edit
  them in the UI — see [Spotify endpoints](../ui-reference.md#spotify-endpoints).

### Where to get go-librespot

- **Docker — included:** nothing to do. The official master image already contains
  go-librespot (built on the upstream `ghcr.io/devgianlu/go-librespot:v0.7.3`
  image), so Spotify Connect works out of the box.
- **Native node:** install the separate `go-librespot` binary and drop it next to
  `ensemble` (or on `$PATH`) — full steps, release links, and how ensemble locates
  it are in **[Spotify Connect](../spotify.md#native-install-go-librespot)**.

If no binary is found, Spotify is simply disabled and a line in the startup banner
says so — everything else keeps working.

---

## Why host networking?

The Docker image expects `--network host` and publishes no ports. Three things need
real LAN presence, none of which survive a NAT bridge:

1. **mDNS discovery** — players find the master by multicast.
2. **Bind-or-increment cluster ports** — gossip/stream/source try the next port if
   one is taken, so a fixed `-p host:container` mapping is the wrong model. (See
   [ports](../config-reference.md#4-ports).)
3. **Spotify Connect** — go-librespot advertises over zeroconf so your phone sees
   the device.

The ports a master binds (all default, all overridable) are listed in the
[Config Reference](../config-reference.md#4-ports). The image does **not** pin them;
if you set an `ENSEMBLE_*_PORT` yourself it becomes pinned (binds exactly or exits).
The startup banner prints the actual bound ports.

### Bridge-mode fallback

If you truly can't use host networking — and accept that players won't auto-discover
the master and Spotify Connect won't be visible — run bridge mode with explicit port
mappings and wire the cluster by hand:

```yaml
services:
  ensemble:
    image: harbor.rand0m.me/public/ensemble:latest
    command: ["--no-mdns", "--name", "house"]
    ports:
      - "8080:8080"            # UI + API
      - "9090:9090/tcp"
      - "9090:9090/udp"        # member stream + clock sync
      - "9200:9200/tcp"
      - "9200:9200/udp"        # source subscriptions + control
      - "7946:7946/tcp"
      - "7946:7946/udp"        # gossip
    volumes:
      - /srv/music:/media:ro
      - ensemble-data:/data
```

Players then need `--no-mdns --join <nas-ip>:7946`. This is the unusual path — host
networking is strongly recommended. More on
[discovery & `--join`](../config-reference.md#5-discovery--clustering).

---

## Multiple masters (zones)

Run one master per zone/library on different hosts; each is its own master, has its
own `--name` (and therefore its own Spotify device) and its own `/data` volume, and
all of them appear together in any node's UI because they share the LAN and gossip.

```sh
# storage room
docker run -d --network host -v /srv/music:/media:ro -v ensemble-a:/data \
  harbor.rand0m.me/public/ensemble:latest --name downstairs
# studio
docker run -d --network host -v /srv/jazz:/media:ro -v ensemble-b:/data \
  harbor.rand0m.me/public/ensemble:latest --name studio
```

---

## Verify it works

1. The container/process is up and the banner printed the bound ports (and whether
   go-librespot was found).
2. `http://<nas-ip>:8080` shows the Rooms page with your master's card.
3. The library browser (select the room → **Media**) lists your folders.
4. A [player node](raspberry-pi.md) on the same LAN appears within seconds; group
   it and **Play here** a track — you hear it on the player, not the NAS.
5. For Spotify: the `ensemble <name>` device shows up in your phone's Spotify app.

---

**See also:** [Configuration Reference](../config-reference.md) ·
[UI Reference](../ui-reference.md) · the Docker-focused
[QUICKSTART](../../../QUICKSTART.md) in the repo root.
