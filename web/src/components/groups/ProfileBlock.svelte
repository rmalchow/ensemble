<script lang="ts">
  // Profile block (09 §5): codec / FEC / rate shown as the auto-negotiated value
  // (from the live GroupStatus.profile) with the limiting "least-capable"
  // listener named (mirrors 04 §4.3.2 client-side). Each field is auto-by-default
  // / read-only with an explicit override toggle (D3/D4). pcm is the mandatory
  // baseline (always selectable); FEC default is xorParity (D4). An infeasible
  // override → 422 (surfaced by the parent's ErrorBanner). The parent owns the
  // PATCH; this emits the chosen override (or null to revert to auto). NEW.
  import Chip from '../ui/Chip.svelte'
  import { leastCapableCodec, leastCapableFec } from '../../lib/groups'
  import type {
    GroupRecord,
    GroupStatus,
    NodeRecord,
    CodecName,
    FECName,
  } from '../../lib/types'

  interface Props {
    group: GroupRecord
    status?: GroupStatus
    nodes: Map<string, NodeRecord>
    disabled?: boolean
    onCodec: (c: CodecName | null) => void
    onFec: (f: FECName | null) => void
  }
  let { group, status, nodes, disabled = false, onCodec, onFec }: Props = $props()

  // Effective (negotiated) values prefer the live status; fall back to stored.
  const effective = $derived(status?.profile ?? group.profile)

  const codecLimit = $derived(leastCapableCodec(group, status, nodes))
  const fecLimit = $derived(leastCapableFec(group, status, nodes))

  // An override is "on" when the stored profile differs from what auto would
  // floor to — but the UI exposes an explicit per-axis toggle instead of
  // inferring it, so each axis tracks its own override flag.
  let codecOverride = $state(false)
  let fecOverride = $state(false)

  function limiterName(id: string | undefined): string {
    if (!id) return ''
    const n = nodes.get(id)
    return n?.name || id
  }
</script>

<div class="profile">
  <h4>Profile</h4>

  <!-- Codec -->
  <div class="axis">
    <div class="axis-head">
      <span class="axis-label">Codec</span>
      <label class="toggle">
        <input
          type="checkbox"
          bind:checked={codecOverride}
          {disabled}
          onchange={() => {
            // Toggling OFF reverts to auto (a null override → engine re-negotiates);
            // toggling ON only reveals the select — no write until a value is picked.
            if (!codecOverride) onCodec(null)
          }}
        />
        <span>Override</span>
      </label>
    </div>
    {#if !codecOverride}
      <div class="auto">
        <Chip tone="accent">{effective.codec.toUpperCase()}</Chip>
        <span class="auto-note">
          auto{#if codecLimit.nodeId}
            — limited by {limiterName(codecLimit.nodeId)}{/if}
        </span>
      </div>
    {:else}
      <select
        value={effective.codec}
        {disabled}
        onchange={(e) => onCodec((e.currentTarget as HTMLSelectElement).value as CodecName)}
      >
        <!-- pcm is the mandatory baseline (always selectable). -->
        <option value="pcm">PCM (baseline)</option>
        <option value="opus">Opus</option>
      </select>
    {/if}
  </div>

  <!-- FEC -->
  <div class="axis">
    <div class="axis-head">
      <span class="axis-label">FEC</span>
      <label class="toggle">
        <input
          type="checkbox"
          bind:checked={fecOverride}
          {disabled}
          onchange={() => {
            if (!fecOverride) onFec(null)
          }}
        />
        <span>Override</span>
      </label>
    </div>
    {#if !fecOverride}
      <div class="auto">
        <Chip tone="neutral">{effective.fec}</Chip>
        <span class="auto-note">
          auto{#if fecLimit.nodeId}
            — limited by {limiterName(fecLimit.nodeId)}{/if}
        </span>
      </div>
    {:else}
      <select
        value={effective.fec}
        {disabled}
        onchange={(e) => onFec((e.currentTarget as HTMLSelectElement).value as FECName)}
      >
        <option value="none">None</option>
        <option value="xorParity">XOR parity</option>
        <option value="duplicate">Duplicate</option>
      </select>
    {/if}
  </div>

  <!-- Rate + read-only transport params (A.12 canonical, shown for reference). -->
  <div class="readonly">
    <span>{(effective.rate / 1000).toFixed(0)} kHz</span>
    <span class="sep">·</span>
    <span>{effective.framesPerChunk} frames/chunk</span>
    <span class="sep">·</span>
    <span>k={effective.fecK}</span>
    <span class="sep">·</span>
    <span>interleave {effective.interleave}</span>
  </div>
</div>

<style>
  .profile {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
  }
  h4 {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  .axis {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .axis-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .axis-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
  }
  .toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--text-muted);
    cursor: pointer;
  }
  .toggle input {
    accent-color: var(--accent);
  }
  .auto {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .auto-note {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  select {
    padding: 0.4rem 0.5rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--surface-2);
    color: var(--text);
    font: inherit;
    font-size: var(--text-sm);
  }
  .readonly {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .sep {
    opacity: 0.5;
  }
</style>
