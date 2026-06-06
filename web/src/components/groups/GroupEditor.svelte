<script lang="ts">
  // Right pane of the Groups screen (09 §5): rename, master indicator, member
  // transfer, profile block, transport radio, media summary, Play/Stop. Owns the
  // group-mutating calls (rename/profile/transport/move/play/stop) threading
  // If-Match from configVersion; a 409 surfaces the reload-&-reapply prompt
  // (never a silent overwrite, 09 §0). Reads live status + nodes from the stores.
  // Pattern: orchestration mirrors Cluster.svelte's handler shape. NEW body.
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import Card from '../ui/Card.svelte'
  import Button from '../ui/Button.svelte'
  import Field from '../ui/Field.svelte'
  import ErrorBanner from '../state/ErrorBanner.svelte'
  import PlayStateBadge from '../dashboard/PlayStateBadge.svelte'
  import MasterBadge from '../common/MasterBadge.svelte'
  import ProfileBlock from './ProfileBlock.svelte'
  import TransportRadio from './TransportRadio.svelte'
  import MemberTransfer from './MemberTransfer.svelte'
  import { pollGroupStatus, groupStatus } from '../../lib/groupStatus'
  import { groups, nodeById, configVersion } from '../../lib/groups'
  import {
    patchGroup,
    playGroup,
    stopGroup,
    moveNode,
    ApiError,
  } from '../../lib/groupActions'
  import { pushToast } from '../../lib/toast'
  import type {
    GroupRecord,
    NodeRecord,
    CodecName,
    FECName,
    Transport,
  } from '../../lib/types'

  interface Props {
    group: GroupRecord
    onReload: () => void
  }
  let { group, onReload }: Props = $props()

  const status = $derived($groupStatus.get(group.id))
  const playing = $derived(status?.playing ?? group.playing)
  const hasMedia = $derived(!!group.media?.file)
  const masterNode = $derived(
    status ? $nodeById.get(status.masterNodeId) : undefined,
  )

  // Local rename buffer; synced from the group name (the parent also remounts
  // this editor per group via {#key}, so the effect mainly handles in-place
  // snapshot refreshes after a successful write).
  let nameDraft = $state('')
  let transport = $state<Transport>('udp')
  let busy = $state(false)
  let actionErr = $state<{ code: string; message: string } | null>(null)
  let conflict = $state(false)

  $effect(() => {
    nameDraft = group.name
  })

  // The pool for MemberTransfer: every member node NOT in this group, tagged
  // with its current group (or Unassigned).
  const pool = $derived(buildPool())
  function buildPool() {
    const here = new Set(group.memberNodeIds)
    const groupOf = new Map<string, GroupRecord>()
    for (const g of $groups) for (const id of g.memberNodeIds) groupOf.set(id, g)
    const out: {
      node: NodeRecord
      fromGroupId: string | null
      fromGroupName: string
    }[] = []
    for (const node of $nodeById.values()) {
      if (here.has(node.id)) continue
      const g = groupOf.get(node.id)
      out.push({
        node,
        fromGroupId: g?.id ?? null,
        fromGroupName: g ? g.name || g.id : 'Unassigned',
      })
    }
    return out
  }

  onMount(() => pollGroupStatus(group.id))

  function ifMatch(): number | null {
    const v = get(configVersion)
    if (v === undefined) {
      pushToast('Config version unknown — reload the screen.', 'error')
      return null
    }
    return v
  }

  // run wraps a mutating call with busy/error + 409 handling; on success it
  // updates configVersion and asks the parent to reload the snapshot.
  async function run(fn: (v: number) => Promise<number>) {
    const v = ifMatch()
    if (v === null) return
    busy = true
    actionErr = null
    try {
      const newVersion = await fn(v)
      configVersion.set(newVersion)
      onReload()
    } catch (e) {
      if (e instanceof ApiError) {
        actionErr = { code: e.code, message: e.message }
        if (e.code === 'version_conflict') conflict = true
        else if (e.code === 'proxy_failed')
          pushToast('A member must be reachable to apply this.', 'error')
      } else {
        actionErr = { code: 'unreachable', message: 'Cannot reach this player.' }
      }
    } finally {
      busy = false
    }
  }

  function saveName() {
    const next = nameDraft.trim()
    if (!next || next === group.name) return
    void run(async (v) => (await patchGroup(group.id, { name: next }, v)).version)
  }

  function setCodec(c: CodecName | null) {
    if (c === null) return // revert-to-auto: no PATCH needed (engine re-negotiates)
    void run(async (v) => (await patchGroup(group.id, { profile: { codec: c } }, v)).version)
  }
  function setFec(f: FECName | null) {
    if (f === null) return
    void run(async (v) => (await patchGroup(group.id, { profile: { fec: f } }, v)).version)
  }

  function setTransport(t: Transport) {
    // 08 §0.7 GroupRecord carries no transport field; record the operator's
    // choice locally. When the engine exposes a transport knob this becomes a
    // patchGroup call. For now it is a no-op write avoided to prevent a 422.
    transport = t
  }

  function moveOut(nodeId: string) {
    // Moving a node out of this group requires a target. With no explicit
    // destination the engine drops it to a solo group; the MVP move-out simply
    // removes it here and lets the server re-home it (a follow-up move-in places
    // it). We PATCH this group's members only.
    const next = group.memberNodeIds.filter((id) => id !== nodeId)
    void run(async (v) => (await patchGroup(group.id, { memberNodeIds: next }, v)).version)
  }

  function moveIn(nodeId: string, fromGroupId: string | null) {
    const nextHere = [...group.memberNodeIds, nodeId]
    if (fromGroupId === null) {
      // Unassigned → just add here (the node had no explicit group).
      void run(async (v) => (await patchGroup(group.id, { memberNodeIds: nextHere }, v)).version)
      return
    }
    // Transactional two-group move: remove from source, add here (08 §E.4 x2).
    const src = $groups.find((g) => g.id === fromGroupId)
    const fromMembers = src?.memberNodeIds ?? []
    void run((v) =>
      moveNode(nodeId, fromGroupId, group.id, fromMembers, group.memberNodeIds, v),
    )
  }

  function onPlay() {
    void run(async (v) => (await playGroup(group.id, v)).version)
  }
  function onStop() {
    void run(async (v) => (await stopGroup(group.id, v)).version)
  }
