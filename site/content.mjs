// All marketing copy for the ensemble site lives here. Edit this file and run
// `node build.mjs` (or `npm run build`) to regenerate site/dist/. No other file
// needs to change to update the words on the page.

const REPO = "https://gitlab.rand0m.me/share/ensemble";
const RELEASES = `${REPO}/-/releases`;
const DOC = (f) => `${REPO}/-/blob/main/docs/user/${f}`;
const GUIDE = DOC("README.md");

export const content = {
  meta: {
    title: "ensemble — synchronized multiroom audio",
    description:
      "Open-source, self-hosted multiroom audio. Drop one small binary on each device — they find each other and play in perfect sync. No cloud, no accounts, no config.",
    url: "",
    themeColor: "#0a0c10",
  },

  brand: { name: "ensemble" },

  nav: [
    { label: "Why", href: "#why" },
    { label: "Screens", href: "#screens" },
    { label: "How", href: "#how" },
    { label: "Quickstart", href: "#quickstart" },
    { label: "Tech", href: "#tech" },
    { label: "Praise", href: "#praise" },
    { label: "Source", href: REPO },
  ],

  hero: {
    eyebrow: "Open-source multiroom audio",
    // each line is rendered on its own row
    title: ["Every room.", "One sound.", "Zero fuss."],
    lede:
      "ensemble is self-hosted, synchronized audio for your whole home. Drop one small binary on each device — they discover each other and play in perfect sync. No cloud, no accounts, no config files.",
    primary: { label: "Get it", href: "download.html" },
    secondary: { label: "View source", href: REPO },
    snippet: { cmd: "./ensemble", caption: "That’s the whole setup." },
    shot: {
      src: "assets/img/overview.png",
      alt: "The ensemble web app on a phone: a playing room group with cover art, now-playing track, group volume and per-speaker volumes.",
    },
  },

  why: {
    eyebrow: "Why ensemble",
    title: "Built to disappear into your home.",
    features: [
      {
        n: "01",
        tag: "simple",
        title: "No moving parts",
        body:
          "One static binary. No config files, no database, no message broker, no account. Copy it to each device, run it, done — multiroom audio without the weekend project.",
      },
      {
        n: "02",
        tag: "automatic",
        title: "Sets itself up",
        body:
          "Nodes find each other over your LAN automatically. Ports, audio devices and host capabilities are detected per machine, with sensible defaults everywhere. First run just works; tune later if you want.",
      },
      {
        n: "03",
        tag: "open",
        title: "Yours to keep",
        body:
          "Free and open source, end to end. Pure-Go builds for Linux on amd64, arm64 and the Pi Zero. Optional Spotify Connect built in. No telemetry, no lock-in — your audio stays on your network.",
      },
      {
        n: "04",
        tag: "built-in ui",
        title: "A UI in every node",
        body:
          "Every node serves the same web app and proxies to the rest. Group rooms, set volumes, browse your library and see what’s playing — from any phone or browser, with nothing to install.",
      },
    ],
  },

  screens: {
    eyebrow: "A look around",
    title: "One app, the whole house.",
    items: [
      {
        src: "assets/img/overview.png",
        alt: "Rooms overview with a playing group, now-playing bar and volumes.",
        kicker: "Now playing",
        title: "See and steer every room",
        body:
          "Group devices into rooms, watch the current track with cover art and position, and balance volume per room and per speaker.",
      },
      {
        src: "assets/img/room-expanded.png",
        alt: "A selected room revealing the add-players roster and the media browser.",
        kicker: "In-line control",
        title: "Add players, pick music",
        body:
          "Select a room to reveal its roster and the master’s library. Drop in a speaker, browse folders, and press play — no menus to dig through.",
      },
      {
        src: "assets/img/queue.png",
        alt: "A playing room with a Next button above an ‘Up next’ list of six queued tracks, each removable.",
        kicker: "Shared queue",
        title: "Everyone adds what’s next",
        body:
          "Queue up a track — or a whole folder — and it plays gaplessly, back to back. The queue belongs to the room, not to one phone: anyone in the house can add what they want to hear, skip ahead, or pull a track. Titles come straight from your files’ tags.",
      },
      {
        src: "assets/img/spotify-endpoints.png",
        alt: "The Spotify endpoints editor with a default device and a custom preset of players.",
        kicker: "Spotify Connect",
        title: "Multi-room Spotify",
        body:
          "Expose named Connect devices that each map to a set of speakers. Pick one in the Spotify app and ensemble forms that group and plays — built in via go-librespot.",
      },
      {
        src: "assets/img/nodes.png",
        alt: "The Nodes page with per-device sections for features and settings.",
        kicker: "Per node",
        title: "Tune every device",
        body:
          "Volume, fine hardware-delay alignment, output device and feature toggles — for each node on your network, from the same page.",
      },
    ],
  },

  how: {
    eyebrow: "How it works",
    title: "Three steps, then it’s just music.",
    steps: [
      {
        n: "1",
        title: "Run a node",
        body:
          "Start ensemble on each device with a speaker — a Raspberry Pi, an old laptop, your NAS. One command, no flags required.",
      },
      {
        n: "2",
        title: "They find each other",
        body:
          "Nodes form a cluster over mDNS automatically. No central server, no broker, no setup — peers appear within seconds.",
      },
      {
        n: "3",
        title: "Group and play",
        body:
          "Open the built-in UI, group devices into rooms, and play. A shared clock keeps every speaker aligned to the millisecond.",
      },
    ],
  },

  quickstart: {
    eyebrow: "Quickstart",
    title: "From download to whole-home sound in minutes.",
    intro:
      "You've set up software like this before — here's the shape of it. Each step links to the guide for the full version.",
    steps: [
      {
        n: "00",
        tag: "one-liner",
        title: "For the lazy people",
        body:
          "One command does the lot: it detects your OS and CPU, downloads the right ensemble build, then asks whether you want Spotify Connect (go-librespot) and a boot-time systemd service — and sets them up. Installs into /usr/local/lib/ensemble.",
        code: "curl -fsSL https://ensemble.rand0m.me/get.sh | sudo bash",
        doc: { label: "What the script does, step by step", href: DOC("running.md") },
      },
      {
        n: "01",
        tag: "download",
        title: "Get a build",
        body:
          "One static, pure-Go binary per device — no runtime, no dependencies. Prebuilt for Linux amd64, arm64, and armv6 (Pi Zero); attached to every tagged release. Or run the master in Docker, with Spotify Connect (go-librespot) baked in.",
        code:
          "# native — grab the binary for each device's architecture, then:\nchmod +x ./ensemble\n\n# docker — master-only image, Spotify Connect included:\ndocker pull harbor.rand0m.me/public/ensemble:latest",
        action: { label: "Download a release", href: RELEASES },
        doc: { label: "Install scenarios (NAS, Pi, desktop, ESP32)", href: GUIDE },
      },
      {
        n: "02",
        tag: "pair",
        title: "Run it — nodes find each other",
        body:
          "Start the binary on each device with a speaker. On first run a node mints a permanent ID, advertises itself over mDNS, and gossips its state — peers appear within seconds, with no central server or broker. Every node serves the same web app and proxies to the rest, so open any one of them and group rooms by telling one node to follow another. A shared clock keeps the group aligned to the millisecond.",
        code:
          "./ensemble                      # on each device\n\n# then open the built-in UI on any node:\n#   http://<any-node-ip>:8080",
        doc: { label: "Pairing, grouping & the UI", href: DOC("ui-reference.md") },
      },
      {
        n: "03",
        tag: "flags",
        title: "The flags you'll actually use",
        body:
          "Every flag has an ENSEMBLE_* environment equivalent; all are optional, with sensible defaults. Ports left at their default bind-or-increment (run several nodes on one box); a port you set explicitly is pinned (binds exactly or exits).",
        params: [
          { flag: "--name <name>", def: "node id", what: "display name + Spotify device name (first start only)" },
          { flag: "--role <role>", def: "both", what: "master | playback | master,playback" },
          { flag: "--media <dir>", def: "<data>/media", what: "library directory, browsed recursively" },
          { flag: "--data <dir>", def: "./data", what: "node.json, cluster state, Spotify creds" },
          { flag: "--http-port <n>", def: "8080", what: "UI + REST API + WebSocket + node proxy" },
          { flag: "ENSEMBLE_OUTPUT", def: "auto", what: "alsa · exec · null · file:<path>" },
        ],
        doc: { label: "Full configuration reference", href: DOC("config-reference.md") },
      },
      {
        n: "04",
        tag: "start",
        title: "Keep it running",
        body:
          "However you supervise processes — from a throwaway foreground run to a boot-time service:",
        methods: [
          { label: "foreground", cmd: "./ensemble" },
          { label: "detached", cmd: "nohup ./ensemble --name kitchen >ensemble.log 2>&1 &" },
          { label: "systemd", cmd: "sudo systemctl enable --now ensemble" },
          { label: "docker", cmd: "docker run -d --network host -v /srv/music:/media:ro … --name living-room" },
          { label: "compose", cmd: "docker compose up -d" },
        ],
        doc: { label: "Startup methods in detail (units + commands)", href: DOC("running.md") },
      },
    ],
  },

  tech: {
    eyebrow: "Under the hood",
    title: "Playing in sync is hard. ensemble does the hard part.",
    intro:
      "Two speakers playing the same instant over flaky Wi-Fi means fighting both the network and physics. Four problems, four fixes:",
    items: [
      {
        tag: "network jitter",
        problem:
          "Wi-Fi delivers packets in bursts and at uneven intervals — played naively, audio stutters and rooms drift apart.",
        solution:
          "Every frame is stamped with a presentation time and played at that exact deadline against a shared clock. A small per-group playout buffer absorbs the jitter, so output stays smooth and every room hits the same instant.",
      },
      {
        tag: "packet loss",
        problem:
          "On a busy network UDP datagrams simply vanish, and a dropped frame is an audible click or gap.",
        solution:
          "Audio is Opus-coded so each frame fits one small packet (no fragile IP fragmentation), and the master sends forward-error-correction parity alongside it, so most lost frames are rebuilt with no retransmit. A TCP transport is one toggle away when you'd rather trade a little latency for certainty.",
      },
      {
        tag: "system clock drift",
        problem:
          "No two devices agree on what time it is — their clocks start at different offsets and tick at slightly different rates.",
        solution:
          "One node is the time reference. Each player continuously measures its offset and round-trip to that master and translates “play at T” into its own local time, so the same frame lands at the same real-world instant on every speaker.",
      },
      {
        tag: "DAC clock drift",
        problem:
          "Even with perfect timing, no two sound cards sample at exactly 48 kHz — a few parts-per-million apart, rooms slide out of phase over a long track.",
        solution:
          "A continuous rate servo watches how fast each DAC actually drains its buffer versus the master timeline and resamples by a micro-correction to match. Rooms stay phase-locked for hours, not just for the first minute.",
      },
    ],
  },

  // Tongue-in-cheek "testimonials". Every quote is invented; the disclaimer makes
  // that unmistakable. Portraits are from Wikimedia Commons, vendored locally.
  testimonials: {
    eyebrow: "Rave reviews",
    title: "The greats can’t stop talking about it.",
    note: "Every quote below is entirely fictional — they never said any of this, and several of them predate electricity. Portraits via Wikimedia Commons.",
    items: [
      {
        name: "Ludwig van Beethoven",
        role: "Composer · 1770–1827",
        quote:
          "I could not hear a single one of my rooms — yet they played as one. Magnificent. I was, however, not consulted.",
        img: "assets/img/testimonials/beethoven.jpg",
      },
      {
        name: "Wolfgang Amadeus Mozart",
        role: "Composer · 1756–1791",
        quote:
          "Setup took less time than a cadenza. One file, no fuss — even a prodigy could manage it.",
        img: "assets/img/testimonials/mozart.jpg",
      },
      {
        name: "Johann Sebastian Bach",
        role: "Composer · 1685–1750",
        quote:
          "Every voice entering at precisely the right instant, in every room at once. Counterpoint, but for speakers.",
        img: "assets/img/testimonials/bach.jpg",
      },
      {
        name: "Miles Davis",
        role: "Jazz · 1926–1991",
        quote:
          "It’s not the notes you sync, it’s the ones you don’t. ensemble gets the silence right in all five rooms.",
        img: "assets/img/testimonials/miles-davis.png",
      },
      {
        name: "Ella Fitzgerald",
        role: "Jazz vocalist · 1917–1996",
        quote:
          "No cloud, no accounts, no scat about subscriptions. Put it on and the whole house swings.",
        img: "assets/img/testimonials/ella-fitzgerald.jpg",
      },
      {
        name: "Freddie Mercury",
        role: "Rock · 1946–1991",
        quote:
          "I want it all, I want it all — and I want it in every room. Darling, it delivered.",
        img: "assets/img/testimonials/freddie-mercury.jpg",
      },
      {
        name: "Jimi Hendrix",
        role: "Rock guitarist · 1942–1970",
        quote:
          "’Scuse me while I sync the sky. Kitchen, hallway, garage — all phase-locked. Far out.",
        img: "assets/img/testimonials/jimi-hendrix.jpg",
      },
      {
        name: "Prince",
        role: "Pop · 1958–2016",
        quote:
          "Dearly beloved, we are gathered to play one song in every room. No latency, no cloud. Just the music.",
        img: "assets/img/testimonials/prince.png",
      },
    ],
  },

  cta: {
    title: "Bring your speakers together.",
    body:
      "Grab a build for your device, or read the user guide to see the whole app first.",
    primary: { label: "Get it", href: "download.html" },
    secondary: { label: "Read the guide", href: GUIDE },
  },

  // The separate download page (download.html). Each option's `file` is resolved
  // at build time: build.mjs hashes the staged binary (src/assets/downloads/) and
  // fills in its SHA-256 + size. `docker` options carry a pull command instead.
  download: {
    eyebrow: "Download",
    title: "Get ensemble for your hardware.",
    intro:
      "One small, static binary per device — pure Go, no runtime, no dependencies. Each archive is the build attached to the matching tagged release: verify its SHA-256, unpack it, and run ./ensemble. Prefer containers? Pull the master image with Spotify Connect built in.",
    options: [
      {
        name: "Raspberry Pi — 64-bit",
        rec: "Recommended: Raspberry Pi OS Lite (64-bit). Also any other arm64 Linux.",
        arch: "linux · arm64",
        logos: ["raspberrypi"],
        file: "assets/downloads/ensemble-linux-arm64.tar.gz",
      },
      {
        name: "Raspberry Pi — 32-bit",
        rec: "Recommended: Raspberry Pi OS Lite (32-bit). Includes the Pi Zero / Pi 1 (armv6). Hard-float — uses /lib/ld-linux-armhf.so.3.",
        arch: "linux · armv6 · hard-float (armhf)",
        logos: ["raspberrypi"],
        file: "assets/downloads/ensemble-linux-armv6hf.tar.gz",
      },
      {
        name: "32-bit ARM — soft-float",
        rec: "For armel userlands whose loader is /lib/ld-linux.so.3, not Raspberry Pi OS. Same ARMv6 code, soft-float ABI.",
        arch: "linux · armv6 · soft-float (armel)",
        logos: ["raspberrypi"],
        file: "assets/downloads/ensemble-linux-armv6.tar.gz",
      },
      {
        name: "PC or server — x86-64",
        rec: "Any modern Linux distribution on a 64-bit Intel/AMD machine.",
        arch: "linux · amd64",
        logos: ["fedora", "ubuntu", "debian", "arch", "manjaro"],
        file: "assets/downloads/ensemble-linux-amd64.tar.gz",
      },
      {
        name: "Docker",
        rec: "A master node with Spotify Connect (go-librespot) built in. Multi-arch: amd64 · arm64.",
        arch: "container image",
        logos: ["docker"],
        docker: "docker pull harbor.rand0m.me/public/ensemble:latest",
      },
    ],
    links: [
      {
        desc: "Looking for an older version, the changelog, or release notes?",
        label: "Other versions",
        href: RELEASES,
      },
      {
        desc: "New here and need help getting a node running?",
        label: "How to run it",
        href: DOC("running.md"),
      },
    ],
  },

  footer: {
    note: "Pure Go + Svelte. Runs on a Raspberry Pi. No cloud, no telemetry.",
    links: [
      { label: "Source", href: REPO },
      { label: "Releases", href: RELEASES },
      { label: "User guide", href: GUIDE },
    ],
  },
};
