<script>
  // One derived group (J arch §4): name, playback bar, members, settings text.
  import {
    nodeById,
    groupLabel,
    groupNameIsDerived,
    addTargets,
  } from "../lib/derive.js";
  import { renameGroup, follow, setGroupSettings } from "../lib/api.js";
  import EditableText from "./EditableText.svelte";
  import PlaybackBar from "./PlaybackBar.svelte";
  import MemberRow from "./MemberRow.svelte";

  let { group, snapshot, self } = $props();

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

<div class="card">
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

  <div class="members">
    {#each members as member (member.id)}
      <MemberRow {member} {group} {self} {snapshot} />
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
  </div>
</div>

<style>
  /* consistent vertical rhythm between the card's stacked rows */
  .card {
    display: flex;
    flex-direction: column;
    gap: 8px;
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
