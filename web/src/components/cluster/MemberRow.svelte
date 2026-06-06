<script lang="ts">
  // One cluster member (09 §4 members table): id/name, addrs, online status chip,
  // the sink-less "control / media only" tag (+ "master (no local audio)" badge)
  // for Caps.Render === false members (D17), and per-row Node / Takeover / Forget
  // actions. Takeover/Forget are confirm-gated by the parent and remain enabled
  // while offline (Forget still revokes+removes; Takeover may need the node
  // reachable). An offline row is dimmed; the sink-less tag is normal-weight and
  // distinct from that offline treatment.
  import Button from '../ui/Button.svelte'
  import OfflineChip from '../state/OfflineChip.svelte'
  import ControlMediaOnlyTag from './ControlMediaOnlyTag.svelte'
  import { isSinkless, isMasterNoAudio } from '../../lib/clusterStore'
  import type { MemberNode, ApiError } from '../../lib/cluster'

  interface Props {
    node: MemberNode
    busy: boolean
    error?: ApiError
    onTakeover: () => void
    onForget: () => void
    onOpenNode: () => void
  }
  let { node, busy, error, onTakeover, onForget, onOpenNode }: Props = $props()

  const sinkless = $derived(isSinkless(node))
  const masterNoAudio = $derived(isMasterNoAudio(node))
</script>

<tr class:offline={!node.online} class:busy>
  <td class="node">
    <span class="name">{node.name || node.id}</span>
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
  <td class="mono addrs">{node.addrs.join(', ') || '—'}</td>
  <td class="status">
    {#if node.online}
      <span class="online">● online</span>
    {:else}
      <span class="offline-wrap">⌁ <OfflineChip /></span>
    {/if}
  </td>
  <td class="actions">
    <div class="action-row">
      <Button variant="ghost" onclick={onOpenNode} disabled={busy}>Node</Button>
      <Button variant="ghost" onclick={onTakeover} disabled={busy} loading={busy}>
        Takeover
      </Button>
      <Button variant="danger" onclick={onForget} disabled={busy}>Forget</Button>
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
    color: var(--text-dim);
    vertical-align: top;
  }
  .node {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .name {
    color: var(--text);
    font-weight: 500;
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
