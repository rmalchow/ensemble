<script>
  // One derived group (J arch §4): name, playback bar, members, settings text.
  import {
    nodeById,
    groupLabel,
    groupNameIsDerived,
    addTargets,
  } from "../lib/derive.js";
  import {
    renameGroup,
    follow,
    setGroupSettings,
    setVolume,
  } from "../lib/api.js";
  import EditableText from "./EditableText.svelte";
  import PlaybackBar from "./PlaybackBar.svelte";
  import MemberRow from "./MemberRow.svelte";

  let { group, snapshot, self, selected = false, onselect } = $props();

  // The server resolves the display label (D42): an explicit override or a
  // DERIVED label from member names. Derived labels render muted/italic; the
  // editor edits the OVERRIDE (empty when derived), and clearing it (commit
  // empty) resets back to the derived label.
  let derived = $derived(groupNameIsDerived(group));
  let derivedLabel = $derived(groupLabel(group));
  let override = $derived(derived ? "" : group.name || "");
  let members = $derived(
    group.members.map((id) => nodeById(snapshot, id)).filter(Boolean),
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
  $effect(() => {
    const v = Math.round(avgVol * 100);
    if (!gvDragging) gvPct = v;
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
      setVolume(m.id, nv).catch(() => gvLast.delete(m.id));
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
  }

  // alive nodes not already in this group → "Add node…" select.
  let candidates = $derived(addTargets(snapshot, group));

  // Adding node X folds it into this group: follow X onto this group's master.
  // The resulting snapshot over WS updates the card (no optimistic UI).
  async function onAdd(e) {
    const nodeId = e.target.value;
    e.target.value = "";
    if (!nodeId) return;
    try {
      await follow(nodeId, group.master);
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
  <div class="row between">
    <h3>
      <EditableText
        value={override}
        placeholder={derivedLabel}
        muted={derived}
        allowEmpty={true}
        onsave={(n) => renameGroup(group.id, n)}
      />
    </h3>
  </div>

  <PlaybackBar {group} />

  {#if members.length > 1}
    <div class="group-vol" title="group volume — scales every member proportionally">
      <span class="gv-label">Group volume</span>
      <input
        type="range"
        min="0"
        max="100"
        value={gvPct}
        oninput={onGvInput}
        onchange={onGvCommit}
        onpointerup={onGvCommit}
        aria-label="group volume"
      />
      <span class="gv-pct small">{gvPct}%</span>
    </div>
  {/if}

  <div class="members">
    {#each members as member (member.id)}
      <MemberRow {member} {group} {self} />
    {/each}
  </div>

  {#if candidates.length > 0}
    <div class="row">
      <select value="" onchange={onAdd} title="add an alive node to this group">
        <option value="">Add node…</option>
        {#each candidates as c (c.id)}
          <option value={c.id}>{c.name}</option>
        {/each}
      </select>
    </div>
  {/if}

  <details class="advanced">
    <summary>Advanced settings</summary>
    <div class="hint settings" title="group stream settings (applied on the master)">
      <label>
        codec
        <select value={codec} onchange={onCodec} aria-label="codec">
          <option value="pcm">pcm</option>
          <option value="opus">opus</option>
        </select>
      </label>
      <span class="dot">·</span>
      <label>
        transport
        <select value={transport} onchange={onTransport} aria-label="transport">
          <option value="udp">udp</option>
          <option value="tcp">tcp</option>
        </select>
      </label>
      <span class="dot">·</span>
      <label class="buf">
        buffer
        <input
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
        <span class="bufval">{bufMs} ms</span>
      </label>
      <span class="dot">·</span>
      <button class="btn" disabled title="acoustic auto-calibration (coming soon)">
        Calibrate…
      </button>
    </div>
  </details>
</div>

<style>
  /* consistent vertical rhythm between the card's stacked rows */
  .card {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  /* the whole card selects the group (→ Media shows its master's library) */
  .group-card {
    cursor: pointer;
  }
  .group-card.selected {
    border-color: var(--accent);
    box-shadow: 0 0 0 1px var(--accent);
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
  .group-vol .gv-pct {
    min-width: 2.8em;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  /* collapsed-by-default advanced section */
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

  /* compact, muted inline controls — keep the settings row small + unobtrusive */
  .settings {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 6px;
  }
  .settings label {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .settings .dot {
    opacity: 0.5;
  }
  .settings select {
    font: inherit;
    color: inherit;
    background: transparent;
    border: 1px solid rgba(255, 255, 255, 0.15);
    border-radius: 4px;
    padding: 0 2px;
  }
  .settings .buf input[type="range"] {
    width: 84px;
    vertical-align: middle;
  }
  .settings .bufval {
    min-width: 3.6em;
    font-variant-numeric: tabular-nums;
  }
</style>
