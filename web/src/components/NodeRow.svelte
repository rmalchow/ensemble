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

  // Output-delay slider (D36), wired exactly like VolumeSlider: the held draft
  // tracks the thumb while dragging; a fresh snapshot re-syncs once released.
  // Range 0..150 ms, step 5, debounced ~200ms PATCH {outputDelayMs}. The old
  // number input reset to 0 mid-edit; this controlled pattern fixes it.
  let delayDragging = $state(false);
  let delayMs = $state(0);
  $effect(() => {
    const v = node.outputDelayMs ?? 0;
    if (!delayDragging) delayMs = v;
  });

  let delayTimer = null;
  function fireDelay() {
    if (delayTimer) {
      clearTimeout(delayTimer);
      delayTimer = null;
    }
    setOutputDelay(node.id, delayMs).catch(() => {});
  }
  function onDelayInput(e) {
    delayDragging = true;
    delayMs = Number(e.target.value);
    if (delayTimer) clearTimeout(delayTimer);
    delayTimer = setTimeout(() => {
      delayTimer = null;
      setOutputDelay(node.id, delayMs).catch(() => {});
    }, 200);
  }
  function onDelayCommit() {
    fireDelay();
    delayDragging = false;
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
  // the operator turned off. Three UNMISTAKABLE states per feature:
  //   - "on"          → available + enabled (effective caps has F, not disabled):
  //                     green "●", clickable, tooltip "click to disable".
  //   - "off"         → available but disabled (F in node.disabled): amber outline
  //                     "○", clickable, tooltip "click to enable".
  //   - "unavailable" → not probed on this host (neither): dimmed "✕" strike,
  //                     NOT clickable, tooltip "not available on this host".
  let disabledSet = $derived(new Set(node.disabled ?? []));

  function effHas(feature) {
    if (feature === "playback") return !!caps.playback;
    if (feature === "opus") return (caps.codecs ?? []).includes("opus");
    if (feature === "input") return (caps.sources ?? []).includes("input");
    return false;
  }

  // state(feature) → "on" | "off" | "unavailable"
  function featState(feature) {
    if (disabledSet.has(feature)) return "off";
    return effHas(feature) ? "on" : "unavailable";
  }

  // The state's glyph + accessible tooltip.
  function featGlyph(st) {
    if (st === "on") return "●";
    if (st === "off") return "○";
    return "✕";
  }
  function featTitle(feature, st) {
    if (st === "unavailable") return `${feature}: not available on this host`;
    if (st === "off") return `${feature}: disabled — click to enable`;
    return `${feature}: enabled — click to disable`;
  }

  // Toggle a feature's disabled membership and PATCH the new list. Unavailable
  // chips are inert (probed off — nothing to toggle).
  function toggleFeature(feature) {
    const st = featState(feature);
    if (st === "unavailable") return;
    const next = new Set(disabledSet);
    if (st === "off") next.delete(feature);
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
      {#if node.alive}
        {relTime(node.lastSeen)}
      {:else}
        offline{#if node.stale}<span class="offline"> · stale</span>{/if}
      {/if}
    </span>
  </div>

  <div class="row wrap small muted netinfo">
    {cidrList(node.addrs)}
  </div>

  {#if portList}
    <div class="row wrap small muted netinfo">
      {portList}
    </div>
  {/if}

  <div class="row wrap feature-row">
    <span class="muted small feat-label">features</span>
    {#each features as f (f)}
      {@const st = featState(f)}
      <button
        type="button"
        class="chip feat {st}"
        disabled={st === "unavailable"}
        aria-pressed={st === "on"}
        title={featTitle(f, st)}
        onclick={() => toggleFeature(f)}
      >
        <span class="glyph" aria-hidden="true">{featGlyph(st)}</span>{f}
      </button>
    {/each}
  </div>

  {#if (caps.codecs ?? []).filter((c) => c !== "opus").length || (caps.formats ?? []).length}
    <div class="row wrap format-row">
      <span class="muted small feat-label">formats</span>
      {#each (caps.codecs ?? []).filter((c) => c !== "opus") as c}
        <span class="chip plain">{c}</span>
      {/each}
      {#each caps.formats ?? [] as f}<span class="chip plain">{f}</span>{/each}
    </div>
  {/if}

  <div class="row wrap">
    <span class="muted small">vol</span>
    <VolumeSlider value={node.volume} onchange={(v) => setVolume(node.id, v)} />
    <span class="spacer"></span>
    <div class="delay">
      <div class="row small muted delay-ctl">
        <span>output delay</span>
        <input
          type="range"
          min="0"
          max="150"
          step="5"
          value={delayMs}
          oninput={onDelayInput}
          onchange={onDelayCommit}
          onpointerup={onDelayCommit}
          aria-label="output delay (ms)"
        />
        <span class="delay-val">{delayMs} ms</span>
      </div>
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
    gap: 8px;
  }
  /* address + port lines: breathing room and comfortable wrap spacing */
  .netinfo {
    padding: 6px 0;
    line-height: 1.6;
  }
  /* a touch more separation around the whole netinfo block */
  .netinfo:first-of-type {
    margin-top: 2px;
  }
  .netinfo + .netinfo {
    margin-top: -2px;
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
  .delay-ctl {
    align-items: center;
    gap: 6px;
  }
  .delay-ctl input[type="range"] {
    width: 110px;
  }
  .delay-val {
    min-width: 3.2rem;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
  .device {
    gap: 6px;
  }
  .device select {
    max-width: 16rem;
  }
  /* the two chip rows: a small leading label, then the chips */
  .feature-row,
  .format-row {
    align-items: center;
    gap: 8px;
    padding: 6px 0;
  }
  .feat-label {
    min-width: 3.6rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-size: 0.7rem;
    opacity: 0.7;
  }

  /* tri-state feature chips (D40): three UNMISTAKABLE states. */
  .chip.feat {
    /* match the passive FORMATS chips exactly (app.css .chip: 11px / 0 8px),
       with button UA defaults zeroed — the pills must not read "button". */
    appearance: none;
    display: inline-flex;
    align-items: center;
    gap: 3px;
    margin: 0;
    min-height: 0;
    font-size: 11px;
    line-height: 1.5;
    border: 1px solid transparent;
    border-radius: 10px;
    padding: 0 8px;
  }
  .chip.feat .glyph {
    font-size: 8px;
    line-height: 1;
  }

  /* ON: available + enabled — solid green accent, ● , clickable. */
  .chip.feat.on {
    cursor: pointer;
    background: #15803d;
    border-color: #15803d;
    color: #fff;
  }
  .chip.feat.on:hover {
    background: #166534;
  }

  /* OFF: available but disabled — outlined amber, ○ , clickable. */
  .chip.feat.off {
    cursor: pointer;
    background: transparent;
    border-color: #b45309;
    color: #b45309;
  }
  .chip.feat.off:hover {
    background: rgba(180, 83, 9, 0.12);
  }

  /* UNAVAILABLE: not probed on this host — dimmed + strike, ✕ , NOT clickable. */
  .chip.feat.unavailable {
    cursor: not-allowed;
    background: transparent;
    border-color: var(--border, #ccc);
    color: var(--muted, #888);
    opacity: 0.5;
    text-decoration: line-through;
  }

  /* passive format chips: plain, never togglable, default cursor. */
  .chip.plain {
    cursor: default;
  }
</style>
