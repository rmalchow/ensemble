<script lang="ts">
  // "Discovered — not yet in this cluster" table (09 §4): node | addr |
  // fingerprint | action, plus a Rescan control that triggers a fresh discovery
  // sweep (C.2 fan-out read; no config write). Empty state uses the §4 copy.
  // Per-row busy/error and the Adopt callback come from the parent screen.
  import type { Snippet } from 'svelte'
  import Button from '../ui/Button.svelte'
  import Skeleton from '../state/Skeleton.svelte'
  import type { DiscoveredNode } from '../../lib/cluster'

  interface Props {
    nodes: DiscoveredNode[]
    loading: boolean
    onRescan: () => void
    // row is a snippet the parent supplies to render each DiscoveredRow with its
    // own busy/error/onAdopt wiring, keeping action state in the screen.
    row: Snippet<[DiscoveredNode]>
  }
  let { nodes, loading, onRescan, row }: Props = $props()
</script>

<section class="discovered">
  <div class="section-head">
    <h2>Discovered — not yet in this cluster</h2>
    <Button variant="ghost" onclick={onRescan} loading={loading}>Rescan</Button>
  </div>

  {#if loading}
    <div class="pad"><Skeleton rows={3} /></div>
  {:else if nodes.length === 0}
    <p class="empty">
      No new players found nearby — power one on, or it may already belong to a
      cluster (use Takeover).
    </p>
  {:else}
    <table>
      <thead>
        <tr>
          <th>node</th>
          <th>addr</th>
          <th>fingerprint</th>
          <th>action</th>
        </tr>
      </thead>
      <tbody>
        {#each nodes as node (node.nodeId)}
          {@render row(node)}
        {/each}
      </tbody>
    </table>
  {/if}
</section>

<style>
  .discovered {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .section-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  h2 {
    margin: 0;
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text);
  }
  .empty {
    color: var(--text-muted);
    font-size: var(--text-sm);
    padding: var(--space-4);
    border: 1px dashed var(--border);
    border-radius: var(--radius-md);
    margin: 0;
  }
  .pad {
    padding: var(--space-2) 0;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }
  th {
    text-align: left;
    color: var(--text-muted);
    font-weight: 500;
    font-size: var(--text-xs);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
  }
</style>
