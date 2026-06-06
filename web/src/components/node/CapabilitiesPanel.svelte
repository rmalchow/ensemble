<script lang="ts">
  // Capabilities & audio backends panel (09 §6, README §6.5, D16/D12/D17). ALWAYS
  // shown (both render variants). Renders the EFFECTIVE structured Capabilities —
  // render flag, sinks (precise/coarse), encode/decode codecs, fec, max rate — and
  // hosts the per-node disable toggles + force-control-only flip. The probed
  // superset each toggle row is drawn from comes from caps.probedSet (effective ∪
  // draft-masked, or an explicit caps.probed); a never-probed path is offered as a
  // disabled "not available" row (you cannot enable what the runtime did not
  // discover, D12). Decode codecs render "—" when the node is not a listener.
  import CapabilityToggleRow from './CapabilityToggleRow.svelte'
  import ForceControlOnlyToggle from './ForceControlOnlyToggle.svelte'
  import Chip from '../ui/Chip.svelte'
  import { sinkTier, enabledSet, probedSet, type CapsListKind } from '../../lib/caps'
  import type { NodeDetailView, CapabilityMask } from '../../lib/node'

  interface Props {
    node: NodeDetailView
    draftMask?: CapabilityMask
    // renderEnabled is the live previewed render flag (drives the render readout +
    // the decode "—" treatment, which follows being a listener).
    renderEnabled: boolean
    disabled: boolean
  }
  let { node, draftMask, renderEnabled, disabled }: Props = $props()

  // forceOn reflects the explicit force-control-only mask bit (render === false).
  const forceOn = $derived(draftMask?.render === false)

  // rows builds, per axis, the union of probed paths with their enabled state and
  // (for sinks) precise/coarse tier — the toggle list.
  function rows(kind: CapsListKind) {
    const enabled = new Set(enabledSet(node.caps, draftMask, kind))
    const probed = probedSet(node.caps, draftMask, kind, node.probed)
    return probed.map((name) => ({
      name,
      enabled: enabled.has(name),
      probed: true,
      tier: kind === 'sinks' ? sinkTier(name) : undefined,
    }))
  }

  const sinkRows = $derived(rows('sinks'))
  const encodeRows = $derived(rows('encode'))
  const decodeRows = $derived(rows('decode'))
  const fecRows = $derived(rows('fec'))
  const hasEnabledSink = $derived(
    enabledSet(node.caps, draftMask, 'sinks').length > 0,
  )
</script>

<div class="caps" id="capabilities">
  <p class="probe-note">runtime-discovered — D12; not a build flag</p>

  <div class="grid">
    <div class="k">render</div>
    <div class="v">
      {#if renderEnabled}
        <Chip tone="success">● yes (listener-capable)</Chip>
      {:else}
        <Chip tone="muted">○ no (control / media only)</Chip>
      {/if}
    </div>

    <div class="k">sinks</div>
    <div class="v list">
      {#if sinkRows.length === 0}
        <span class="dash">(none probed)</span>
      {:else}
        {#each sinkRows as r (r.name)}
          <CapabilityToggleRow
            kind="sinks"
            name={r.name}
            enabled={r.enabled}
            probed={r.probed}
            tier={r.tier}
            {disabled}
          />
        {/each}
      {/if}
      {#if !hasEnabledSink && sinkRows.length > 0}
        <span class="reenable">↳ re-enable a sink to gain audio output</span>
      {/if}
    </div>

    <div class="k">encode codecs</div>
    <div class="v list">
      {#each encodeRows as r (r.name)}
        <CapabilityToggleRow
          kind="encode"
          name={r.name}
          enabled={r.enabled}
          probed={r.probed}
          {disabled}
        />
      {/each}
      <span class="axis-note">what it can ORIGINATE as master; pcm baseline</span>
    </div>

    <div class="k">decode codecs</div>
    <div class="v list">
      {#if !renderEnabled}
        <span class="dash">— (not a listener)</span>
      {:else}
        {#each decodeRows as r (r.name)}
          <CapabilityToggleRow
            kind="decode"
            name={r.name}
            enabled={r.enabled}
            probed={r.probed}
            {disabled}
          />
        {/each}
        <span class="axis-note">what it can PLAY as a listener</span>
      {/if}
    </div>

    <div class="k">fec</div>
    <div class="v list">
      {#each fecRows as r (r.name)}
        <CapabilityToggleRow
          kind="fec"
          name={r.name}
          enabled={r.enabled}
          probed={r.probed}
          {disabled}
        />
      {/each}
    </div>

    <div class="k">max rate</div>
    <div class="v"><code class="mono">{node.caps.maxRate}</code> Hz</div>
  </div>

  <div class="disable-box">
    <p class="box-title">Disable available paths (per-node config → effective caps)</p>
    <p class="box-hint">
      Unchecking a probed path removes it from what this node advertises. The node
      re-probes / re-masks and re-advertises a narrower effective Capabilities;
      the group profile re-negotiates if a now-removed codec / FEC was in use.
    </p>
    <ForceControlOnlyToggle on={forceOn} {disabled} />
  </div>
</div>

<style>
  .caps {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .probe-note {
    margin: 0;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .grid {
    display: grid;
    grid-template-columns: 8rem 1fr;
    gap: var(--space-2) var(--space-3);
    align-items: start;
  }
  .k {
    font-size: var(--text-sm);
    color: var(--text-muted);
    padding-top: 0.25rem;
  }
  .v {
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  .v.list {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
  }
  .dash {
    color: var(--text-muted);
  }
  .reenable {
    font-size: var(--text-xs);
    color: var(--warn-bright);
  }
  .axis-note {
    font-size: var(--text-xs);
    color: var(--text-muted);
    margin-top: 0.1rem;
  }
  .mono {
    font-family: var(--font-mono);
  }
  .disable-box {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--surface-2);
    padding: var(--space-3) var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .box-title {
    margin: 0;
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text);
  }
  .box-hint {
    margin: 0;
    font-size: var(--text-xs);
    color: var(--text-muted);
    line-height: 1.5;
  }
</style>
