<script>
  // One member inside a group card (J arch §4): name, volume, master badge,
  // source stats (master row only when playing), remove. Renaming a node happens
  // on the Nodes page, not here; adding one uses the room's assign roster.
  import { nodeSetVolume, leaveGroup } from "../lib/api.js";
  import VolumeSlider from "./VolumeSlider.svelte";

  let { member, group, self } = $props();

  let isThisMaster = $derived(member.id === group.master);
  let pb = $derived(group.playback || { state: "idle" });
  let src = $derived(pb.source || {});
  let showStats = $derived(isThisMaster && pb.state === "playing");
</script>

<div class="member">
  <span class="member-id">
    <span class="dot {member.alive ? 'alive' : 'dead'}"></span>
    <span class="mname" title={member.name}>{member.name || "(unnamed)"}</span>
    {#if isThisMaster}<span class="badge">master</span>{/if}
    {#if member.id === self.id}<span class="chip">this node</span>{/if}
  </span>

  <VolumeSlider
    value={member.volume}
    onchange={(v) => nodeSetVolume(member, v)}
  />

  <!-- Rams: silence is not a signal — show counts only when they mean something. -->
  {#if showStats && (src.clients ?? 0) > 0}
    <span class="chip">{src.clients} listeners</span>
  {/if}
  {#if showStats && (src.restarts ?? 0) > 0}
    <span class="chip">{src.restarts} reconnects</span>
  {/if}

  <span class="spacer"></span>

  <button
    class="btn icon-btn"
    onclick={() => leaveGroup(member)}
    title="remove from room"
    aria-label="remove from room">✕</button
  >
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
  .mname {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Leave control: icon-only, compact, no text width (Fix 2). */
  .icon-btn {
    line-height: 1;
    padding: 4px 7px;
  }
</style>
