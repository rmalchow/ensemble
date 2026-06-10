<script>
  // One derived group (J arch §4): headline, playback bar, members, settings.
  import { nodeById, nameOf, addTargets, playerZone } from "../lib/derive.js";
  import { assignToGroup, setGroupSettings, nodeSetVolume } from "../lib/api.js";
  import PlaybackBar from "./PlaybackBar.svelte";
  import MemberRow from "./MemberRow.svelte";
  import MediaBrowser from "./MediaBrowser.svelte";

  let { group, snapshot, self, selected = false, onselect } = $props();

  // An empty group (no players — an idle zone) serializes members as null; guard it.
  let members = $derived(
    (group.members || []).map((id) => nodeById(snapshot, id)).filter(Boolean),
  );
  // Headline = "[master]: [player1] + [player2]" (or "[master]: no players").
  // Names are NODE names (renamed on the Nodes page, not here). The master labels
  // the room; the players are its other members.
  let masterName = $derived(nameOf(snapshot, group.master));
  let playerNames = $derived(
    members.filter((m) => m.id !== group.master).map((m) => nameOf(snapshot, m.id)),
  );
  let settings = $derived(group.settings || {});
  let codec = $derived(settings.codec ?? "opus");
  let transport = $derived(settings.transport ?? "udp");
  // bufferMs tracks the slider thumb locally while dragging; a fresh snapshot
  // re-syncs it once released (mirrors VolumeSlider).
  let bufDragging = $state(false);
  let bufMs = $state(150);
  $effect(() => {
    const v = settings.bufferMs ?? 150;
    if (!bufDragging) bufMs = v;
  });

  // POST the FULL current trio with the one change applied, addressed to the
  // group's MASTER (the endpoint is master-only; the proxy makes it work from
  // anywhere). Live-applies via RECONFIG (D23).
  function applySettings(change) {
    const next = { codec, transport, bufferMs: bufMs, ...change };
    return setGroupSettings(group.master, next).catch(() => {});
  }

  function onCodec(e) {
    applySettings({ codec: e.target.value });
  }
  function onTransport(e) {
    applySettings({ transport: e.target.value });
  }

  // bufferMs slider: debounce ~250ms while dragging; trailing commit on release.
  let bufTimer = null;
  function onBufInput(e) {
    bufDragging = true;
    bufMs = Number(e.target.value);
    if (bufTimer) clearTimeout(bufTimer);
    bufTimer = setTimeout(() => {
      bufTimer = null;
      applySettings({ bufferMs: bufMs });
    }, 250);
  }
  function onBufCommit() {
    if (bufTimer) {
      clearTimeout(bufTimer);
      bufTimer = null;
    }
    applySettings({ bufferMs: bufMs });
    bufDragging = false;
  }

  // Group volume = the AVERAGE of member volumes; dragging it scales every
  // member PROPORTIONALLY (a muted member stays muted; the loudest reaches 1
  // first and then clamps). The baseline (average + each member's volume) is
  // captured at drag start so repeated emits during one drag stay proportional
  // and never compound off the snapshot echoes coming back over the WS.
  const clamp01 = (x) => (x < 0 ? 0 : x > 1 ? 1 : x);
  let avgVol = $derived(
    members.length
      ? members.reduce((s, m) => s + (m.volume || 0), 0) / members.length
      : 0,
  );
  let gvDragging = $state(false);
  let gvPct = $state(0);
  let gvBase = null; // {avg, vols: Map<id, 0..1>} captured at drag start
  let gvLast = new Map(); // last value sent per member (dedup within a drag)
  let gvTimer = null;
  // Settle hold: after a drag the per-member PATCHes round-trip and the average
  // re-converges over a frame or two. Holding the thumb across that window stops
  // it snapping back to the old average before the echoes land. Clamping means
  // the achieved average needn't exactly equal the target, so this is a short
  // time window rather than an exact-match guard (cf. VolumeSlider).
  let gvPending = $state(false);
  let gvSettleTimer = null;
  $effect(() => {
    const v = Math.round(avgVol * 100);
    if (gvDragging || gvPending) return;
    gvPct = v;
  });

  function applyGroupVolume() {
    if (!gvBase) return;
    const target = gvPct / 100;
    const factor = gvBase.avg > 0 ? target / gvBase.avg : null;
    for (const m of members) {
      const base = gvBase.vols.get(m.id) ?? 0;
      // factor === null ⇒ every member was muted: move them together to target.
      const nv = clamp01(factor != null ? base * factor : target);
      if (gvLast.get(m.id) === nv) continue;
      gvLast.set(m.id, nv);
      nodeSetVolume(m, nv).catch(() => gvLast.delete(m.id));
    }
  }
  function onGvInput(e) {
    if (!gvDragging) {
      gvBase = {
        avg: avgVol,
        vols: new Map(members.map((m) => [m.id, m.volume || 0])),
      };
      gvLast = new Map();
    }
    gvDragging = true;
    gvPct = Number(e.target.value);
    if (gvTimer) clearTimeout(gvTimer);
    gvTimer = setTimeout(applyGroupVolume, 150);
  }
  function onGvCommit() {
    if (gvTimer) {
      clearTimeout(gvTimer);
      gvTimer = null;
    }
    applyGroupVolume();
    gvDragging = false;
    gvBase = null;
    // hold the thumb briefly while the member PATCHes echo back into avgVol.
    gvPending = true;
    if (gvSettleTimer) clearTimeout(gvSettleTimer);
    gvSettleTimer = setTimeout(() => {
      gvSettleTimer = null;
      gvPending = false; // resume tracking the server-reported average
    }, 500);
  }

  // alive playback nodes not already in this room → the assign roster (chips).
  // Members of THIS room are NOT here — they show as MemberRows below.
  let candidates = $derived(addTargets(snapshot, group));

  // Clicking a chip points that node's player at this group's master. A gossiping
  // node uses follow (proxy-aware); a non-gossiping playback node is patched
  // master-side (assignToGroup routes by node.playbackNode). The WS snapshot then
  // moves it out of its old card and into this one.
  async function assignNode(node) {
    try {
      await assignToGroup(node, group.master);
    } catch {
      // toast shown by api.js
    }
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="card group-card"
  class:selected
  onclick={() => onselect && onselect(group.master)}
>
  <h3 class="headline" title="{masterName}: {playerNames.join(' + ') || 'no players'}">
    <span class="master">{masterName}</span><span class="colon">:</span>
    {#if playerNames.length}
      <span class="players">{playerNames.join(" + ")}</span>
    {:else}
      <em class="noplayers">no players</em>
    {/if}
  </h3>

  <PlaybackBar {group} expanded={selected} />

  <div class="group-vol" title="group volume — scales every member proportionally">
    <span class="gv-label">Group volume</span>
    <input
      type="range"
      min="0"
      max="100"
      value={gvPct}
      disabled={members.length === 0}
      oninput={onGvInput}
      onchange={onGvCommit}
      onpointerup={onGvCommit}
      aria-label="group volume"
    />
    <span class="gv-pct small">{gvPct}%</span>
  </div>

  <div class="members">
    {#each members as member (member.id)}
      <MemberRow {member} {group} {self} />
    {/each}
  </div>

  <!-- Operational controls live only on the SELECTED room (Rams): assigning players,
       choosing media, and stream settings are focused actions, not glanceable state,
       so showing them on every card would be N-fold noise. The outline marks it. -->
  {#if selected}
    {#if candidates.length > 0}
      <div class="assign-roster" role="group" aria-label="add a player to this room">
        {#each candidates as c (c.id)}
          {@const zone = playerZone(snapshot, c)}
          <button
            class="assign-chip"
            onclick={() => assignNode(c)}
            title={zone === "idle"
              ? `add ${c.name} (idle) to this room`
              : `move ${c.name} here (currently in ${zone})`}
          >
            <span class="ac-name">{c.name}</span>
            <span class="ac-zone" class:idle={zone === "idle"}>{zone}</span>
          </button>
        {/each}
      </div>
    {/if}

    <MediaBrowser {snapshot} nodeId={group.master} />

    <details class="advanced">
      <summary>Advanced settings</summary>
      <div class="settings-grid" title="group stream settings (applied on the master)">
        <span class="lbl">Codec</span>
        <div class="ctl">
          <select value={codec} onchange={onCodec} aria-label="codec">
            <option value="pcm">pcm</option>
            <option value="opus">opus</option>
          </select>
        </div>

        <span class="lbl">Transport</span>
        <div class="ctl">
          <select value={transport} onchange={onTransport} aria-label="transport">
            <option value="udp">udp</option>
            <option value="tcp">tcp</option>
          </select>
        </div>

        <span class="lbl">Buffer</span>
        <div class="ctl">
          <input
            class="grow"
            type="range"
            min="50"
            max="500"
            step="25"
            value={bufMs}
            oninput={onBufInput}
            onchange={onBufCommit}
            onpointerup={onBufCommit}
            aria-label="buffer ms"
          />
          <span class="val">{bufMs} ms</span>
        </div>
      </div>
    </details>
  {/if}
</div>

<style>
  /* consistent vertical rhythm between the card's stacked rows */
  .card {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  /* the whole card selects the group (→ reveals its roster / media / settings) */
  .group-card {
    cursor: pointer;
  }
  .group-card.selected {
    /* feathered selection: a translucent accent border + a soft, slightly-spread
       glow instead of a hard 1px ring — hides the corner aliasing on the curve. */
    border-color: color-mix(in srgb, var(--accent) 70%, transparent);
    box-shadow:
      0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent),
      0 0 22px -6px color-mix(in srgb, var(--accent) 45%, transparent);
  }

  /* headline: "[master]: [players]" — the room's self-describing label. Node
     names (renamed on the Nodes page); not editable here. */
  .headline {
    margin: 0;
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0 6px;
    min-width: 0;
  }
  .headline .master {
    font-weight: 600;
  }
  .headline .colon {
    margin-left: -4px;
    color: var(--muted);
  }
  .headline .players {
    font-weight: 400;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .headline .noplayers {
    color: var(--muted);
  }

  /* assign roster: one muted chip per player NOT in this room (members show as
     rows above). Click moves a player here; the sub-label says where it is now. */
  .assign-roster {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .assign-chip {
    display: inline-flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 1px;
    padding: 3px 9px;
    background: var(--panel-2);
    border: 1px solid var(--border);
    border-radius: 8px;
    color: var(--fg);
    cursor: pointer;
    line-height: 1.2;
  }
  .assign-chip:hover {
    border-color: var(--accent);
    color: var(--accent);
  }
  .assign-chip .ac-name {
    font-size: 13px;
  }
  .assign-chip .ac-zone {
    font-size: 10px;
    color: var(--muted);
  }
  .assign-chip .ac-zone.idle {
    font-style: italic;
    opacity: 0.7;
  }

  /* group volume — a touch more prominent than a member row, full width */
  .group-vol {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .group-vol .gv-label {
    color: var(--muted);
    min-width: 7.5em;
  }
  .group-vol input[type="range"] {
    flex: 1;
  }
  /* disabled (a room with no players): dim the whole row */
  .group-vol:has(input:disabled) {
    opacity: 0.4;
  }
  .group-vol input[type="range"]:disabled {
    cursor: not-allowed;
  }
  .group-vol .gv-pct {
    min-width: 2.8em;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  /* advanced settings: foldable, collapsed by default, inside the selected card */
  .advanced > summary {
    cursor: pointer;
    color: var(--muted);
    font-size: 12px;
    list-style: revert;
    user-select: none;
  }
  .advanced[open] > summary {
    margin-bottom: 6px;
  }

  /* roomy two-column settings: label | control, one setting per row */
  .settings-grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    align-items: center;
    gap: 10px 14px;
    padding: 2px 2px 4px;
  }
  .settings-grid .lbl {
    color: var(--muted);
    font-size: 12px;
  }
  .settings-grid .ctl {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .settings-grid select {
    font: inherit;
    color: inherit;
    background: transparent;
    border: 1px solid rgba(255, 255, 255, 0.15);
    border-radius: 4px;
    padding: 2px 4px;
    max-width: 14em;
  }
  .settings-grid .ctl .grow {
    flex: 1;
    min-width: 0;
  }
  .settings-grid .val {
    min-width: 3.6em;
    text-align: right;
    color: var(--muted);
    font-variant-numeric: tabular-nums;
  }
</style>
