<script lang="ts">
  // Per-group member rows (09 §3). Heavy-adapted from ../media DeviceList +
  // DeviceListItem (the online-dot + row hover/dim CSS is reused; the columns
  // and badges are Ensemble's). Columns: member · channel role · LIVE sync error
  // · render state. Four 09 §3 cases handled by memberRows():
  //   master           → role shown, sync "—", ● rendering
  //   masterNoAudio    → "⊘ master (no local audio)", no role, no sync
  //   noSink (config)  → "⊘ no sink", no role, no sync, NORMAL weight
  //   offline          → row dimmed + "last known" chip, sync "—"
  // The sink-less ⊘ state (a configuration) is visually distinct from offline
  // (dimmed + chip) per the explicit 09 §3 requirement.
  import SyncErrorBadge from './SyncErrorBadge.svelte'
  import MasterBadge from '../common/MasterBadge.svelte'
  import OfflineChip from '../state/OfflineChip.svelte'
  import { memberRows } from '../../lib/groups'
  import { navigate } from '../../lib/router'
  import type { GroupRecord, GroupStatus, NodeRecord } from '../../lib/types'

  interface Props {
    group: GroupRecord
    status?: GroupStatus
    nodes: Map<string, NodeRecord>
    liveness?: Record<string, boolean>
  }
  let { group, status, nodes, liveness = {} }: Props = $props()

  const rows = $derived(memberRows(group, status, nodes, liveness))

  function channelLabel(ch: NodeRecord['channel']): string {
    switch (ch) {
      case 'left':
        return 'L'
      case 'right':
        return 'R'
      default:
        return 'L+R'
    }
  }

  // openNode deep-links a member row into its Node-detail screen (09 §3 → §6).
  // The sink-less ⊘ / "master (no local audio)" badges deep-link to the
  // Capabilities panel anchor so the offline-vs-sink-less distinction is
  // consistent end-to-end (P6.3 §5.6).
  function openNode(id: string, anchor = '') {
    navigate(`/nodes/${encodeURIComponent(id)}${anchor}`)
  }
</script>

{#if rows.length === 0}
  <p class="empty">No members yet.</p>
{:else}
  <table>
    <thead>
      <tr>
        <th>Member</th>
        <th>Role</th>
        <th>Sync error</th>
        <th>State</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as row (row.node.id)}
        <tr class:offline={!row.online && row.kind !== 'masterNoAudio' && row.kind !== 'noSink'}>
          <td class="member">
            <span class="dot" class:online={row.online}></span>
            <button
              type="button"
              class="name"
              onclick={() => openNode(row.node.id)}
              title="Open node detail"
            >
              {row.node.name || row.node.id}
            </button>
          </td>
          <td class="role">
            {#if row.showRole}
              <span class="ch">{channelLabel(row.node.channel)}</span>
            {:else}
              <span class="muted">—</span>
            {/if}
          </td>
          <td class="sync">
            {#if !row.online && row.kind !== 'noSink' && row.kind !== 'masterNoAudio'}
              <OfflineChip variant="last-known" />
            {:else if row.kind === 'noSink' || row.kind === 'masterNoAudio'}
              <span class="muted">—</span>
            {:else}
              <SyncErrorBadge us={row.syncErrorUs} isMaster={row.isMaster} />
            {/if}
          </td>
          <td class="state">
            {#if row.kind === 'masterNoAudio'}
              <button
                type="button"
                class="badge-link"
                onclick={() => openNode(row.node.id, '#capabilities')}
                title="View capabilities"
              >
                <MasterBadge node={row.node} />
              </button>
            {:else if row.kind === 'master'}
              <MasterBadge node={row.node} />
            {:else if row.kind === 'noSink'}
              <button
                type="button"
                class="nosink"
                onclick={() => openNode(row.node.id, '#capabilities')}
                title="Sink-less — view capabilities"
              >
                ⊘ no sink
              </button>
            {:else if !row.online}
              <OfflineChip variant="offline" />
            {:else}
              <span class="rendering">● rendering</span>
            {/if}
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}

<style>
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }
  th {
    text-align: left;
    font-weight: 500;
    color: var(--text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    padding: 0 var(--space-3) var(--space-2);
    border-bottom: 1px solid var(--border-subtle);
  }
  td {
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: middle;
  }
  tbody tr:last-child td {
    border-bottom: none;
  }
  tbody tr:hover {
    background: var(--surface-2);
  }
  /* Offline rows are DIMMED (distinct from the normal-weight sink-less ⊘). */
  tr.offline {
    opacity: 0.5;
  }
  .member {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    background: var(--text-muted);
    flex-shrink: 0;
  }
  .dot.online {
    background: var(--success-bright);
  }
  .name {
    color: var(--text);
    background: none;
    border: none;
    font: inherit;
    padding: 0;
    cursor: pointer;
    text-align: left;
  }
  .name:hover {
    color: var(--accent-bright);
    text-decoration: underline;
  }
  .badge-link {
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    font: inherit;
  }
  .ch {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--text-dim);
    padding: 0.05rem 0.4rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
  }
  .muted {
    color: var(--text-muted);
  }
  .rendering {
    color: var(--success-bright);
    font-size: var(--text-xs);
  }
  /* Sink-less is a configuration, not an error — normal weight, muted tone. */
  .nosink {
    color: var(--text-muted);
    font-size: var(--text-xs);
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    font-family: inherit;
  }
  .nosink:hover {
    color: var(--accent-bright);
    text-decoration: underline;
  }
  .empty {
    color: var(--text-muted);
    font-size: var(--text-sm);
    margin: 0;
  }
</style>
