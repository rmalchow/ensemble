<script lang="ts">
  // One enable/disable checkbox for a probed capability path (09 §6 "Disable
  // available paths"). NEW — no mpvsync analogue. Disabling masks the runtime-
  // detected path via per-node config (07); re-enabling restores it. A path that
  // was never probed (`probed === false`) is rendered DISABLED with a "not
  // available on this node" note (you cannot enable what the runtime did not
  // discover, D12). Sinks may carry a precise/coarse tier tag.
  import Chip from '../ui/Chip.svelte'
  import SinkBackendRow from './SinkBackendRow.svelte'
  import { toggleCapability } from '../../lib/nodeStore'
  import type { CapsListKind, SinkTier } from '../../lib/caps'

  interface Props {
    kind: CapsListKind
    name: string
    enabled: boolean
    probed: boolean
    tier?: SinkTier
    disabled: boolean
  }
  let { kind, name, enabled, probed, tier, disabled }: Props = $props()

  function onChange(e: Event) {
    const on = (e.currentTarget as HTMLInputElement).checked
    toggleCapability(kind, name, on)
  }
</script>

<label class="toggle" class:offered={probed} class:notprobed={!probed}>
  <input
    type="checkbox"
    checked={enabled}
    disabled={disabled || !probed}
    onchange={onChange}
    aria-label="{enabled ? 'Disable' : 'Enable'} {name}"
  />
  {#if kind === 'sinks'}
    <SinkBackendRow {name} {tier} />
  {:else}
    <code class="name">{name}</code>
  {/if}
  {#if !probed}
    <Chip tone="muted">not available on this node</Chip>
  {/if}
</label>

<style>
  .toggle {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0.25rem 0;
    cursor: pointer;
  }
  .toggle.notprobed {
    opacity: 0.55;
    cursor: not-allowed;
  }
  input {
    accent-color: var(--accent);
  }
  .name {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--text);
  }
</style>
