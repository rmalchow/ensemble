<script lang="ts">
  // Settings → cluster info (09 §8). Name / created / node counts (online·total)
  // + the CA fingerprint (the trust anchor, also shown on the Cluster header) in
  // a CopyField. Reads C.1; seeds configVersion from the ETag. Renders from the
  // gossiped ConfigDoc, so it still works while peers are offline.
  import { onMount } from 'svelte'
  import { clusterInfoFull, ApiError, type ClusterInfoBody } from '../../lib/api'
  import { configVersion, clusterInfo as clusterInfoStore } from '../../lib/stores'
  import { fmtFingerprint, fmtDate } from '../../lib/format'
  import CopyField from '../CopyField.svelte'
  import StateMachine from '../state/StateMachine.svelte'

  type DataState = 'loading' | 'ready' | 'error'
  let dataState = $state<DataState>('loading')
  let cluster = $state<ClusterInfoBody | null>(null)
  let counts = $state<{ nodes: number; groups: number } | null>(null)
  let err = $state<{ code: string; message: string } | null>(null)

  async function load() {
    dataState = 'loading'
    err = null
    try {
      const { data } = await clusterInfoFull()
      cluster = data.cluster
      counts = data.counts
      configVersion.set(data.version)
      clusterInfoStore.set({
        name: data.cluster.name,
        caFingerprint: data.cluster.caFingerprint,
      })
      dataState = 'ready'
    } catch (e) {
      err =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
      dataState = 'error'
    }
  }

  onMount(load)
</script>

<StateMachine state={dataState} error={err ?? undefined} onRetry={load}>
  <div class="grid">
    <div class="row"><span class="k">Name</span><span class="v">{cluster?.name}</span></div>
    <div class="row">
      <span class="k">Created</span><span class="v mono">{fmtDate(cluster?.created)}</span>
    </div>
    <div class="row">
      <span class="k">Nodes</span><span class="v">{counts?.nodes ?? '—'}</span>
    </div>
    <div class="row">
      <span class="k">Groups</span><span class="v">{counts?.groups ?? '—'}</span>
    </div>
  </div>
  <div class="ca">
    <CopyField label="CA fingerprint" value={fmtFingerprint(cluster?.caFingerprint)} />
  </div>
</StateMachine>

<style>
  .grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-2) var(--space-5);
    margin-bottom: var(--space-4);
    max-width: 28rem;
  }
  .row {
    display: contents;
  }
  .k {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .v {
    color: var(--text);
    font-size: var(--text-sm);
  }
  .ca {
    max-width: 32rem;
  }
</style>
