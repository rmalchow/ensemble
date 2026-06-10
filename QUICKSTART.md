# Ensemble — Docker quick start

This guide covers running an ensemble **master** in Docker for a few common
setups, including Spotify Connect via the bundled `go-librespot`. For the
non-Docker / on-device story (Raspberry Pi playback nodes, flags, the UI) see
the main [README](README.md).

> **A note on roles.** Every ensemble binary can be a *master* (owns cluster
> state, serves the UI/API, sources + streams audio) and/or a *playback* node
> (receives and plays). **This Docker image is master-only** — it's the brain +
> the music library, not a speaker. Your speakers are playback nodes (e.g.
> Raspberry Pis running the native binary). A master on a server with no sound
> card is exactly the intended shape.

---

## TL;DR

```sh
docker run -d --name ensemble-master \
  --network host \
  -v /srv/music:/media:ro \
  -v ensemble-data:/data \
  registry.gitlab.rand0m.me/share/ensemble:latest --name living-room
```

Open `http://<host-ip>:8080`, drop a playback node on the same LAN, and they
find each other within seconds. Or use Compose:

```sh
docker compose -f docker/docker-compose.yml up -d
```

---

## Why host networking?

The image deliberately publishes **no ports** and expects `--network host`.
Three things need real LAN presence, none of which survive a NAT bridge:

1. **mDNS discovery** — playback nodes find the master by multicast.
2. **The cluster ports** — gossip/stream/source bind-or-increment (try the next
   port if one is taken), so a fixed `-p host:container` mapping is the wrong
   model.
3. **Spotify Connect** — `go-librespot` advertises over zeroconf so your phone
   sees the device.

The ports a master binds (all overridable, all default to the spec values):

| Port | Env | Purpose |
|------|-----|---------|
| `8080` | `ENSEMBLE_HTTP_PORT` | UI + REST API + WebSocket + node proxy |
| `9090` | `ENSEMBLE_STREAM_PORT` | member stream + clock sync (tcp+udp) |
| `9200` | `ENSEMBLE_SOURCE_PORT` | audio source: subscriptions + control (tcp+udp) |
| `7946` | `ENSEMBLE_GOSSIP_PORT` | cluster gossip (tcp+udp) |

### Bridge-mode fallback (no discovery, no Spotify Connect)

If you genuinely can't use host networking (and accept that playback nodes won't
auto-discover the master and Spotify Connect won't be visible), run in bridge
mode with explicit mappings and wire the cluster manually with `--join`:

```yaml
services:
  ensemble:
    image: registry.gitlab.rand0m.me/share/ensemble:latest
    command: ["--no-mdns", "--name", "living-room"]
    ports:
      - "8080:8080"          # UI + API
      - "9090:9090/tcp"
      - "9090:9090/udp"      # member stream + clock sync
      - "9200:9200/tcp"
      - "9200:9200/udp"      # source subscriptions + control
      - "7946:7946/tcp"
      - "7946:7946/udp"      # gossip
    volumes:
      - /srv/music:/media:ro
      - ensemble-data:/data
```

Playback nodes then need `--no-mdns --join <host-ip>:7946`. The UI works, but
this is the unusual path — host networking is the recommended default.

---

## Volumes

| Container path | Mount | Why |
|----------------|-------|-----|
| `/media` | **read-only** (`:ro`) | Your music library. Browsed recursively. The master never writes here. |
| `/data` | **read-write** | `node.json` (stable ID + name), `cluster.json`, and go-librespot's credentials + audio FIFO. Persist this across restarts. |

`/media` is read-only on purpose — ensemble only reads your library. `/data`
must be writable and should be a named volume (or a host dir) so the node keeps
its identity and Spotify login across upgrades.

---

## Use case 1 — Master + local music library

A headless box (NAS, mini-PC, server) holding the library; speakers are separate
playback nodes.

```sh
docker run -d --name ensemble-master \
  --network host \
  -v /srv/music:/media:ro \
  -v ensemble-data:/data \
  registry.gitlab.rand0m.me/share/ensemble:latest --name house
```

