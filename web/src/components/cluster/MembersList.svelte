<script lang="ts">
  // "Cluster members" table (09 §4): node | addrs | status | actions. Loads
  // independently of the discovered table (discovery enrichment can be slow —
  // P2.7 §9.4 wants per-table loading). The empty case effectively never happens
  // (a one-node cluster lists itself), but we render a friendly note defensively.
  import type { Snippet } from 'svelte'
  import Skeleton from '../state/Skeleton.svelte'
  import type { MemberNode } from '../../lib/cluster'

  interface Props {
    nodes: MemberNode[]
    loading: boolean
    // row is supplied by the screen so each MemberRow keeps its own busy/error +
    // confirm-gated action wiring.
    row: Snippet<[MemberNode]>
  }
  let { nodes, loading, row }: Props = $props()
</script>

<section class="members">
  <h2>Cluster members</h2>

  {#if loading}
    <div class="pad"><Skeleton rows={3} /></div>
  {:else if nodes.length === 0}
    <p class="empty">No members yet.</p>
  {:else}
    <table>
      <thead>
        <tr>
          <th>node</th>
          <th>addrs</th>
          <th>status</th>
          <th class="right">actions</th>
        </tr>
      </thead>
      <tbody>
        {#each nodes as node (node.id)}
          {@render row(node)}
        {/each}
      </tbody>
    </table>
  {/if}
</section>

<style>
  .members {
    display: flex;
    flex-direction: column;
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
  th.right {
    text-align: right;
  }
</style>
