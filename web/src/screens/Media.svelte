<script lang="ts">
  // Media screen (09 §7): scope switcher → data/ file list → now-playing bar.
  // Reuses the two-zone panel layout + header-with-scope-dropdown styling from
  // ../media ClusterDetail (chrome only); the content is the audio-group flow.
  //
  // Flow: pick a GROUP (media plays on its MASTER — D5 master-side decode) or a
  // NODE scope → browse the master's data/ (F.1, proxied to the master) → per-row
  // Select & play (F.3 {file, loop}) / Stop (F.4); the loop toggle persists via
  // F.2 when stopped or rides the next F.3 when playing. Selection persists in
  // ConfigDoc so Dashboard/Groups reflect it. Live now-playing comes from the
  // per-group ~1 Hz /status poll (G.2). All commands proxy to the master.
  import { onMount, onDestroy } from 'svelte'
  import { get } from 'svelte/store'
  import Card from '../components/ui/Card.svelte'
  import Button from '../components/ui/Button.svelte'
  import Skeleton from '../components/state/Skeleton.svelte'
  import EmptyState from '../components/state/EmptyState.svelte'
  import ErrorBanner from '../components/state/ErrorBanner.svelte'
  import OfflineChip from '../components/state/OfflineChip.svelte'
  import ScopeSwitcher from '../components/media/ScopeSwitcher.svelte'
  import FileList from '../components/media/FileList.svelte'
  import NowPlaying from '../components/media/NowPlaying.svelte'
  import { navigate } from '../lib/router'
  import { refreshGroups, groups, groupById, nodeById, configVersion } from '../lib/groups'
  import { pollGroupStatus, groupStatus } from '../lib/groupStatus'
  import { listMedia, selectAndPlay, setMedia, stop, type MediaFile } from '../lib/media'
  import { ApiError } from '../lib/api'
  import { pushToast } from '../lib/toast'
  import type { GroupRecord, NodeRecord } from '../lib/types'

  type Scope = { kind: 'group'; id: string } | { kind: 'node'; id: string }

  // ---- Scope from the route query (?group=<id> / ?node=<id>) ----------------
  function readScope(): Scope | null {
    const q = typeof location !== 'undefined' ? new URLSearchParams(location.search) : new URLSearchParams()
    const g = q.get('group')
    if (g) return { kind: 'group', id: g }
    const n = q.get('node')
    if (n) return { kind: 'node', id: n }
    return null
  }

  let scope = $state<Scope | null>(readScope())

  // ---- Static structure load (groups + nodes) -------------------------------
  type Phase = 'loading' | 'ready' | 'error'
  let phase = $state<Phase>('loading')
  let loadErr = $state<{ code: string; message: string } | null>(null)

  async function loadStructure() {
    phase = 'loading'
    loadErr = null
    try {
      await refreshGroups()
      // Default scope: the first group (most common path). Persist to the query.
      if (scope === null) {
        const first = get(groups)[0]
        if (first) setScope({ kind: 'group', id: first.id }, true)
      }
      phase = 'ready'
    } catch (e) {
      loadErr = toErr(e)
      phase = 'error'
    }
  }

  function setScope(next: Scope, replace = false) {
    scope = next
    const q = next.kind === 'group' ? `?group=${encodeURIComponent(next.id)}` : `?node=${encodeURIComponent(next.id)}`
    navigate(`/media${q}`, replace)
  }

  // ---- Scope resolution → master node + target group ------------------------
  const allGroups = $derived<GroupRecord[]>($groups)
  const nodesMap = $derived<Map<string, NodeRecord>>($nodeById)
  const nodesByIdObj = $derived<Record<string, NodeRecord>>(
    Object.fromEntries(nodesMap.entries()),
  )

  // The group the commands target: the scoped group, or the scoped node's group.
  const targetGroup = $derived<GroupRecord | undefined>(resolveGroup())
  function resolveGroup(): GroupRecord | undefined {
    if (!scope) return undefined
    if (scope.kind === 'group') return $groupById.get(scope.id)
    // Node scope: find the group that contains the node (a node is in exactly
    // one group, README §2) so play/stop still target a group.
    return allGroups.find((g) => g.memberNodeIds.includes(scope!.id))
  }

  // The live status drives both the now-playing cursor and the elected master.
  const status = $derived(targetGroup ? $groupStatus.get(targetGroup.id) : undefined)

  // Master node whose data/ is browsed. Prefer the LIVE elected master from
  // status; fall back to the group's first member when status has not landed.
  const masterNodeId = $derived<string | undefined>(
    scope?.kind === 'node'
      ? scope.id
      : (status?.masterNodeId ?? targetGroup?.memberNodeIds[0]),
  )
  const masterNode = $derived<NodeRecord | undefined>(
    masterNodeId ? nodesMap.get(masterNodeId) : undefined,
  )
  const masterLabel = $derived(
    masterNode ? `data/ on ${masterNode.name || masterNode.id}` : 'data/',
  )

  // ---- data/ listing (F.1) --------------------------------------------------
  type ListPhase = 'idle' | 'loading' | 'ready' | 'empty' | 'error' | 'offline'
  let listPhase = $state<ListPhase>('idle')
  let listErr = $state<{ code: string; message: string } | null>(null)
  let files = $state<MediaFile[]>([])
  let dirs = $state<string[]>([])
  let browsePath = $state('') // data/-relative folder being browsed ("" = root)
  let listedFor = $state<string | null>(null) // masterNodeId the list belongs to

  async function loadFiles(nodeId: string | undefined, path = browsePath) {
    if (!nodeId) return
    listPhase = 'loading'
    listErr = null
    try {
      const r = await listMedia(nodeId, path)
      files = r.data.files ?? []
      dirs = r.data.dirs ?? []
      browsePath = r.data.path ?? path
      listedFor = nodeId
      listPhase = files.length === 0 && dirs.length === 0 && !browsePath ? 'empty' : 'ready'
    } catch (e) {
      const err = toErr(e)
      // A 502 proxy_failed means the scoped master is unreachable → offline state
      // (show last-selected media dimmed, browse/play disabled).
      if (e instanceof ApiError && e.status === 502) {
        listPhase = 'offline'
      } else {
        listErr = err
        listPhase = 'error'
      }
    }
  }

  // Enter a subfolder (or ".." back up) of the master's data/ tree.
  function openDir(path: string) {
    browsePath = path
    void loadFiles(masterNodeId, path)
  }

  // Reload the listing whenever the resolved master changes (back at the root —
  // the browse path belongs to the previous master's tree).
  $effect(() => {
    if (phase !== 'ready') return
    const id = masterNodeId
    if (id && id !== listedFor) {
      browsePath = ''
      void loadFiles(id, '')
    }
  })

  // ---- Live poll lifecycle (now-playing position / playing flag) ------------
  // One ~1 Hz /status subscription for the currently-targeted group; swapped
  // when the scope changes group. Dedup'd by groupStatus.ts.
  let disposePoll: (() => void) | null = null
  let polledGroupId: string | null = null
  $effect(() => {
    const gid = targetGroup?.id ?? null
    if (gid === polledGroupId) return
    disposePoll?.()
    disposePoll = gid ? pollGroupStatus(gid) : null
    polledGroupId = gid
  })
  onDestroy(() => disposePoll?.())

  onMount(loadStructure)

  // ---- Derived view facts ---------------------------------------------------
  const selectedFile = $derived(targetGroup?.media?.file || null)
  const loop = $derived(targetGroup?.media?.loop ?? true) // D14: default loop on
  const playing = $derived(status?.playing ?? targetGroup?.playing ?? false)
  const selectedMeta = $derived<MediaFile | undefined>(
    files.find((f) => f.file === selectedFile),
  )
  const lengthSec = $derived(
    selectedMeta?.durationMs ? selectedMeta.durationMs / 1000 : undefined,
  )
  // 08 G.2 exposes no play cursor; positionSec stays undefined until a backend
  // field appears (P4.10 §9 risk 2). The bar degrades to file + loop + length.
  const positionSec = $derived<number | undefined>(deriveStatusPosition())
  function deriveStatusPosition(): number | undefined {
    const s = status as unknown as { positionSamples?: number; positionSec?: number } | undefined
    if (!s) return undefined
    if (typeof s.positionSec === 'number') return s.positionSec
    if (typeof s.positionSamples === 'number') return s.positionSamples / 48000 // A.12 canonical rate
    return undefined
  }

  let busyFile = $state<string | null>(null)
  let cmdErr = $state<{ code: string; message: string } | null>(null)

  function ifMatch(): number | null {
    const v = get(configVersion)
    if (v === undefined) {
      pushToast('Config version unknown — reload the screen.', 'error')
      return null
    }
    return v
  }

  async function onPlay(file: string) {
    if (!targetGroup) return
    const v = ifMatch()
    if (v === null) return
    busyFile = file
    cmdErr = null
    try {
      // Master-follows-source: the node whose library is being browsed
      // (listedFor) becomes the group's master and decodes the file locally.
      const r = await selectAndPlay(targetGroup.id, file, loop, v, listedFor ?? undefined)
      configVersion.set(r.data.version)
      // Reflect the selection locally so the row flips before the next poll.
      await refreshGroups()
    } catch (e) {
      handleCmd(e)
    } finally {
      busyFile = null
    }
  }

  async function onStop() {
    if (!targetGroup || !selectedFile) return
    const v = ifMatch()
    if (v === null) return
    busyFile = selectedFile
    cmdErr = null
    try {
      const r = await stop(targetGroup.id, v)
      configVersion.set(r.data.version)
      await refreshGroups()
    } catch (e) {
      handleCmd(e)
    } finally {
      busyFile = null
    }
  }

  // Loop toggle: while playing it rides the next select-and-play (re-issue F.3
  // with the same file); while stopped it persists via F.2 without starting.
  async function onToggleLoop(next: boolean) {
    if (!targetGroup || !selectedFile) return
    const v = ifMatch()
    if (v === null) return
    cmdErr = null
    try {
      const r = playing
        ? await selectAndPlay(targetGroup.id, selectedFile, next, v, listedFor ?? undefined)
        : await setMedia(targetGroup.id, selectedFile, next, v, listedFor ?? undefined)
      configVersion.set(r.data.version)
      await refreshGroups()
    } catch (e) {
      handleCmd(e)
    }
  }

  function handleCmd(e: unknown) {
    cmdErr = toErr(e)
    if (e instanceof ApiError && e.code === 'proxy_failed') {
      pushToast('The group master must be reachable to play/stop.', 'error')
    }
  }

  function toErr(e: unknown): { code: string; message: string } {
    return e instanceof ApiError
      ? { code: e.code, message: e.message }
      : { code: 'unreachable', message: 'Cannot reach this player.' }
  }

  const groupName = $derived(targetGroup?.name || targetGroup?.id || '—')
