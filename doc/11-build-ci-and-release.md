# 11 — Building, cross-compiling, CI & release

> Part of the **Ensemble** spec set. Spine: [README.md](./README.md).

The headline consequence of the architecture (D12: pure-Go audio via **direct kernel
ioctl ALSA**, no `libasound`/cgo/dlopen; pure-Go decoders; no ffmpeg) is that **the
binary is fully static, pure-Go, and cross-compiles to every target with no C
toolchain.** Building is `go build`; cross-building is `GOOS/GOARCH go build`. That is
the whole story, and it is what makes a GitLab pipeline trivial.

---

## 1. Build prerequisites

| Need | Why | Notes |
|---|---|---|
| **Go ≥ 1.26** | compile the binary | the only mandatory build tool |
| **Node.js ≥ 20 + npm** | build the Svelte/Vite UI → `internal/web/dist`, embedded via `go:embed` | only to (re)build the frontend assets |
| **— nothing else —** | | **no** gcc / clang, **no** `libasound-dev`, **no** cross toolchain, **no** ffmpeg |

`CGO_ENABLED=0` is the default posture for every build. There is **no** build-time
capability switch (no `-tags alsa`): all sink backends are compiled in and selected at
runtime (D12).

> **`go:embed` ordering:** the Go packages embed `internal/web/dist`, so the **frontend
> must be built before any `go build`/`go test`/`go vet`** (an empty/missing `dist`
> fails compilation). The pipeline enforces this with stage ordering + artifacts.

---

## 2. Local build

```bash
# 1. frontend → internal/web/dist (embedded)
cd web && npm ci && npm run build && cd ..

# 2. host binary
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" \
  -o bin/ensemble ./cmd/ensemble
```

Helper scripts (mirror the mpvsync layout):
- `scripts/build.sh` — frontend + host binary (the two steps above).
- `scripts/build-arm64.sh` — `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 …` for Raspberry Pi.
- `scripts/dev.sh fmt|vet|test|tidy`.

`-s -w` strips symbols (smaller binary); `-trimpath` makes builds reproducible;
`-X main.version=…` stamps the version surfaced at `/bootstrap/info`/`GET /status`
(`softwareVersion`).

---

## 3. Cross-compilation matrix

Pure-Go ⇒ set `GOOS`/`GOARCH` and go — no sysroot, no cross-gcc:

| Target | GOOS | GOARCH | extra | Use |
|---|---|---|---|---|
| x86-64 server / dev | `linux` | `amd64` | | NUC, VM, dev box |
| **Raspberry Pi 3/4/5 (64-bit)** | `linux` | `arm64` | | **recommended Pi target** |
| Raspberry Pi (32-bit OS) | `linux` | `arm` | `GOARM=7` | older/lite images; arm64 preferred |

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" \
  -o bin/ensemble-linux-arm64 ./cmd/ensemble
```

The direct-ioctl ALSA sink uses only `golang.org/x/sys/unix` (syscalls + the kernel
`sound/asound.h` UAPI), which is available on `arm`, `arm64`, and `amd64` — so the
**precise** sink cross-compiles on all of them. (64-bit is recommended for Go
performance; 32-bit works.)

---

## 4. Runtime requirements (per node — NOT build-time)

The artifact is a **single static binary**; drop it on the box and run it. At runtime a
node needs only:

| For | Requires | Else |
|---|---|---|
| **Precise** sink (sub-ms) | Linux kernel + ALSA, a `/dev/snd/pcmC*D*p` the process can open (direct hardware, no sound server holding it) | falls back to coarse |
| **Coarse** sink | `aplay` **or** `pw-play`/`paplay` on `$PATH` | `Render=false` (control/media-only node) |
| Networking | UDP (clock+audio) + TCP (mTLS control) reachable on the LAN | — |

**Not required at runtime:** `libasound`, ffmpeg, a Go install, any shared library. The
binary is self-contained. (An I2S DAC HAT or HDMI audio is recommended hardware — see
README §8 — but that's deployment, not a software dependency.)

---

## 5. GitLab CI — pipeline

### 5.1 Runner prerequisites
- A GitLab Runner with the **Docker executor** (shared SaaS runners work).
- Network access to pull `golang` and `node` images and Go modules.
- No special privileges, no `services`, no toolchain images — pure-Go keeps it minimal.

### 5.2 `.gitlab-ci.yml`
Drop this at the repo root once `cmd/ensemble` + `web/` exist:

```yaml
stages: [frontend, test, build, release]

