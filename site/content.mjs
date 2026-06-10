// All marketing copy for the ensemble site lives here. Edit this file and run
// `node build.mjs` (or `npm run build`) to regenerate site/dist/. No other file
// needs to change to update the words on the page.

const REPO = "https://gitlab.rand0m.me/share/ensemble";
const RELEASES = `${REPO}/-/releases`;
const GUIDE = `${REPO}/-/blob/main/docs/user/README.md`;

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
    { label: "How it works", href: "#how" },
    { label: "Source", href: REPO },
  ],

  hero: {
    eyebrow: "Open-source multiroom audio",
    // each line is rendered on its own row
    title: ["Every room.", "One sound.", "Zero fuss."],
    lede:
      "ensemble is self-hosted, synchronized audio for your whole home. Drop one small binary on each device — they discover each other and play in perfect sync. No cloud, no accounts, no config files.",
    primary: { label: "Get it", href: RELEASES },
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
          "Select a room to reveal its roster and the master’s library. Drop in a speaker, browse folders, and hit Play here — no menus to dig through.",
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

  cta: {
    title: "Bring your speakers together.",
    body:
      "Grab a build for your device, or read the user guide to see the whole app first.",
    primary: { label: "Download a release", href: RELEASES },
    secondary: { label: "Read the guide", href: GUIDE },
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
