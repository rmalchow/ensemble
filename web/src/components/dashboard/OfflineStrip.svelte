<script lang="ts">
  // Trailing strip of cluster members currently unreachable + their last-known
  // group (09 §3). Dimmed treatment via the shared OfflineChip. Hidden entirely
  // when every member is online (no empty strip clutter).
  import OfflineChip from '../state/OfflineChip.svelte'
  import type { NodeRecord, GroupRecord } from '../../lib/types'

  interface Props {
    nodes: NodeRecord[]
    liveness: Record<string, boolean>
    groupByNode: Map<string, GroupRecord>
  }
  let { nodes, liveness, groupByNode }: Props = $props()

  const offline = $derived(nodes.filter((n) => liveness[n.id] === false))
</script>

{#if offline.length > 0}
  <div class="strip" aria-label="Offline players">
    <span class="label">Offline</span>
    <ul>
      {#each offline as n (n.id)}
        <li>
          <span class="name">{n.name || n.id}</span>
          {#if groupByNode.get(n.id)}
            <span class="group">last in {groupByNode.get(n.id)?.name}</span>
          {/if}
          <OfflineChip variant="offline" />
        </li>
      {/each}
    </ul>
  </div>
{/if}

<style>
  .strip {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
    padding: var(--space-3) var(--space-4);
    border: 1px dashed var(--border);
    border-radius: var(--radius-md);
    background: var(--surface-2);
    opacity: 0.85;
  }
  .label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
  }
  ul {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    list-style: none;
    margin: 0;
    padding: 0;
  }
  li {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  .group {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
