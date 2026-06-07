<script>
  // One node in the Nodes table (J arch §4): editable name, id, addrs, caps,
  // liveness, volume, output-delay calibration.
  import { shortId, cidrList, relTime, ports } from "../lib/fmt.js";
  import {
    renameNode,
    setVolume,
    setOutputDelay,
    setOutputDevice,
    setDisabled,
    testTone,
  } from "../lib/api.js";
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

  // output-device selection (D37). List comes from the node's enumeration; the
  // <select> is hidden when empty (no ALSA on that host).
  let outputDevices = $derived(node.outputDevices ?? []);
  let outputDevice = $derived(node.outputDevice ?? "default");

  function onDeviceChange(e) {
    const dev = e.target.value;
    if (dev === outputDevice) return;
    setOutputDevice(node.id, dev).catch(() => {});
  }

  // Tri-state feature chips (D40): playback/opus/input are operator-toggleable.
  // `capabilities` is EFFECTIVE (probed minus disabled); `disabled` lists what
  // the operator turned off. For feature F:
  //   - disabled    → F in node.disabled (amber, clickable to re-enable)
  //   - available   → effective caps has F (normal, clickable to disable)
  //   - unavailable → neither (probed off on this host; dimmed, NOT clickable)
  let disabledSet = $derived(new Set(node.disabled ?? []));

  function effHas(feature) {
    if (feature === "playback") return !!caps.playback;
    if (feature === "opus") return (caps.codecs ?? []).includes("opus");
    if (feature === "input") return (caps.sources ?? []).includes("input");
    return false;
  }

  // state(feature) → "disabled" | "available" | "unavailable"
  function featState(feature) {
    if (disabledSet.has(feature)) return "disabled";
    return effHas(feature) ? "available" : "unavailable";
  }

  // Toggle a feature's disabled membership and PATCH the new list. Unavailable
  // chips are inert (probed off — nothing to toggle).
  function toggleFeature(feature) {
    const st = featState(feature);
    if (st === "unavailable") return;
    const next = new Set(disabledSet);
    if (st === "disabled") next.delete(feature);
    else next.add(feature);
    setDisabled(node.id, [...next]).catch(() => {});
  }

  let features = ["playback", "opus", "input"];
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
    {#each features as f (f)}
      {@const st = featState(f)}
      <button
        type="button"
        class="chip feat {st}"
        disabled={st === "unavailable"}
        title={st === "unavailable"
          ? `${f} not available on this host`
          : st === "disabled"
            ? `${f} disabled — click to enable`
            : `${f} enabled — click to disable`}
        onclick={() => toggleFeature(f)}
      >
        {f}{#if st === "disabled"} (off){/if}
      </button>
    {/each}
    <span class="sep"></span>
    {#each (caps.codecs ?? []).filter((c) => c !== "opus") as c}
      <span class="chip">{c}</span>
    {/each}
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

  <div class="row wrap">
    {#if outputDevices.length > 0}
      <label class="row small muted device">
        output device
        <select value={outputDevice} onchange={onDeviceChange}>
          {#each outputDevices as d (d.id)}
            <option value={d.id}>{d.desc} ({d.id})</option>
          {/each}
        </select>
      </label>
    {/if}
    <button class="small" title="play a 1s test tone on this node's output"
      onclick={() => testTone(node.id)}>♪ test tone</button>
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
  .device {
    gap: 6px;
  }
  .device select {
    max-width: 16rem;
  }
  /* tri-state feature chips (D40) */
  .chip.feat {
    cursor: pointer;
    border: 1px solid transparent;
    font: inherit;
  }
  .chip.feat.available {
    /* normal look; subtle affordance via hover */
  }
  .chip.feat.available:hover {
    border-color: currentColor;
  }
  .chip.feat.disabled {
    background: #b45309;
    color: #fff;
  }
  .chip.feat.unavailable {
    cursor: default;
    opacity: 0.45;
    text-decoration: line-through;
  }
  .sep {
    width: 1px;
    align-self: stretch;
    background: var(--border, #ccc);
    margin: 0 2px;
  }
</style>
