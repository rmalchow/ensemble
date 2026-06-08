<script>
  // One member inside a group card (J arch §4): name, volume, master badge,
  // source stats (master row only when playing), leave. Grouping a node in is
  // done from the group's "Add node…" control, not per-member.
  import { renameNode, setVolume, unfollow } from "../lib/api.js";
  import EditableText from "./EditableText.svelte";
  import VolumeSlider from "./VolumeSlider.svelte";

  let { member, group, self } = $props();

  let isThisMaster = $derived(member.id === group.master);
  let solo = $derived(group.members.length <= 1);
  let pb = $derived(group.playback || { state: "idle" });
  let src = $derived(pb.source || {});
  let showStats = $derived(isThisMaster && pb.state === "playing");
</script>

<div class="member">
  <span class="member-id">
    <span class="dot {member.alive ? 'alive' : 'dead'}"></span>
    <EditableText
      value={member.name}
      onsave={(n) => renameNode(member.id, n)}
      placeholder="(unnamed)"
    />
    {#if isThisMaster}<span class="badge">master</span>{/if}
    {#if member.id === self.id}<span class="chip">this node</span>{/if}
  </span>

  <VolumeSlider
    value={member.volume}
    onchange={(v) => setVolume(member.id, v)}
  />

  {#if showStats}
    <span class="chip">{src.clients ?? 0} listeners</span>
    <span class="chip">{src.restarts ?? 0} reconnects</span>
  {/if}

  <span class="spacer"></span>

  {#if !solo}
    <button
      class="btn icon-btn"
      onclick={() => unfollow(member.id)}
      title="leave group"
      aria-label="leave group">✕</button
    >
  {/if}
</div>

<style>
  /* Fixed-width leading column so every row's volume slider starts at the
     same x, regardless of name length or master/this-node badges. */
  .member-id {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    width: 16rem;
    min-width: 16rem;
    overflow: hidden;
  }

  /* Leave control: icon-only, compact, no text width (Fix 2). */
  .icon-btn {
    line-height: 1;
    padding: 4px 7px;
  }
</style>
