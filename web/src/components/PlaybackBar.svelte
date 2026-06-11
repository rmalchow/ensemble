<script>
  // Playback row for a group (J arch §4, D39): a fixed-height band — a state pill,
  // the source-type icon + currently-playing track, position, and play/pause + stop
  // on the right. When active it tints (accent band) so the playing room is obvious
  // at a glance; idle keeps the SAME footprint, just emptier (no reflow either way).
  import { position } from "../lib/fmt.js";
  import { createPlayClock, reconcile, sample } from "../lib/playclock.js";
  import {
    stop,
    pause,
    resume,
    next,
    queueRemove,
    queuePlay,
    getQueue,
  } from "../lib/api.js";

  let { group, expanded = false } = $props();

  let pb = $derived(group.playback || { state: "idle" });
  let playing = $derived(pb.state === "playing");
  let paused = $derived(pb.state === "paused");
  let active = $derived(playing || paused);

  // The UPCOMING queue. Only its length + a change marker (queueRev) ride the
  // gossiped playback record — a big queue would blow memberlist's UDP packet and
  // stall propagation — so the items themselves are pulled live from the master,
  // proxied, whenever the marker (or now-playing track) moves. `next` is enabled
  // only while actually playing AND something is queued behind it.
  let queueLen = $derived(pb.queueLen || 0);
  let canNext = $derived(playing && queueLen > 0);

  // fetched upcoming items (only while the card is expanded and there's a queue).
  let queue = $state([]);
  let lastQueueKey = ""; // guards refetch: only re-pull when the queue actually moved
  $effect(() => {
    // Reads below register as effect dependencies; position-only heartbeat ticks
    // (not in the key) don't re-pull.
    const key = `${group.master}|${pb.queueRev || 0}|${queueLen}|${pb.uri}|${expanded}|${active}`;
    if (key === lastQueueKey) return;
    lastQueueKey = key;
    if (!expanded || !active || queueLen <= 0) {
      queue = [];
      return;
    }
    const master = group.master;
    getQueue(master)
      .then((list) => {
        if (lastQueueKey === key) queue = Array.isArray(list) ? list : [];
      })
      .catch(() => {
        if (lastQueueKey === key) queue = [];
      });
  });

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
  // track length for the transport bar (seconds); 0 = unknown (live/line-in →
  // the bar shows elapsed time only, no scrub track). Files report it from the
  // decoder, Spotify from go-librespot.
  let durationSec = $derived(meta && meta.durationSec ? meta.durationSec : 0);

  // Smooth position: the server reports positionSec only ~every 5 s (group
  // heartbeat) and a little stale, but position is realtime between events — so a
  // local clock (lib/playclock) free-runs at 1x, gently slews toward each heartbeat,
  // and hard-snaps only on a real discontinuity (track change, resume, seek). The
  // clock object is PLAIN (non-reactive) so the ticker writing displayPos can't feed
  // back into the reconcile effect; displayPos ($state) is the only reactive output.
  let displayPos = $state(0);
  const clock = createPlayClock();
  $effect(() => {
    reconcile(clock, {
      positionSec: pb.positionSec || 0, // tracked deps: position, uri, state
      uri: pb.uri || "",
      playing,
      nowMs: performance.now(),
    });
    displayPos = sample(clock, performance.now(), durationSec);
  });
  $effect(() => {
    if (!playing) return; // frozen (paused/idle); the reconcile effect set displayPos
    const id = setInterval(() => {
      displayPos = sample(clock, performance.now(), durationSec);
    }, 250);
    return () => clearInterval(id);
  });
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
  async function onnext() {
    try {
      await next(group.master);
    } catch {
      // toast shown by api.js
    }
  }
  async function onremove(index, uri) {
    try {
      await queueRemove(group.master, index, uri);
    } catch {
      // toast shown by api.js
    }
  }
  // clicking a queue item plays it now: the current track is dropped, the clicked
  // track jumps to the front and starts playing (gapless front-switch).
  async function onplay(index, uri) {
    try {
      await queuePlay(group.master, index, uri);
    } catch {
      // toast shown by api.js
    }
  }

  // a queue entry's display label: tag title (+ artist) when present, else the
  // URI-derived (filename) fallback — same as the now-playing bar.
  function queueTitle(item) {
    if (item.metadata && item.metadata.title) return item.metadata.title;
    return friendlyTrack(item.uri);
  }
  function queueSub(item) {
    return item.metadata ? item.metadata.artist || "" : "";
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
      class="btn ctl"
      disabled={!canNext}
      onclick={onnext}
      title="next"
      aria-label="next"
    >
      ⏭
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

{#if active}
  <!-- transport bar: elapsed · scrubber · total. Display-only for now (disabled);
       seeking lands in a later pass. Sources with no known length (line-in, live
       streams) show elapsed only and a disabled empty track. -->
  <div class="transport">
    <span class="t-time small">{position(displayPos)}</span>
    <input
      class="t-bar"
      type="range"
      min="0"
      max={durationSec || 1}
      value={durationSec ? Math.min(displayPos, durationSec) : 0}
      step="0.1"
      disabled
      title="seeking coming soon"
      aria-label="playback position"
    />
    <span class="t-time small">{durationSec ? position(durationSec) : "live"}</span>
  </div>
{/if}

{#if active && queueLen > 0 && !expanded}
  <!-- collapsed: just the count on a non-selected card (queue list is noise there).
       Uses the gossiped length — no fetch needed when the card isn't selected. -->
  <div class="queue-collapsed small">{queueLen} in queue</div>
{:else if active && queueLen > 0}
  <!-- expanded queue: the upcoming tracks under the now-playing bar, scrolling
       internally so ~10 are visible without growing the card unbounded. -->
  <div class="queue">
    <div class="queue-head small">Up next · {queueLen}</div>
    <ul class="queue-list">
      {#each queue as item, i (item.uri + ":" + i)}
        <li class="queue-item">
          <button
            class="q-play"
            onclick={() => onplay(i, item.uri)}
            title="play now"
            aria-label="play {queueTitle(item)} now"
          >
            <span class="q-idx small">{i + 1}</span>
            <span class="q-meta">
              <span class="q-title">{queueTitle(item)}</span>
              {#if queueSub(item)}<span class="q-sub small">{queueSub(item)}</span>{/if}
            </span>
          </button>
          <span class="spacer"></span>
          <button
            class="btn q-rm"
            onclick={() => onremove(i, item.uri)}
            title="remove from queue"
            aria-label="remove from queue"
          >−</button>
        </li>
      {/each}
    </ul>
  </div>
{/if}

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
  /* transport scrubber row under the bar: elapsed · slider · total. */
  .transport {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 2px 2px;
  }
  .t-time {
    flex: 0 0 auto;
    min-width: 3em;
    font-variant-numeric: tabular-nums;
    color: var(--muted);
    text-align: center;
  }
  .t-bar {
    flex: 1 1 auto;
    min-width: 0;
    height: 4px;
    accent-color: var(--accent);
    cursor: default; /* display-only for now */
  }
  .t-bar:disabled {
    opacity: 0.7;
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

  /* collapsed queue summary (non-selected card): just the count, unobtrusive. */
  .queue-collapsed {
    color: var(--muted);
    padding: 0 2px 2px;
  }

  /* expanded queue under the bar: a bordered panel that scrolls internally so a
     long queue never grows the card; ~10 rows visible at a glance. */
  .queue {
    border: 1px solid color-mix(in srgb, var(--accent) 24%, var(--border));
    border-radius: 8px;
    background: var(--panel-2);
    padding: 6px 8px;
  }
  .queue-head {
    color: var(--muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-weight: 700;
    padding: 2px 2px 6px;
  }
  .queue-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    /* ~10 rows then scroll */
    max-height: 280px;
    overflow-y: auto;
  }
  .queue-item {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 3px 2px;
    border-radius: 6px;
  }
  .queue-item:hover {
    background: color-mix(in srgb, var(--accent) 10%, transparent);
  }
  /* the clickable body (index + media info) — plays that track now. A bare
     button reset so it reads as a row, with the gap/alignment the row used to own. */
  .q-play {
    flex: 1 1 auto;
    min-width: 0;
    display: flex;
    align-items: flex-start;
    gap: 8px;
    padding: 0;
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .q-play:hover .q-title {
    color: var(--accent);
  }
  .q-idx {
    flex: 0 0 auto;
    width: 1.8em;
    text-align: right;
    color: var(--muted);
    font-variant-numeric: tabular-nums;
    line-height: 1.2;
  }
  .q-meta {
    flex: 1 1 auto;
    min-width: 0;
    display: flex;
    flex-direction: column;
    line-height: 1.2;
  }
  .q-title {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
  }
  .q-sub {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--muted);
  }
  .q-rm {
    flex: 0 0 auto;
    width: 28px;
    padding: 2px 0;
    text-align: center;
    line-height: 1;
    font-size: 15px;
  }
  .q-rm:hover {
    border-color: var(--danger);
    color: var(--danger);
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
