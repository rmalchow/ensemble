<script>
  // One node in the Nodes table (J arch §4): editable name, id, addrs, caps,
  // liveness, volume, output-delay calibration.
  import { shortId, cidrList, relTime, ports } from "../lib/fmt.js";
  import { renameNode, setVolume, setOutputDelay } from "../lib/api.js";
  import EditableText from "./EditableText.svelte";
  import VolumeSlider from "./VolumeSlider.svelte";

  let { node, self } = $props();

  let isSelf = $derived(node.id === self.id);
  let caps = $derived(node.capabilities || {});
  let portList = $derived(ports(node));

  // local draft for the output-delay input; reverts to node.outputDelayMs on
  // each new snapshot, committed on blur/Enter only (D36).
  let delayDraft = $state(0);
  $effect(() => {
    delayDraft = node.outputDelayMs ?? 0;
  });

  function commitDelay() {
    let ms = Math.round(Number(delayDraft));
    if (Number.isNaN(ms)) {
      delayDraft = node.outputDelayMs ?? 0;
      return;
    }
    if (ms > 500) ms = 500;
    if (ms < -500) ms = -500;
    if (ms === (node.outputDelayMs ?? 0)) return;
    setOutputDelay(node.id, ms).catch(() => {});
  }

  function onkey(e) {
    if (e.key === "Enter") {
      e.preventDefault();
      e.target.blur();
    }
  }
</script>

<div class="noderow card" class:stale={!node.alive}>
  <div class="row wrap between">
    <div class="row">
      <span class="dot {node.alive ? 'alive' : 'dead'}"></span>
      <EditableText
        value={node.name}
        onsave={(n) => renameNode(node.id, n)}
        placeholder="(unnamed)"
      />
      {#if isSelf}<span class="chip">this node</span>{/if}
      <span class="node-id small" title={node.id}>{shortId(node.id)}</span>
    </div>
    <span class="muted small">
      {node.alive ? relTime(node.lastSeen) : "offline"}
      {#if node.stale}<span class="offline"> · stale</span>{/if}
    </span>
  </div>

  <div class="row wrap small muted">
    {cidrList(node.addrs)}
  </div>

  {#if portList}
    <div class="row wrap small muted">
      {portList}
    </div>
  {/if}

  <div class="row wrap">
    <span class="chip">playback {caps.playback ? "yes" : "no"}</span>
    {#each caps.codecs ?? [] as c}<span class="chip">{c}</span>{/each}
    {#each caps.formats ?? [] as f}<span class="chip">{f}</span>{/each}
  </div>

  <div class="row wrap">
    <span class="muted small">vol</span>
    <VolumeSlider value={node.volume} onchange={(v) => setVolume(node.id, v)} />
    <span class="spacer"></span>
    <div class="delay">
      <label class="row small muted">
        output delay (ms)
        <input
          type="number"
          min="-500"
          max="500"
          bind:value={delayDraft}
          onblur={commitDelay}
          onkeydown={onkey}
          style="width: 70px;"
        />
      </label>
      <div class="hint">
        compensates fixed device latency; causes a brief local restart
      </div>
    </div>
  </div>
</div>

<style>
  /* vertical rhythm between the stacked rows inside a node card */
  .noderow {
    flex-direction: column;
    align-items: stretch;
    gap: 6px;
  }
  .delay {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 2px;
  }
  .delay .hint {
    text-align: right;
  }
</style>
