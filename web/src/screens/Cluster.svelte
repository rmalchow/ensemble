<script lang="ts">
  // Cluster screen (09 §4): the operator surface for cluster membership. Renders
  // the CA-fingerprint header, the Discovered table (PIN-gated Adopt + Rescan),
  // and the Members table (Takeover/Forget behind the shared confirm dialog,
  // sink-less tag + master-no-audio note, online/offline treatment). It owns no
  // protocol — it orchestrates lib/cluster.ts calls + the clusterStore derived
  // views and threads If-Match from configVersion. On a 409 version_conflict it
  // reloads + asks the operator to reapply (never a silent overwrite, 09 §0).
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import Card from '../components/ui/Card.svelte'
  import ErrorBanner from '../components/state/ErrorBanner.svelte'
  import CaFingerprintHeader from '../components/cluster/CaFingerprintHeader.svelte'
  import DiscoveredList from '../components/cluster/DiscoveredList.svelte'
  import DiscoveredRow from '../components/cluster/DiscoveredRow.svelte'
  import MembersList from '../components/cluster/MembersList.svelte'
  import MemberRow from '../components/cluster/MemberRow.svelte'
  import { confirmAction } from '../lib/confirm'
  import { navigate } from '../lib/router'
  import { pushToast } from '../lib/toast'
  import {
    adopt,
    takeover,
    forget,
    ApiError,
    type DiscoveredNode,
    type MemberNode,
  } from '../lib/cluster'
  import {
    refreshCluster,
    caFingerprint,
    clusterName,
    configVersion,
    discovered,
    members,
    rowState,
    setRowBusy,
    setRowError,
  } from '../lib/clusterStore'

  // Screen-level load state. The two tables share a single initial load but each
  // renders its own skeleton while data is in flight (per-table loading, §9.4).
  type Phase = 'loading' | 'ready' | 'error'
  let phase = $state<Phase>('loading')
  let loadErr = $state<{ code: string; message: string } | null>(null)
  // conflict drives the reload-&-reapply prompt after a 409 on a write (09 §0).
  let conflict = $state(false)

  async function load() {
    phase = 'loading'
    loadErr = null
    try {
      await refreshCluster()
      phase = 'ready'
    } catch (e) {
      loadErr =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
      phase = 'error'
    }
  }

  onMount(() => {
    void load()
    // Near-realtime: quiet 5s refresh while the screen is open (liveness flips,
    // freshly discovered/adopted nodes) — no loading flicker, errors keep the
    // last good data (the next write/load surfaces a persistent failure).
    const t = setInterval(() => {
      if (phase === 'ready') void refreshCluster().catch(() => {})
    }, 5000)
    return () => clearInterval(t)
  })

  // ifMatch reads the freshest config version; a missing version is a guard
  // (412 would otherwise fire) — apiFetch attaches it from this value.
  function ifMatch(): number | null {
    const v = get(configVersion)
    if (v === undefined) {
      pushToast('Config version unknown — reload the screen.', 'error')
      return null
    }
    return v
  }

  // handleWriteError centralises the §0.5 / §4 error policy: a 409 flips the
  // reload-&-reapply prompt; everything else lands inline on the row (and a 502
  // gets a clarifying toast about reachability).
  function handleWriteError(id: string, e: unknown) {
    if (e instanceof ApiError) {
      setRowError(id, e)
      if (e.code === 'version_conflict') conflict = true
      else if (e.code === 'proxy_failed')
        pushToast('Target node must be reachable to complete this.', 'error')
    } else {
      setRowError(
        id,
        new ApiError(0, 'unreachable', 'Cannot reach this player.'),
      )
    }
  }

  async function onAdopt(node: DiscoveredNode, pin: string) {
    const v = ifMatch()
    if (v === null) return
    setRowBusy(node.nodeId, true)
    try {
      const res = await adopt(
        {
          nodeId: node.nodeId,
          addr: node.addrs[0] ?? '',
          fingerprint: node.fingerprint,
          pin,
          // Carry the mDNS-advertised friendly name into the NodeRecord so the
          // members table shows it instead of a bare id.
          name: node.name,
        },
        v,
      )
      configVersion.set(res.version)
      pushToast(`Adopted ${res.node.name || node.nodeId}.`, 'success')
      await load()
    } catch (e) {
      handleWriteError(node.nodeId, e)
    } finally {
      setRowBusy(node.nodeId, false)
    }
  }

  // Takeover (C.4): a FOREIGN discovered node is moved into this cluster by
  // presenting its CURRENT cluster's admin password (03 §4) — the row collects
  // the password, so no extra confirm dialog.
  async function onTakeover(node: DiscoveredNode, password: string) {
    const v = ifMatch()
    if (v === null) return
    setRowBusy(node.nodeId, true)
    try {
      const res = await takeover(
        {
          nodeId: node.nodeId,
          addr: node.addrs[0] ?? '',
          fingerprint: node.fingerprint ?? '',
          pin: '0000',
          password,
          name: node.name,
          force: true,
        },
        v,
      )
      configVersion.set(res.version)
      pushToast(`Took over ${res.node.name || node.nodeId}.`, 'success')
      await load()
    } catch (e) {
      handleWriteError(node.nodeId, e)
    } finally {
      setRowBusy(node.nodeId, false)
    }
  }

  async function onForget(node: MemberNode) {
    const label = node.name || node.id
    const ok = await confirmAction({
      type: 'forget',
      title: 'Forget node',
      message: `Forget ${label}? Revokes its cert and drops it from config + allowlist.`,
      confirmLabel: 'Forget',
      danger: true,
    })
    if (!ok) return
    const v = ifMatch()
    if (v === null) return
    setRowBusy(node.id, true)
    try {
      const res = await forget(node.id, v)
      configVersion.set(res.version)
      if (res.affectedGroups.length > 0) {
        pushToast(
          `Forgot ${label}; updated groups: ${res.affectedGroups.join(', ')}.`,
          'info',
        )
      } else {
        pushToast(`Forgot ${label}.`, 'success')
      }
      await load()
    } catch (e) {
      handleWriteError(node.id, e)
    } finally {
      setRowBusy(node.id, false)
    }
  }

  function openNode(id: string) {
    navigate(`/nodes/${encodeURIComponent(id)}`)
  }

  const tablesLoading = $derived(phase === 'loading')
