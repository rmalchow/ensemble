<script lang="ts">
  // One cluster member (09 §4 members table): name (a LINK to the node detail
  // page), addrs (host:port when live), online status chip, the sink-less
  // "control / media only" tag (+ "master (no local audio)" badge) for
  // Caps.Render === false members (D17), and a single trashcan Forget action
  // (confirm-gated by the parent; works while offline — it revokes+removes).
  // Takeover lives in the DISCOVERED table (a foreign node is adoptable-with-
  // password, not a member of THIS cluster). An offline row is dimmed; the
  // sink-less tag is normal-weight and distinct from that offline treatment.
  import OfflineChip from '../state/OfflineChip.svelte'
  import ControlMediaOnlyTag from './ControlMediaOnlyTag.svelte'
  import { isSinkless, isMasterNoAudio } from '../../lib/clusterStore'
  import type { MemberNode, ApiError } from '../../lib/cluster'

  interface Props {
    node: MemberNode
    busy: boolean
    error?: ApiError
    onForget: () => void
    onOpenNode: () => void
  }
  let { node, busy, error, onForget, onOpenNode }: Props = $props()

  const sinkless = $derived(isSinkless(node))
  const masterNoAudio = $derived(isMasterNoAudio(node))
</script>

<tr class:offline={!node.online} class:busy>
  <td class="node">
    <button type="button" class="name" onclick={onOpenNode} disabled={busy}>
      {node.name || node.id}
    </button>
    <span class="tags">
      <span class="id mono">{node.id}</span>
      {#if sinkless}
        <ControlMediaOnlyTag nodeId={node.id} />
      {/if}
      {#if masterNoAudio}
        <span class="master-badge">master (no local audio)</span>
      {/if}
    </span>
  </td>
  <!-- Defensive ?? []: a null addrs from an older server must degrade to "—",
       not crash the whole members table render. -->
  <td class="mono addrs">{(node.addrs ?? []).join(', ') || '—'}</td>
  <td class="status">
    {#if node.online}
      <span class="online">● online</span>
    {:else}
      <span class="offline-wrap">⌁ <OfflineChip /></span>
    {/if}
  </td>
  <td class="actions">
    <div class="action-row">
      <button
        type="button"
        class="trash"
        onclick={onForget}
        disabled={busy}
        aria-label="Forget {node.name || node.id}"
        title="Forget node"
      >
        🗑
      </button>
    </div>
    {#if error}
      <p class="row-error" role="alert">
        <span class="code mono">{error.code}</span>
        <span>{error.message}</span>
      </p>
    {/if}
  </td>
</tr>

<style>
  tr.offline {
    opacity: 0.6;
  }
  tr.busy {
    opacity: 0.8;
  }
  td {
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    color: var(--text);
    vertical-align: top;
  }
  .node {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .name {
    background: none;
    border: none;
    padding: 0;
    font: inherit;
    text-align: left;
    color: var(--text);
    font-weight: 500;
    cursor: pointer;
  }
  .name:hover:not(:disabled) {
    text-decoration: underline;
  }
  .tags {
    display: inline-flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .addrs {
    font-size: var(--text-xs);
  }
  .online {
    color: var(--success-bright);
    font-size: var(--text-sm);
  }
  .offline-wrap {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .master-badge {
    padding: 0.1rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--accent);
    color: var(--accent-bright);
    background: rgba(31, 111, 235, 0.12);
    font-weight: 500;
  }
  .action-row {
    display: flex;
    gap: var(--space-2);
    justify-content: flex-end;
  }
  .trash {
    background: none;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0.35rem 0.6rem;
    font-size: var(--text-sm);
    line-height: 1;
    cursor: pointer;
    color: var(--danger-bright);
  }
  .trash:hover:not(:disabled) {
    border-color: var(--danger-bright);
    background: rgba(218, 54, 51, 0.12);
  }
  .trash:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .row-error {
    margin: var(--space-2) 0 0;
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
    font-size: var(--text-xs);
    color: var(--danger-bright);
    text-align: right;
  }
  .code {
    color: var(--danger-bright);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
