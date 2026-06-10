# Running ensemble

> **You are here:** [User Guide](README.md) › **Running ensemble**
> How to *start* a node and keep it running. What to put in `--name`, `--role`,
> and friends is in the [Configuration Reference](config-reference.md); which kind
> of node to run where is in the [scenarios](README.md#pick-your-setup).

Ensemble is a single process. Every method below runs the **same** binary (or
image) with the **same** flags — they differ only in how the process is supervised
and how long it lives. Pick by how permanent you need it:

| You want… | Use |
|-----------|-----|
| To try it / watch the logs | [Foreground](#foreground) |
| A quick background process on a box you're SSH'd into | [`nohup &`](#background-with-nohup) |
| A permanent speaker on a Pi / headless Linux (ALSA) | [systemd **system** service](#systemd-system-service) |
| A permanent node on your own desktop (PipeWire/PulseAudio) | [systemd **user** service](#systemd-user-service) |
| A master on a NAS / server / container host | [Docker](#docker) or [Docker Compose](#docker-compose) |

---

## Foreground

The simplest way — run it in your terminal:

```sh
./ensemble
```

It prints a startup banner (bound ports, audio backend, whether go-librespot was
found) and keeps running until you press **Ctrl-C**. Add `-v` for debug logging.
Best for the first run, a quick demo, or debugging — see the
[desktop quick start](scenarios/desktop.md#quick-start-one-machine).

---

## Background with `nohup`

To leave it running after you close the terminal or log out, without setting up a
service:

```sh
nohup ./ensemble --name kitchen > ensemble.log 2>&1 &
```

- `nohup … &` detaches it from the shell; `> ensemble.log 2>&1` captures its output.
- Find it later with `pgrep -af ensemble`; stop it with `kill <pid>` (or
  `pkill -f '/ensemble'`).

> **Caveat:** `nohup` does **not** restart ensemble after a crash or a reboot. For
> anything you want to "just always be there," use a systemd service or Docker with
> a restart policy instead.

---

## systemd (system service)

The right choice for an **always-on speaker** on a Raspberry Pi or any headless
Linux box using **ALSA** directly. It starts on boot, restarts on failure, and
logs to the journal.

Install the binary and a data dir, then drop in a unit:

```sh
sudo install -Dm755 ensemble /opt/ensemble/ensemble
sudo mkdir -p /var/lib/ensemble
```

`/etc/systemd/system/ensemble.service`:

```ini
[Unit]
Description=ensemble multiroom audio node
After=network-online.target sound.target
Wants=network-online.target

[Service]
Type=simple
# A user with access to the sound card. On Raspberry Pi OS, `pi` is in `audio`
# already; otherwise create a dedicated user and add it to the audio group:
#   sudo useradd --system --home /var/lib/ensemble --groups audio ensemble
User=pi
SupplementaryGroups=audio
WorkingDirectory=/opt/ensemble
Environment=ENSEMBLE_DATA_DIR=/var/lib/ensemble
# Default name = the machine's hostname; replace with --name kitchen if you like.
ExecStart=/opt/ensemble/ensemble --role playback --name %H
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Enable and manage it:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now ensemble      # start now + on every boot
systemctl status ensemble                 # is it up?
journalctl -u ensemble -f                 # follow its logs / banner
sudo systemctl restart ensemble           # after editing the unit / upgrading
sudo systemctl disable --now ensemble     # stop + don't start on boot
```

Notes:
- Drop `--role playback` if this box should also host a library / be a master
  (see [roles](config-reference.md#3-roles)).
- The unit pins `ENSEMBLE_DATA_DIR` so the node's identity lives at a stable path
  regardless of the working directory. To use **Spotify** on a system service, put
  the `go-librespot` binary in `WorkingDirectory` (or on `$PATH`) —
  [details](scenarios/nas-master.md#where-to-get-go-librespot).

> **Why a *system* service for a Pi but a *user* service for your desktop?** A
> system service runs outside any login session, which is perfect for raw ALSA but
> has **no access to a per-user PipeWire/PulseAudio session**. If your machine
> routes audio through PipeWire/Pulse (most desktops), use the user service below.

---

## systemd (user service)

The right choice on a **desktop/laptop** whose audio goes through **PipeWire or
PulseAudio**: a user service inherits your audio session automatically — no
`audio`-group fiddling, no backend surprises.

`~/.config/systemd/user/ensemble.service`:

```ini
[Unit]
Description=ensemble multiroom audio node (user)
After=default.target

[Service]
Type=simple
# Runs from your home dir; ./data and ./go-librespot resolve there.
WorkingDirectory=%h/ensemble
ExecStart=%h/ensemble/ensemble --name %u-desktop
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

Enable and manage it (note: **`--user`**, and no `sudo`):

```sh
mkdir -p ~/.config/systemd/user
# (save the unit above, with your ensemble binary at ~/ensemble/ensemble)
systemctl --user daemon-reload
systemctl --user enable --now ensemble
systemctl --user status ensemble
journalctl --user -u ensemble -f
systemctl --user restart ensemble
```

To keep it running **after you log out / before you log in** (e.g. a headless
desktop that should be a speaker at boot), enable lingering once:

```sh
sudo loginctl enable-linger "$USER"
```

---

## Docker

The official image is **master-only** and bakes in go-librespot. Run it on a NAS,
mini-PC, or server (full rationale in the
[NAS / server guide](scenarios/nas-master.md)):

```sh
docker run -d --name ensemble-master \
  --network host \
  -v /srv/music:/media:ro \
  -v ensemble-data:/data \
  --restart unless-stopped \
  harbor.rand0m.me/public/ensemble:latest --name living-room
```

- `--network host` is **required** — playback nodes find the master over mDNS and
  go-librespot advertises Spotify over zeroconf; neither survives a NAT bridge
  ([why](scenarios/nas-master.md#why-host-networking)).
- `--restart unless-stopped` brings it back after a crash or host reboot.

Manage it:

```sh
docker logs -f ensemble-master
docker restart ensemble-master
docker stop ensemble-master && docker rm ensemble-master
```

---

## Docker Compose

The same thing, declarative. A ready-to-use file ships at
[`docker/docker-compose.yml`](../../docker/docker-compose.yml); a minimal version:

```yaml
services:
  ensemble:
    image: harbor.rand0m.me/public/ensemble:latest
    container_name: ensemble-master
    network_mode: host          # required — see above
    restart: unless-stopped
    environment:
      ENSEMBLE_ROLE: master      # this image is master-only
      ENSEMBLE_MEDIA_DIR: /media
      ENSEMBLE_DATA_DIR: /data
    command: ["--name", "living-room"]
    volumes:
      - /srv/music:/media:ro     # your library — READ-ONLY
      - ensemble-data:/data      # node identity, cluster state, Spotify creds
volumes:
  ensemble-data:
```

```sh
docker compose up -d            # start in the background
docker compose logs -f          # follow logs
docker compose pull && docker compose up -d   # upgrade to a newer image
docker compose down             # stop + remove (named volume persists)
```

---

**See also:** [Configuration Reference](config-reference.md) ·
[Pick your setup](README.md#pick-your-setup) ·
[NAS / server master](scenarios/nas-master.md)