</script>

<Card>
  {#snippet actions()}
    <div class="head-actions">
      <PlayStateBadge {playing} />
      {#if masterNode}
        <MasterBadge node={masterNode} />
      {/if}
    </div>
  {/snippet}

  <div class="editor">
    {#if conflict}
      <ErrorBanner
        code="version_conflict"
        message="This group changed since you loaded it. Reload and reapply."
        onReloadReapply={() => {
          conflict = false
          onReload()
        }}
      />
    {:else if actionErr}
      <ErrorBanner code={actionErr.code} message={actionErr.message} />
    {/if}

    <!-- Rename -->
    <Field label="Group name" id="group-name">
      <div class="name-row">
        <input
          id="group-name"
          type="text"
          bind:value={nameDraft}
          disabled={busy}
          onblur={saveName}
          onkeydown={(e) => e.key === 'Enter' && saveName()}
        />
      </div>
    </Field>

    <!-- Membership -->
    <MemberTransfer
      {group}
      nodes={$nodeById}
      {pool}
      disabled={busy}
      onMoveOut={moveOut}
      onMoveIn={moveIn}
    />

    <!-- Profile + transport -->
    <div class="cols">
      <ProfileBlock {group} {status} nodes={$nodeById} disabled={busy} onCodec={setCodec} onFec={setFec} />
      <TransportRadio value={transport} disabled={busy} onChange={setTransport} />
    </div>

    <!-- Media summary + play/stop -->
    <div class="media">
      <div class="media-info">
        <span class="media-label">Media</span>
        {#if hasMedia}
          <span class="file">{group.media.file}</span>
          {#if group.media.loop}<span class="loop">loop</span>{/if}
        {:else}
          <span class="nomedia">none selected — choose one in Media</span>
        {/if}
      </div>
      <div class="play">
        {#if playing}
          <Button variant="ghost" loading={busy} onclick={onStop}>◼ Stop</Button>
        {:else}
          <Button variant="primary" loading={busy} disabled={!hasMedia} onclick={onPlay}>
            ▶ Play
          </Button>
        {/if}
      </div>
    </div>
  </div>
</Card>

<style>
  .head-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .editor {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .name-row input {
    width: 100%;
    padding: 0.5rem 0.65rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--surface-2);
    color: var(--text);
    font: inherit;
  }
  .name-row input:focus {
    outline: none;
    border-color: var(--accent);
    box-shadow: 0 0 0 3px var(--focus-ring);
  }
  .cols {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-3);
    align-items: start;
  }
  .media {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
  }
  .media-info {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .media-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
  }
  .file {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--text);
  }
  .loop {
    font-size: var(--text-xs);
    color: var(--accent-bright);
    border: 1px solid var(--accent);
    border-radius: 999px;
    padding: 0.05rem 0.4rem;
  }
  .nomedia {
    font-size: var(--text-sm);
    color: var(--text-muted);
  }
</style>
