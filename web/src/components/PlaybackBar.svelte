<script>
  // Playback row for a group (J arch §4, D39): a fixed-height band — a state pill,
  // the source-type icon + currently-playing track, position, and play/pause + stop
  // on the right. When active it tints (accent band) so the playing room is obvious
  // at a glance; idle keeps the SAME footprint, just emptier (no reflow either way).
  import { position } from "../lib/fmt.js";
  import { stop, pause, resume } from "../lib/api.js";

  let { group } = $props();

  let pb = $derived(group.playback || { state: "idle" });
  let playing = $derived(pb.state === "playing");
  let paused = $derived(pb.state === "paused");
  let active = $derived(playing || paused);

  // a friendly one-line name + a type glyph for the source uri.
  let track = $derived(friendlyTrack(pb.uri));
  let icon = $derived(iconFor(pb.uri));

  // now-playing metadata (the D57 source channel): title/artist/album/cover art.
  // Falls back to the URI-derived label when the source supplies none (line-in).
  let meta = $derived(pb.metadata || null);
  let title = $derived(meta && meta.title ? meta.title : track);
  let subtitle = $derived(
    meta ? [meta.artist, meta.album].filter(Boolean).join(" · ") : "",
  );
  function friendlyTrack(uri) {
    if (!uri) return "";
    if (uri.startsWith("spotify:")) return "Spotify";
    if (uri.startsWith("input:")) return "line-in";
    if (uri.startsWith("file:")) {
      const p = uri.slice(5);
      return p.split("/").pop() || p;
    }
    return uri; // http(s):// stream — show the url
  }
  function iconFor(uri) {
    if (!uri) return "";
    if (uri.startsWith("spotify:")) return "🟢";
    if (uri.startsWith("input:")) return "🎙";
    if (uri.startsWith("http")) return "📻";
    return "♪"; // file
  }

  async function ontoggle() {
    try {
      await (playing ? pause(group.master) : resume(group.master));
    } catch {
      // toast shown by api.js
    }
  }
  async function onstop() {
    try {
      await stop(group.master);
    } catch {
      // toast shown by api.js
    }
  }
</script>

<div class="playbar" class:active>
  <span class="state-pill" class:playing class:paused>
    {active ? (paused ? "paused" : "playing") : "idle"}
  </span>

  <div class="now">
    {#if active}
      {#if meta && meta.artUrl}
        <img class="art" src={meta.artUrl} alt="" />
      {:else if icon}
        <span class="icon">{icon}</span>
      {/if}
      <span class="meta" title={pb.uri}>
        <span class="track">{title}</span>
        {#if subtitle}<span class="sub small">{subtitle}</span>{/if}
      </span>
      <span class="pos small">{position(pb.positionSec)}</span>
    {/if}
  </div>

  <div class="controls">
    <button
      class="btn ctl"
      disabled={!active}
      onclick={ontoggle}
      title={playing ? "pause" : "resume"}
      aria-label={playing ? "pause" : "resume"}
    >
      {playing ? "⏸" : "▶"}
    </button>
    <button
      class="btn btn-danger ctl"
      disabled={!active}
      onclick={onstop}
      title="stop"
      aria-label="stop"
    >
      ■
    </button>
  </div>
</div>

<style>
  /* fixed-height band: identical footprint playing vs idle (no reflow). Tints
     when active so the playing room is obvious across the wall of cards. */
  .playbar {
    display: flex;
    align-items: center;
    gap: 10px;
    min-height: 44px;
    padding: 4px 10px;
    border: 1px solid transparent;
    border-radius: 8px;
  }
  .playbar.active {
    background: color-mix(in srgb, var(--accent) 14%, transparent);
    border-color: color-mix(in srgb, var(--accent) 38%, transparent);
  }

  /* state pill — solid + colored so the state reads at a glance */
  .state-pill {
    flex: 0 0 auto;
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--panel-2);
    color: var(--muted);
  }
  .state-pill.playing {
    background: var(--ok);
    color: #04210f;
  }
  .state-pill.paused {
    background: #f59e0b;
    color: #2a1900;
  }

  /* center: icon + track (ellipsised so the controls never move) + position */
  .now {
    flex: 1;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .now .icon {
    flex: 0 0 auto;
    font-size: 15px;
  }
  .now .art {
    flex: 0 0 auto;
    width: 34px;
    height: 34px;
    border-radius: 4px;
    object-fit: cover;
    background: var(--panel-2);
  }
  /* title + subtitle stack; ellipsised so controls never move */
  .now .meta {
    flex: 1 1 auto;
    min-width: 0;
    display: flex;
    flex-direction: column;
    justify-content: center;
    line-height: 1.2;
  }
  .now .track {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 14px;
  }
  .now .sub {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--muted);
  }
  .now .pos {
    flex: 0 0 auto;
    font-variant-numeric: tabular-nums;
    color: var(--muted);
  }

  /* right: two equal-width controls, identical footprint in every state */
  .controls {
    flex: 0 0 auto;
    display: flex;
    gap: 6px;
  }
  .controls .ctl {
    width: 42px;
    padding: 6px 0;
    text-align: center;
    line-height: 1;
    font-size: 15px;
  }
  .controls .ctl:disabled {
    opacity: 0.4;
    cursor: default;
  }

  /* Narrow viewports: stack into two rows — the ellipsised media info on top,
     then the state pill + transport buttons below. Wide layout is unchanged. */
  @media (max-width: 560px) {
    .playbar {
      flex-wrap: wrap;
      min-height: 0;
      row-gap: 8px;
    }
    .now {
      order: 1;
      flex-basis: 100%;
    }
    .state-pill {
      order: 2;
    }
    .controls {
      order: 3;
      margin-left: auto; /* buttons to the right, state pill to the left */
    }
  }
</style>
