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
  import Calibrate from "./Calibrate.svelte";

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
    // hold the thumb briefly while the member PATCHes echo back into avgVol.
    gvPending = true;
    if (gvSettleTimer) clearTimeout(gvSettleTimer);
    gvSettleTimer = setTimeout(() => {
      gvSettleTimer = null;
      gvPending = false; // resume tracking the server-reported average
    }, 500);
  }

  // group members that expose a microphone/line-in — the calibration recorder
  // must be a clock-synced group member (docs/calibrate.md §6).
  let micNodes = $derived(
    members.filter((m) => (m.capabilities?.sources ?? []).includes("input")),
  );
  let micNodeId = $state("");
  $effect(() => {
    // default to the first capable member; keep the selection valid as members
    // come and go.
    const ids = micNodes.map((m) => m.id);
    if (!ids.includes(micNodeId)) micNodeId = ids[0] || "";
  });

  // capture devices enumerated on the chosen mic node (D48). "" = system default.
  let micDevices = $derived(
    members.find((m) => m.id === micNodeId)?.inputDevices ?? [],
  );
  let micDeviceId = $state("");
  $effect(() => {
    const ids = micDevices.map((d) => d.id);
    if (!ids.includes(micDeviceId)) micDeviceId = micDevices.length ? micDevices[0].id : "";
  });

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

      <span class="lbl">Calibration</span>
      <div class="ctl stack">
        <select
          bind:value={micNodeId}
          aria-label="calibration microphone node"
          title="node whose microphone records the calibration sweep"
          disabled={micNodes.length === 0}
        >
          {#if micNodes.length === 0}
            <option value="">no microphone in group</option>
          {:else}
            {#each micNodes as m (m.id)}
              <option value={m.id}>🎤 {m.name}</option>
            {/each}
          {/if}
        </select>
        <select
          bind:value={micDeviceId}
          aria-label="calibration input device"
          title="which capture device on the mic node to record from"
          disabled={!micNodeId || micDevices.length === 0}
        >
          {#if micDevices.length === 0}
            <option value="">system default</option>
          {:else}
            {#each micDevices as d (d.id)}
              <option value={d.id}>{d.desc}</option>
            {/each}
          {/if}
        </select>
        <Calibrate {micNodeId} micDevice={micDeviceId} />
      </div>
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
  /* calibration stacks node + device pickers above the button/results */
  .settings-grid .ctl.stack {
    flex-direction: column;
    align-items: stretch;
    gap: 6px;
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