variables:
  CGO_ENABLED: "0"
  GOMODCACHE: "$CI_PROJECT_DIR/.gomodcache"
  GIT_DEPTH: "0"            # full history so `git describe` sees tags

# 1) Build the embedded UI; pass internal/web/dist to later stages as an artifact.
frontend:
  stage: frontend
  image: node:22-alpine
  cache:
    key: { files: [web/package-lock.json] }
    paths: [web/node_modules]
  script:
    - cd web
    - npm ci
    - npm run build          # emits ../internal/web/dist
  artifacts:
    paths: [internal/web/dist]
    expire_in: 1 day

# 2) Vet + unit tests + a cross-build smoke (no toolchain needed).
test:
  stage: test
  image: golang:1.26
  needs: [frontend]
  cache:
    key: gomod
    paths: [.gomodcache]
  script:
    - go vet ./...
    - go test ./...
    - GOOS=linux GOARCH=arm64 go build ./...   # proves the arm64 cross-build stays clean

# 3) Cross-compile every target in parallel → downloadable artifacts.
build:
  stage: build
  image: golang:1.26
  needs: [frontend, test]
  cache: { key: gomod, paths: [.gomodcache], policy: pull }
  parallel:
    matrix:
      - { GOOS: linux, GOARCH: amd64 }
      - { GOOS: linux, GOARCH: arm64 }
      - { GOOS: linux, GOARCH: arm, GOARM: "7" }
  script:
    - VER="${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}"
    - SUFFIX="${GOOS}-${GOARCH}${GOARM:+v$GOARM}"
    - go build -trimpath -ldflags "-s -w -X main.version=$VER"
        -o "bin/ensemble-$SUFFIX" ./cmd/ensemble
  artifacts:
    name: "ensemble-${CI_COMMIT_REF_SLUG}"
    paths: [bin/]
    expire_in: 4 weeks

# 4) On a tag, publish a GitLab Release that links the per-arch binaries.
release:
  stage: release
  image: registry.gitlab.com/gitlab-org/release-cli:latest
  needs: [build]
  rules:
    - if: $CI_COMMIT_TAG
  script: [ 'echo "Releasing $CI_COMMIT_TAG"' ]
  release:
    tag_name: "$CI_COMMIT_TAG"
    name: "Ensemble $CI_COMMIT_TAG"
    description: "Static pure-Go binaries for linux/amd64, arm64, armv7."
    assets:
      links:
        - name: "ensemble-linux-amd64"
          url: "$CI_PROJECT_URL/-/jobs/artifacts/$CI_COMMIT_TAG/raw/bin/ensemble-linux-amd64?job=build"
        - name: "ensemble-linux-arm64"
          url: "$CI_PROJECT_URL/-/jobs/artifacts/$CI_COMMIT_TAG/raw/bin/ensemble-linux-arm64?job=build"
        - name: "ensemble-linux-armv7"
          url: "$CI_PROJECT_URL/-/jobs/artifacts/$CI_COMMIT_TAG/raw/bin/ensemble-linux-armv7?job=build"
```

### 5.3 Downloading the binaries from GitLab
- **Every pipeline:** project → **Build → Pipelines** (or **Jobs**) → the `build` job →
  **Download artifacts** (the right sidebar), or **Browse** to grab one binary. Direct
  URL pattern: `…/-/jobs/artifacts/<ref>/download?job=build`.
- **Tagged releases:** project → **Deploy → Releases** → the release page lists the
  per-arch binaries as asset links (the `release` job above). This is the stable,
  user-facing download surface; cut a release by pushing a tag (`git tag v0.1.0 && git
  push --tags`).
- **API/CI consumers:** `GET /projects/:id/jobs/artifacts/:ref/raw/bin/ensemble-linux-arm64?job=build`.

### 5.4 Notes
- The `build` job artifacts are produced on **every** pipeline (4-week retention), so
  binaries are downloadable without tagging; the `release` job only adds the polished,
  permanent Releases page on tags.
- Caching: Go module cache keyed `gomod`; npm `node_modules` keyed on the lockfile.
- If you later add the **optional Opus** capability via a C lib, keep it behind runtime
  dlopen (purego) so the pipeline stays toolchain-free; do **not** reintroduce cgo (it
  would break this matrix and the single-binary/runtime-graceful model).
- Add a `lint` job (`golangci-lint`) and `( cd web && npm run check )` to `test` when the
  codebase exists.
