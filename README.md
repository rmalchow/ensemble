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

Every node serves the same app; it talks to its own node and proxies to the
others, so it doesn't matter which one you open. Three sections:

### Groups
Each card is a group (one or more rooms playing together).
- The **master** is badged; everyone else follows it.
- Per-member **volume sliders** (live), a **✕** to leave the group, and an
  **Add node…** dropdown to pull another node in.
- **▶ / ⏸** play/pause and **stop** for the group; the playing track and
  position are shown, with listener/reconnect counts on the master.
- Per-group **codec** (pcm/opus), **transport** (udp/tcp) and **buffer** (ms).
  On flaky Wi-Fi, prefer **opus** (small packets) — pcm datagrams fragment.
- Click a group's name to rename it. The name is tied to that *set of rooms*,
  so it returns whenever they regroup; unnamed groups show a derived label like
  `kitchen + living room`.

### Nodes
Every known node: name (click to rename), ID, addresses, ports, and live/stale
status. Per node you also get a **volume** slider, an **output-delay** slider
(0–150 ms, to nudge one room earlier/later for perfect alignment), the **output
device** picker (ALSA hosts), a **♪ test tone** button to check a speaker, and
**feature toggles** (playback / opus / input — green = on, amber = off,
dimmed = unavailable on that host).

### Media
Pick any node, browse its library (with folders), and hit **Play here** — if
that node isn't currently its group's master, mastership moves to it first, then
it plays to the whole group. There's also a field to play an `http(s)://` stream
and a button to play the node's line-in.

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

Ports are **bind-or-increment**: if one is taken the node tries the next, and
advertises whatever it actually bound.

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
[`DUMB-CLIENT.md`](docs/DUMB-CLIENT.md) is the wire protocol, and
[`RELEASING.md`](RELEASING.md) covers tagging a release.

## Status & scope

Works today: discovery, grouping, takeover, synchronized playback, pause/resume,
opus, per-node volume/delay/device, persisted names & settings, the web UI, and
a tag-driven release pipeline. Out of scope for now: auth/TLS, internet-facing
operation, playlists, and hardware-mixer volume. v1 is a trusted-LAN system.
