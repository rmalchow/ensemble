# ensemble

**Self-organizing multiroom audio.** One small binary per device. Start it on
two machines and they find each other, form a group, and play the same music
in sync — no server, no database, no config files, no cloud.

```
┌── kitchen ──┐   ┌── living room ──┐   ┌── bedroom ──┐
│  ensemble   │◀─▶│    ensemble     │◀─▶│  ensemble   │
└─────────────┘   └────────┬────────┘   └─────────────┘
                           │ plays a track from its library,
                           ▼ streams it in lock-step to the whole group
                     🔊  🔊  🔊   one song, every room, in time
```

- **Zero-config** — nodes discover each other over mDNS + gossip and replicate
  their state automatically. Pull the plug on any node and the rest heal.
- **One universal binary** — no cgo, no build flavors. Opus and ALSA are loaded
  at runtime if present, so the same `linux/amd64` or `linux/arm64` build runs
  on a beefy desktop or a Raspberry Pi.
- **Synchronized playback** — a master-anchored clock plus a per-node rate servo
  keep rooms aligned; lost packets are recovered with FEC or TCP.
- **Built-in web UI** — every node serves a single-page app at `/`. Control the
  whole cluster from any one of them.
- **Plays your stuff** — local files (`wav` / `mp3` / `flac`), internet radio
  (`http(s)://…`), or a line-in capture. Browse any node's library from any UI.
- **Group as you like** — any node is a group of one by default; tell one to
  follow another and they merge. Name a combination of rooms and the name
  sticks whenever that set reforms.

> A node can also be a "dumb" speaker: the wire protocol is small and
> documented in [`docs/DUMB-CLIENT.md`](docs/DUMB-CLIENT.md) for firmware
> (e.g. an ESP32 + DAC), with a reference client in [`cmd/dumbclient`](cmd/dumbclient).

---

## Quick start

### Build

Needs Go 1.26 and Node (for the UI). Pure-Go, statically-ish linked:

```sh
git clone https://gitlab.rand0m.me/share/ensemble.git
cd ensemble
./scripts/build.sh --ui     # builds the SPA, then linux amd64 + arm64 into bin/,
                            # plus ./ensemble for this host
```

Prebuilt binaries are attached to each [tagged release](RELEASING.md).

**Running in Docker?** A multi-arch master image (amd64 + arm64, Spotify Connect
built in) plus a Compose example live in [`docker/`](docker/) — see the
[Docker quick start](QUICKSTART.md).

### Run

On each device:

```sh
./ensemble
```

That's it — defaults are sensible. The node mints a permanent ID on first run,
binds its ports, advertises itself over mDNS, and starts serving the UI. Run it
on a second machine on the same LAN and they find each other within seconds.

Open the UI in a browser:

```
http://<that-device's-ip>:8080
```

Put music in the node's library directory (created on first run):

```
<data-dir>/media/        # default: ./data/media — folders are browsed recursively
```

### Two nodes on one machine (to try it out)

```sh
./scripts/dev2.sh        # launches two nodes on 127.0.0.1 with null audio sinks
```

…or run the full end-to-end smoke test:

```sh
./scripts/e2e.sh
```

---

## Using the UI

Every node serves the same app and proxies to the others, so open
`http://<any-node-ip>:8080` on a phone or browser. There are two pages: the
**Rooms** overview (landing page) and the **Nodes** page (the **⚙ gear**,
top-right).

- **Rooms** — one card per group: now-playing (cover art / title / artist),
  group + per-member volumes, and — when you select a card — the add-players
  roster, the media browser, and advanced per-group settings (codec/transport/
  buffer).
- **Nodes** — per-node cards: addresses, feature toggles (playback/opus/input,
  plus a **spotify** badge), **Spotify endpoints** (Connect devices + presets),
  and settings (volume, hw delay, output device, test tone).

📖 **Full walkthrough with screenshots: [docs/user](docs/user/README.md).**

<p>
  <img src="docs/user/images/overview.png" width="240" alt="Rooms overview" />
  <img src="docs/user/images/nodes.png" width="240" alt="Nodes page" />
</p>

---

## Configuration

Flags (each has an `ENSEMBLE_*` env equivalent); all are optional:

| Flag | Default | What |
|------|---------|------|
| `--data <dir>` | `./data` | data dir: `node.json`, `cluster.json`, `media/` |
| `--media <dir>` | `<data>/media` | library directory (browsed recursively) |
| `--name <name>` | first 8 of the node ID | display name (first start only) |
| `--http-port` | `8080` | UI + REST API + WebSocket + node proxy |
| `--stream-port` | `9090` | member-side stream + clock sync (tcp+udp) |
| `--source-port` | `9200` | audio source: subscriptions + control (tcp+udp) |
| `--gossip-port` | `7946` | cluster gossip (tcp+udp) |
| `--host <addr>` | all interfaces | bind address |

Ports are **bind-or-increment** *when left at the default*: if one is taken the
node tries the next and advertises whatever it actually bound. A port you **set
explicitly** (flag or `ENSEMBLE_*_PORT` env) is **pinned** — it binds that exact
number or the node exits with a clear error, so a misconfiguration surfaces
immediately instead of silently drifting.

Env-only knobs:

| Env | Default | What |
|-----|---------|------|
| `ENSEMBLE_OUTPUT` | `auto` | output backend: `auto` \| `alsa` \| `exec` \| `null` \| `file:<path>` |
| `ENSEMBLE_ALSA_LATENCY_MS` | `200` | ALSA device buffer (raise on a glitchy Pi) |
| `ENSEMBLE_LOG` | `info` | log level (`debug` \| `info` \| `warn` \| `error`) |

`auto` picks the best available backend: **alsa → exec** (`pw-play`/`aplay`/…)
**→ null**. Pass `-v` for debug logging; `ensemble run …` is accepted as an
alias for `ensemble …`.

For a hermetic setup without mDNS (e.g. tests, or a segmented network), use
`--no-mdns` with `--join <host:gossipPort>,…` seeds.

---

## How it works (in one breath)

Every node runs the identical binary and is, by default, the master of a group
containing only itself. Telling node B to *follow* node A merges them; the only
replicated fact is each node's `following` field — group membership, the group
ID (the master's ID), and failover all fall out of that, recomputed everywhere
from gossiped state. When a group plays, its master opens the media source,
stamps each 20 ms PCM/Opus frame with a presentation time on the shared clock,
and streams it to every member (including itself) that has *subscribed* to its
source port. Each member runs a clock follower and a jitter-buffered, rate-
servo'd playout so the audio leaves every speaker at the same instant.

Deeper docs live in [`docs/`](docs/): [`README.md`](docs/README.md) is the full
spec, [`arch/`](docs/arch) has per-component design and the decision log,
[`DUMB-CLIENT.md`](docs/DUMB-CLIENT.md) is the wire protocol,
[`esp32.md`](docs/esp32.md) the ESP32-S2 hardware node, [`calibrate.md`](docs/calibrate.md)
the acoustic auto-calibration, and [`RELEASING.md`](RELEASING.md) covers releases.

## Status & scope

Works today: discovery, grouping, takeover, synchronized playback, pause/resume,
opus, per-node volume/delay/device, persisted names & settings, the web UI, and
a tag-driven release pipeline. Out of scope for now: auth/TLS, internet-facing
operation, playlists, and hardware-mixer volume. v1 is a trusted-LAN system.
