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

  <button
    class="btn icon-btn"
    onclick={() => leaveGroup(member)}
    title="remove from room"
    aria-label="remove from room">✕</button
  >
</div>

<style>
  /* Leading column: prefers 16rem (so sliders line up across rows when there's
     room) but shrinks on a narrow card — the name ellipsises. */
  .member-id {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    flex: 0 1 16rem;
    min-width: 0;
    overflow: hidden;
  }
  .mname {
    flex: 1 1 auto;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .member-id :global(.badge),
  .member-id :global(.chip) {
    flex: 0 0 auto;
  }

  /* volume fills the middle of a wide row */
  .member :global(.vol) {
    flex: 1 1 auto;
    min-width: 6rem;
  }

  /* Leave control: icon-only, compact, pinned to the right of the name line. */
  .icon-btn {
    flex: 0 0 auto;
    margin-left: auto;
    line-height: 1;
    padding: 4px 7px;
  }

  /* Narrow cards: name + remove button on the first line (badges hidden), the
     volume slider wraps to the next line. */
  @media (max-width: 560px) {
    .member {
      flex-wrap: wrap;
    }
    .member-id {
      order: 1;
      flex: 1 1 auto;
    }
    .member-id :global(.badge),
    .member-id :global(.chip) {
      display: none;
    }
    .icon-btn {
      order: 2;
    }
    .member :global(.vol) {
      order: 3;
      flex: 1 1 100%;
    }
    .member > :global(.chip) {
      order: 3;
    }
  }
</style>
