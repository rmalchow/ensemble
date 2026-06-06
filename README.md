# Ensemble

**Sample-accurate multiroom audio that organizes itself.** Power on a player, it's
discovered and adopted into your cluster, and it plays in lock-step with every other
speaker in its group — no controller box, no cloud, no config files.

> **Status:** design complete, implementation starting. The full architecture,
> protocols, interfaces, API, UI, security model, and build pipeline are specified in
> **[`doc/`](./doc/README.md)** — start with [`doc/README.md`](./doc/README.md). Code
> lands phase by phase (see [`doc/10-roadmap-and-dumb-nodes.md`](./doc/10-roadmap-and-dumb-nodes.md)).

---

## Why Ensemble

- **Truly synchronized.** A per-group master decodes audio once and unicasts timestamped
  chunks; every player locks to a shared clock and continuously micro-resamples to hold
  **sub-millisecond** alignment — tight enough for a real stereo image split across two
  speakers on WiFi.
- **Self-organizing.** Zero-config LAN discovery, gossip-replicated cluster state, and
  automatic per-group master election with seamless failover. Any node's web UI operates
  the *whole* cluster — adopt, configure, and play from anywhere.
- **Grouped however you like.** Any number of independent groups, each with its own clock,
  its own media, and its own membership. A node is a group of one, or one of many.
- **One static binary, no dependencies.** Pure Go — no `libasound`, no cgo, no ffmpeg.
  Precise audio comes from talking to the kernel directly; if a box can't do precise, it
  gracefully falls back. Cross-compiles to a Raspberry Pi with **zero toolchain**.
- **Secure by design.** All control traffic is mutually authenticated (mTLS); realtime
  traffic is accepted only from known cluster addresses. Nodes join via a PIN-gated
  certificate exchange.
- **Runs on cheap hardware.** A 512 MB quad-core Pi (3 A+ / Zero 2 W) is a comfortable
  full node. Headless media-only nodes (e.g. a NAS container) can drive a group without
  any audio output of their own.

## How it works

```
        ┌── group "Living Room" ──────────────┐     ┌── group "Kitchen" ──┐
 mp3/   │  master: decode → chunk → unicast ──┼──►  │  master + listener  │
 FLAC/  │  + shared clock                     │     └─────────────────────┘
 HTTP   │     └─► player (left)  player (right)│
        └─────────────────────────────────────┘
  one cluster · one CA · gossip-replicated config · mTLS control plane
```

Each player maps the group's stream timeline to its own sound hardware, correcting for
its DAC's clock drift and a one-time per-speaker delay trim — so the room stays in phase.

## Quick start

> Buildable once the implementation lands; the pipeline and prerequisites are specified
> now in [`doc/11-build-ci-and-release.md`](./doc/11-build-ci-and-release.md).

```bash
# build (pure Go; Node only to bundle the web UI)
cd web && npm ci && npm run build && cd ..
CGO_ENABLED=0 go build -o bin/ensemble ./cmd/ensemble

# cross-compile for a Raspberry Pi — no C toolchain needed
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/ensemble-arm64 ./cmd/ensemble

# run a node (first one creates the cluster via the setup wizard)
./bin/ensemble
```

Prebuilt binaries for `linux/amd64`, `arm64`, and `armv7` are published as GitLab CI
artifacts and on the project's **Releases** page (see the build doc).

## Hardware

- **Player node:** any Linux box; a 512 MB quad-A53 Pi is plenty. An I2S DAC HAT or HDMI
  audio is recommended over the noisy analog jack.
- **Network:** wired or WiFi (prefer 5 GHz; the buffer + FEC absorb jitter). For large
  groups, prefer a wired master.

## Documentation

The complete specification lives in **[`doc/`](./doc/README.md)**:

| | |
|---|---|
| [Spine / decisions / contracts](./doc/README.md) | [Architecture](./doc/01-architecture-and-packages.md) |
| [Cluster & discovery](./doc/02-cluster-discovery-membership.md) | [Security & adoption](./doc/03-adoption-takeover-security-pki.md) |
| [Clock & groups](./doc/04-clock-and-groups.md) | [Streaming protocol](./doc/05-audio-streaming-protocol.md) |
| [Audio output & sync](./doc/06-audio-output-scheduling.md) | [Config & replication](./doc/07-config-and-replication.md) |
| [HTTP API](./doc/08-http-api-reference.md) | [UI screens](./doc/09-ui-screens.md) |
| [Roadmap & dumb nodes](./doc/10-roadmap-and-dumb-nodes.md) | [Build, CI & release](./doc/11-build-ci-and-release.md) |
| [Appendix: algorithms, pinned deps, parameters](./doc/A-appendix-algorithms-and-pinned-choices.md) | |

## Clone

```bash
git clone https://gitlab.rand0m.me/ruben/go/ensemble.git
```

## License

_TBD._
