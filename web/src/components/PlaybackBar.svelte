<script>
  // Playback state + position + play/pause + stop for a group (J arch §4, D39).
  import { position } from "../lib/fmt.js";
  import { stop, pause, resume } from "../lib/api.js";

  let { group } = $props();

  let pb = $derived(group.playback || { state: "idle" });
  let playing = $derived(pb.state === "playing");
  let paused = $derived(pb.state === "paused");
  let active = $derived(playing || paused);

  async function onstop() {
    try {
      await stop(group.master);
    } catch {
      // toast shown by api.js
    }
  }

  async function onpause() {
    try {
      await pause(group.master);
    } catch {
      // toast shown by api.js
    }
  }

  async function onresume() {
    try {
      await resume(group.master);
    } catch {
      // toast shown by api.js
    }
  }
</script>

<div class="row wrap" style="margin: 4px 0 8px;">
  {#if active}
    {#if paused}
      <span class="badge paused">paused</span>
    {:else}
      <span class="badge">playing</span>
    {/if}
    <span class="muted small" title={pb.uri}>{pb.uri}</span>
    <span class="small">{position(pb.positionSec)}</span>
    <span class="chip">{pb.codec}</span>
    <span class="chip">{pb.transport}</span>
    <span class="spacer"></span>
    {#if paused}
      <button class="btn" onclick={onresume}>▶ resume</button>
    {:else}
      <button class="btn" onclick={onpause}>⏸ pause</button>
    {/if}
    <button class="btn btn-danger" onclick={onstop}>Stop</button>
  {:else}
    <span class="muted small">idle</span>
  {/if}
</div>

<style>
  .badge.paused {
    background: #b45309;
    color: #fff;
  }
</style>
