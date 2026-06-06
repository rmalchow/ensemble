<script lang="ts">
  // Now-playing bar (09 §7): current file + loop state + target group +
  // position/length readout. `length` comes from the selected MediaFile's
  // durationMs; `position` is DERIVED from GroupStatus where obtainable — 08 G.2
  // exposes no play cursor (only streamGen / playing / per-member sync), so the
  // bar degrades to "file ↻ on <group> · <length>" with no live cursor when
  // positionSec is absent (P4.10 §9 risk 2: lights up automatically if a
  // positionSamples field later appears in G.2). mmss() renders the coarse M:SS.
  import { mmss } from '../../lib/format'

  interface Props {
    file: string | null
    loop: boolean
    groupName: string
    positionSec?: number // derived from GroupStatus when available
    lengthSec?: number // from the selected MediaFile.durationMs
  }
  let { file, loop, groupName, positionSec, lengthSec }: Props = $props()

  const hasCursor = $derived(positionSec !== undefined && Number.isFinite(positionSec))
</script>

<div class="nowplaying" class:idle={!file}>
  {#if file}
    <span class="label">Now playing:</span>
    <span class="file">{file}</span>
    {#if loop}<span class="loop" title="looping">↻</span>{/if}
    <span class="on">on group {groupName}</span>
    {#if hasCursor}
      <span class="cursor">
        ▶ {mmss(positionSec)} / {mmss(lengthSec)}
      </span>
    {:else if lengthSec}
      <span class="cursor muted">{mmss(lengthSec)}</span>
    {/if}
  {:else}
    <span class="label muted">Nothing playing on {groupName}.</span>
  {/if}
</div>

<style>
  .nowplaying {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    padding: var(--space-3) var(--space-4);
    border-top: 1px solid var(--border-subtle);
    background: var(--surface-2);
    border-radius: 0 0 var(--radius-md) var(--radius-md);
    font-size: var(--text-sm);
  }
  .nowplaying.idle {
    color: var(--text-muted);
  }
  .label {
    color: var(--text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .file {
    color: var(--text);
    font-family: var(--font-mono);
  }
  .loop {
    color: var(--accent-bright);
  }
  .on {
    color: var(--text-dim);
  }
  .cursor {
    margin-left: auto;
    color: var(--text);
    font-variant-numeric: tabular-nums;
  }
  .muted {
    color: var(--text-muted);
  }
</style>