</script>

<div class="media">
  {#if phase === 'error' && loadErr}
    <ErrorBanner code={loadErr.code} message={loadErr.message} onRetry={loadStructure} />
  {:else if phase === 'loading'}
    <Card><Skeleton rows={5} /></Card>
  {:else if allGroups.length === 0}
    <Card title="No groups yet">
      <EmptyState
        title="No groups to play to"
        description="Media plays on a group's master. Create a group first, then return here to browse and play."
      >
        {#snippet cta()}
          <Button variant="primary" onclick={() => navigate('/groups')}>Go to Groups</Button>
        {/snippet}
      </EmptyState>
    </Card>
  {:else if scope && targetGroup === undefined && scope.kind === 'group'}
    <Card title="Group not found">
      <ErrorBanner
        code="not_found"
        message="That group is no longer in the cluster. Pick another scope."
        onRetry={loadStructure}
      />
    </Card>
  {:else}
    <Card>
      {#snippet actions()}
        <div class="head-actions">
          {#if scope}
            <ScopeSwitcher
              groups={allGroups}
              nodesById={nodesByIdObj}
              {scope}
              onChange={(s) => setScope(s)}
            />
          {/if}
          <Button
            variant="ghost"
            disabled={listPhase === 'loading' || !masterNodeId}
            onclick={() => loadFiles(masterNodeId)}
          >
            ⟳ Refresh
          </Button>
        </div>
      {/snippet}

      <div class="panel">
        {#if cmdErr}
          <ErrorBanner
            code={cmdErr.code}
            message={cmdErr.message}
            onReloadReapply={cmdErr.code === 'version_conflict' ? loadStructure : undefined}
          />
        {/if}

        {#if listPhase === 'loading' || listPhase === 'idle'}
          <p class="src">{masterLabel}</p>
          <Skeleton rows={4} />
        {:else if listPhase === 'offline'}
          <div class="offline-head">
            <span class="src">{masterLabel}</span>
            <OfflineChip variant="offline" />
          </div>
          <p class="banner-msg">
            The scoped master is unreachable. Showing the last-selected media; browse and
            playback are disabled until it comes back online.
          </p>
          {#if selectedFile}
            <FileList
              files={[{ file: selectedFile, durationMs: selectedMeta?.durationMs }]}
              masterNodeName={masterLabel}
              {selectedFile}
              {playing}
              {loop}
              disabled={true}
              onPlay={() => {}}
              onStop={() => {}}
              onToggleLoop={() => {}}
            />
          {/if}
        {:else if listPhase === 'error' && listErr}
          <ErrorBanner code={listErr.code} message={listErr.message} onRetry={() => loadFiles(masterNodeId)} />
        {:else if listPhase === 'empty'}
          <EmptyState
            title="No media in {masterLabel}"
            description="Drop .mp3 files into this node's data/ folder, then refresh. The folder is per-node."
          >
            {#snippet cta()}
              <Button variant="ghost" onclick={() => loadFiles(masterNodeId)}>⟳ Refresh</Button>
            {/snippet}
          </EmptyState>
        {:else}
          <FileList
            {files}
            {dirs}
            path={browsePath}
            onOpenDir={openDir}
            masterNodeName={masterLabel}
            {selectedFile}
            {playing}
            {loop}
            {busyFile}
            onPlay={onPlay}
            onStop={onStop}
            onToggleLoop={onToggleLoop}
          />
        {/if}
      </div>

      <NowPlaying
        file={selectedFile}
        {loop}
        {groupName}
        {positionSec}
        {lengthSec}
      />
    </Card>
  {/if}
</div>

<style>
  .media {
    max-width: 64rem;
  }
  .head-actions {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .src {
    font-size: var(--text-sm);
    color: var(--text-dim);
    margin: 0;
  }
  .offline-head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .banner-msg {
    font-size: var(--text-sm);
    color: var(--text-muted);
    margin: 0;
  }
</style>
