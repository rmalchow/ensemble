<script lang="ts">
  // Groups screen (screen 5, 09 §5): a two-column shell — GroupList (left) +
  // GroupEditor (right). Pattern reuse of ../media PlaylistEditor's list-left/
  // editor-right layout ONLY; all timeline/clip/lane code dropped. Create groups
  // (NewGroupModal → POST), select one to edit, move members (transactional
  // two-group write), override profile/transport, play/stop. The synthetic
  // "Unassigned" bucket lists adopted nodes in no explicit group.
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import GroupList from '../components/groups/GroupList.svelte'
  import GroupEditor from '../components/groups/GroupEditor.svelte'
  import NewGroupModal from '../components/groups/NewGroupModal.svelte'
  import MembersTable from '../components/dashboard/MembersTable.svelte'
  import Card from '../components/ui/Card.svelte'
  import ErrorBanner from '../components/state/ErrorBanner.svelte'
  import Skeleton from '../components/state/Skeleton.svelte'
  import {
    refreshGroups,
    groups,
    groupById,
    nodeById,
    nodeLiveness,
    unassignedNodeIds,
    configVersion,
  } from '../lib/groups'
  import { createGroup, ApiError } from '../lib/groupActions'
  import { pushToast } from '../lib/toast'
  import type { GroupRecord } from '../lib/types'

  type Phase = 'loading' | 'ready' | 'error'
  let phase = $state<Phase>('loading')
  let loadErr = $state<{ code: string; message: string } | null>(null)
  let selectedId = $state<string | null>(null)

  // New-group modal state.
  let modalOpen = $state(false)
  let modalBusy = $state(false)
  let modalErr = $state<string | undefined>(undefined)

  async function load() {
    phase = 'loading'
    loadErr = null
    try {
      await refreshGroups()
      // Default selection: first group, or the Unassigned bucket if no groups.
      if (selectedId === null) {
        const first = get(groups)[0]
        selectedId = first ? first.id : get(unassignedNodeIds).length > 0 ? '__unassigned__' : null
      }
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

  const selectedGroup = $derived<GroupRecord | undefined>(
    selectedId && selectedId !== '__unassigned__'
      ? $groupById.get(selectedId)
      : undefined,
  )
  const showUnassigned = $derived(selectedId === '__unassigned__')

  // A synthetic GroupRecord so the Unassigned bucket can reuse MembersTable.
  const unassignedGroup = $derived<GroupRecord>({
    id: '__unassigned__',
    name: 'Unassigned',
    memberNodeIds: $unassignedNodeIds,
    profile: {
      codec: 'pcm',
      fec: 'none',
      rate: 48000,
      framesPerChunk: 480,
      fecK: 8,
      interleave: 4,
    },
    media: { file: '', loop: false },
    playing: false,
  })

  function openModal() {
    modalErr = undefined
    modalOpen = true
  }

  async function onCreate(name: string) {
    const v = get(configVersion)
    if (v === undefined) {
      modalErr = 'Config version unknown — reload the screen.'
      return
    }
    modalBusy = true
    modalErr = undefined
    try {
      const r = await createGroup(name, v)
      configVersion.set(r.version)
      modalOpen = false
      pushToast(`Created group “${name}”.`, 'success')
      await load()
      // Select the newly created group if the snapshot now lists it by name.
      const created = get(groups).find((g) => g.name === name)
      if (created) selectedId = created.id
    } catch (e) {
      if (e instanceof ApiError) modalErr = `${e.code}: ${e.message}`
      else modalErr = 'Cannot reach this player.'
    } finally {
      modalBusy = false
    }
  }
</script>

<div class="groups">
  {#if phase === 'error' && loadErr}
    <ErrorBanner code={loadErr.code} message={loadErr.message} onRetry={load} />
  {:else}
    <div class="layout">
      <aside class="left">
        <Card title="Groups">
          {#if phase === 'loading'}
            <Skeleton rows={3} />
          {:else}
            <GroupList
              groups={$groups}
              unassignedCount={$unassignedNodeIds.length}
              {selectedId}
              onSelect={(id) => (selectedId = id)}
              onNew={openModal}
            />
          {/if}
        </Card>
      </aside>

      <section class="right">
        {#if phase === 'loading'}
          <Card><Skeleton rows={6} /></Card>
        {:else if showUnassigned}
          <Card title="Unassigned players">
            <p class="hint">
              Adopted players not yet placed in a group. Open a group and move
              them in.
            </p>
            <MembersTable
              group={unassignedGroup}
              nodes={$nodeById}
              liveness={$nodeLiveness}
            />
          </Card>
        {:else if selectedGroup}
          {#key selectedGroup.id}
            <GroupEditor group={selectedGroup} onReload={load} />
          {/key}
        {:else}
          <Card title="No group selected">
            <p class="hint">
              Select a group on the left, or create one to start composing a
              synchronised playback zone.
            </p>
          </Card>
        {/if}
      </section>
    </div>
  {/if}
</div>

<NewGroupModal
  open={modalOpen}
  busy={modalBusy}
  error={modalErr}
  onCreate={onCreate}
  onCancel={() => (modalOpen = false)}
/>

<style>
  .groups {
    max-width: 72rem;
  }
  .layout {
    display: grid;
    grid-template-columns: 16rem 1fr;
    gap: var(--space-5);
    align-items: start;
  }
  .hint {
    color: var(--text-muted);
    font-size: var(--text-sm);
    margin: 0 0 var(--space-3);
  }
  @media (max-width: 48rem) {
    .layout {
      grid-template-columns: 1fr;
    }
  }
</style>
