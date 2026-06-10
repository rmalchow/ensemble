# Ensemble MASTER image — built ON TOP of the upstream go-librespot image, so
# Spotify Connect comes for free (binary already on PATH, audio libs present);
# we only add the ensemble binary. Multi-arch (amd64 + arm64) follows
# go-librespot's published platforms. go-librespot 0.7.3 is the version the
# Spotify bridge (internal/spotify) is verified against.
#
#   docker run --rm --network host \
#     -v /srv/music:/media:ro -v ensemble-data:/data \
#     registry.example/ensemble:latest --name living-room
#
# --network host is required in practice: mDNS discovery, the gossip/stream/
# source ports, and Spotify Connect's zeroconf all need real LAN reachability,
# and ports bind-or-increment when taken — so there is nothing to publish/EXPOSE.
# See ../QUICKSTART.md for full setup.

# ---- stage 1: build the embedded UI --------------------------------------
FROM node:24-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build          # emits /src/web/dist

# ---- stage 2: cross-compile the binary (CGO off → no emulation) ----------
# Runs on the BUILD platform and cross-compiles to $TARGETARCH; a pure-Go static
# binary runs as-is on the alpine base.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=linux
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Real SPA over the committed placeholder so //go:embed all:dist picks it up.
COPY --from=frontend /src/web/dist ./web/dist
ARG VERSION=docker
RUN GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/ensemble ./cmd/ensemble

# ---- stage 3: runtime = upstream go-librespot (alpine) + ensemble --------
FROM ghcr.io/devgianlu/go-librespot:v0.7.3
# The base already has go-librespot at /usr/bin/go-librespot (on PATH, found by
# ensemble's bridge) plus alsa-lib/avahi. Add only what ensemble needs: libopus
# (master stream-encode, dlopen'd) and certs (TLS media sources). One apk layer;
# everything else is COPY, so the multi-arch build stays trivial.
RUN apk add --no-cache opus ca-certificates

COPY --from=builder /out/ensemble /usr/local/bin/ensemble

# Master-only. Media on /media (mount read-only), mutable state on /data. Every
# port the master binds is overridable here (all default to the spec values) and
# bind-or-increments if taken, so nothing is EXPOSEd (host networking only).
ENV ENSEMBLE_ROLE=master \
    ENSEMBLE_MEDIA_DIR=/media \
    ENSEMBLE_DATA_DIR=/data \
    ENSEMBLE_HTTP_PORT=8080 \
    ENSEMBLE_STREAM_PORT=9090 \
    ENSEMBLE_SOURCE_PORT=9200 \
    ENSEMBLE_GOSSIP_PORT=7946
VOLUME ["/media", "/data"]

# Override the base entrypoint — ensemble launches go-librespot itself. Clear the
# inherited CMD so stray default args never reach ensemble.
ENTRYPOINT ["ensemble"]
CMD []