</script>

<div class="cluster">
  <CaFingerprintHeader name={$clusterName} fingerprint={$caFingerprint} />

  {#if phase === 'error' && loadErr}
    <ErrorBanner code={loadErr.code} message={loadErr.message} onRetry={load} />
  {:else}
    {#if conflict}
      <ErrorBanner
        code="version_conflict"
        message="The cluster config changed since you loaded it. Reload and reapply."
        onReloadReapply={() => {
          conflict = false
          void load()
        }}
      />
    {/if}

    <Card>
      <DiscoveredList
        nodes={$discovered}
        loading={tablesLoading}
        onRescan={load}
      >
        {#snippet row(node: DiscoveredNode)}
          <DiscoveredRow
            {node}
            busy={$rowState[node.nodeId]?.busy ?? false}
            error={$rowState[node.nodeId]?.error}
            onAdopt={(pin) => onAdopt(node, pin)}
            onTakeover={(password) => onTakeover(node, password)}
          />
        {/snippet}
      </DiscoveredList>
    </Card>

    <Card>
      <MembersList nodes={$members} loading={tablesLoading}>
        {#snippet row(node: MemberNode)}
          <MemberRow
            {node}
            busy={$rowState[node.id]?.busy ?? false}
            error={$rowState[node.id]?.error}
            onForget={() => onForget(node)}
            onOpenNode={() => openNode(node.id)}
          />
        {/snippet}
      </MembersList>
    </Card>
  {/if}
</div>

<style>
  .cluster {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    max-width: 64rem;
  }
</style>
