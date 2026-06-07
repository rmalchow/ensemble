<script>
  // One derived group (J arch §4): name, playback bar, members, settings text.
  import {
    nodeById,
    groupLabel,
    groupNameIsDerived,
    addTargets,
  } from "../lib/derive.js";
  import { renameGroup, follow } from "../lib/api.js";
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

  <div class="hint">
    codec {settings.codec ?? "opus"} · transport {settings.transport ?? "udp"} ·
    buffer {settings.bufferMs ?? 150} ms
  </div>
</div>

<style>
  /* consistent vertical rhythm between the card's stacked rows */
  .card {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
</style>
