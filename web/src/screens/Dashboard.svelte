<script lang="ts">
  // Dashboard (screen 3, 09 §3): the landing page. A vertical stack of GroupCards
  // (one per group, each with play state, master, negotiated profile/transport,
  // and a members table with sink-less + offline badges) plus a trailing
  // OfflineStrip. Pattern reuse of ../media ClusterOverview's topbar/connected
  // layout + dark CSS, but the device-list/detail split is replaced by the
  // group-card stack (brand/header come from the P0.4 AppShell, not here).
  //
  // Static structure (groups, members) loads via refreshGroups() — the composed
  // 08 §E.1/§D.1 reads (P0.4 builds a polling/refresh model, not a WS snapshot;
  // see P3.3 §9 open question 3). Per-card live sync comes from each GroupCard's
  // own ~1 Hz /status poll.
  import { onMount } from 'svelte'
  import GroupCard from '../components/dashboard/GroupCard.svelte'
  import OfflineStrip from '../components/dashboard/OfflineStrip.svelte'
  import ErrorBanner from '../components/state/ErrorBanner.svelte'
  import Skeleton from '../components/state/Skeleton.svelte'
  import Card from '../components/ui/Card.svelte'
  import { navigate } from '../lib/router'
  import { refreshGroups, groups, nodeById, nodeLiveness } from '../lib/groups'
  import { ApiError } from '../lib/api'
  import type { GroupRecord, NodeRecord } from '../lib/types'

  type Phase = 'loading' | 'ready' | 'error'
  let phase = $state<Phase>('loading')
  let loadErr = $state<{ code: string; message: string } | null>(null)

  async function load() {
    phase = 'loading'
    loadErr = null
    try {
      await refreshGroups()
      phase = 'ready'
    } catch (e) {
      loadErr =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
      phase = 'error'
    }
  }

  onMount(load)

  const allNodes = $derived<NodeRecord[]>([...$nodeById.values()])
  // groupByNode maps node id → its group, for the offline strip's "last in …".
  const groupByNode = $derived(
    new Map<string, GroupRecord>(
      $groups.flatMap((g) => g.memberNodeIds.map((id) => [id, g] as const)),
    ),
  )
</script>

<div class="dashboard">
  {#if phase === 'error' && loadErr}
    <ErrorBanner code={loadErr.code} message={loadErr.message} onRetry={load} />
  {:else if phase === 'loading'}
    <Card><Skeleton rows={4} /></Card>
    <Card><Skeleton rows={4} /></Card>
  {:else if $groups.length === 0}
    <Card title="No groups yet">
      <p class="empty">
        Create a group in
        <button class="link" type="button" onclick={() => navigate('/groups')}>
          Groups
        </button>
        to start playing synchronised audio.
      </p>
    </Card>
  {:else}
    {#each $groups as group (group.id)}
      <GroupCard {group} />
    {/each}
    <OfflineStrip nodes={allNodes} liveness={$nodeLiveness} {groupByNode} />
  {/if}
</div>

<style>
  .dashboard {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    max-width: 64rem;
  }
  .empty {
    color: var(--text-dim);
    font-size: var(--text-sm);
    margin: 0;
  }
  .link {
    background: none;
    border: none;
    color: var(--accent-bright);
    cursor: pointer;
    font: inherit;
    padding: 0;
  }
  .link:hover {
    text-decoration: underline;
  }
</style>