Then bring up playback nodes on your speakers (native binary, not this image) —
on a Raspberry Pi:

```sh
./ensemble --role playback --name kitchen
```

They appear in the UI at `http://<master-ip>:8080`; group them, pick the master
library, and **Play here**.

> Want the master to *also* play audio out of the box it runs on? That needs the
> ALSA device, which is awkward and unusual inside a container — run the native
> binary with the default `master,playback` role on that host instead.

---

## Use case 2 — Spotify Connect (go-librespot, built in)

`go-librespot` is **baked into the image** (amd64 + arm64). On startup the master
advertises a Spotify Connect device named **`ensemble <name>`** (so `--name
living-room` → **`ensemble living-room`**). Nothing else to install.

```sh
docker run -d --name ensemble-master \
  --network host \
  -v /srv/music:/media:ro \
  -v ensemble-data:/data \
  registry.gitlab.rand0m.me/share/ensemble:latest --name living-room
```

Then, on your phone:

1. Make sure the phone is on the **same LAN** (host networking + zeroconf — this
   is why bridge mode can't do it).
2. Open Spotify → **Connect to a device** → pick **`ensemble living-room`**.
3. Hit play. The master auto-switches that group to the Spotify source and
   streams it to every speaker in the group; pausing/stopping in Spotify returns
   the group to idle.

Notes:
- Spotify **Premium** is required (a Connect/`go-librespot` constraint).
- Login/credentials persist in `/data` — keep that volume and you authenticate
  once.
- Audio is piped from go-librespot as s16le 44.1 kHz; its localhost event API
  (port 3678) stays inside the container.

---

## Use case 3 — Multiple masters (zones)

Run one container per zone/library on different hosts; each is its own master and
appears in the UI. Give each a distinct `--name` (and therefore a distinct
Spotify device) and its own `/data` volume. They share the same LAN and gossip,
so any node's UI shows them all.

```sh
# host A
docker run -d --network host -v /srv/music:/media:ro -v ensemble-a:/data \
  registry.gitlab.rand0m.me/share/ensemble:latest --name downstairs
# host B
docker run -d --network host -v /srv/jazz:/media:ro -v ensemble-b:/data \
  registry.gitlab.rand0m.me/share/ensemble:latest --name studio
```

---

## Building the image yourself

Multi-arch (amd64 + arm64) with `buildx`. The build is self-contained — it builds
the SPA and cross-compiles the binary in its own stages:

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f docker/Dockerfile \
  --build-arg VERSION="$(git describe --tags --always)" \
  -t registry.example/ensemble:latest \
  --push .
```

For a quick single-arch local image, drop `--platform`/`--push` and add `--load`:

```sh
docker buildx build -f docker/Dockerfile -t ensemble:dev --load .
```

CI builds and pushes this on the default branch (`:dev`) and on release tags
(`:<tag>` + `:latest`) — see `.gitlab-ci.yml`.

---

## Configuration reference

Everything is set via `ENSEMBLE_*` env (Compose `environment:`) or flags after
the image name. The most useful in a container:

| Env / flag | Default | What |
|------------|---------|------|
| `ENSEMBLE_ROLE` | `master` (in image) | role set; keep `master` here |
| `ENSEMBLE_MEDIA_DIR` / `--media` | `/media` | library directory |
| `ENSEMBLE_DATA_DIR` / `--data` | `/data` | node + cluster state, Spotify creds |
| `--name` | first 8 of node ID | display name + Spotify device name (first start only) |
| `ENSEMBLE_HTTP_PORT` / `--http-port` | `8080` | UI + API |
| `ENSEMBLE_STREAM_PORT` / `--stream-port` | `9090` | member stream + clock |
| `ENSEMBLE_SOURCE_PORT` / `--source-port` | `9200` | source subscriptions + control |
| `ENSEMBLE_GOSSIP_PORT` / `--gossip-port` | `7946` | cluster gossip |
| `ENSEMBLE_LOG` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `--no-mdns` + `--join <ip:7946>` | — | hermetic / bridge-mode clustering |

See the [README configuration table](README.md#configuration) for the complete
list.
