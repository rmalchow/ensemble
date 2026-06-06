<script lang="ts">
  // Node detail — screen 6 (09 §6). Registered at /nodes/:id (P1.4 router). Pure
  // presentation + action wiring over P6.2's node-config / capability-mask /
  // calibration endpoints; it owns no protocol/config-write logic of its own.
  //
  // Layout (09 §6 wireframe): Header · Identity · conditional Audio-output (A:
  // channel/gain/HWDelay+calibration when render; B: control-media-only panel
  // when sink-less) · always-shown Capabilities & audio backends · Network ·
  // Save/Revert (If-Match). States: loading (skeleton, controls disabled) · error
  // (not-found → back to Cluster) · offline (read-only, rename labelled "applies
  // when node returns") · sink-less (content variant B) · 409 (reload & reapply,
  // never silent overwrite).
  //
  // The screen prefers the live cluster/groups snapshot to ENRICH the record with
  // online/groupId/isMaster/fingerprint (the D.2 record alone carries config, not
  // liveness/election), then falls back to a one-shot getNode on mount and after
  // each save (P6.3 §9 risk 3).
  import { onMount } from 'svelte'
  import Card from '../components/ui/Card.svelte'
  import Skeleton from '../components/state/Skeleton.svelte'
  import Banner from '../components/Banner.svelte'
  import ProxyHint from '../components/cluster/ProxyHint.svelte'
  import NodeDetailHeader from '../components/node/NodeDetailHeader.svelte'
  import IdentityPanel from '../components/node/IdentityPanel.svelte'
  import AudioOutputSection from '../components/node/AudioOutputSection.svelte'
  import CapabilitiesPanel from '../components/node/CapabilitiesPanel.svelte'
  import NetworkPanel from '../components/node/NetworkPanel.svelte'
  import SaveBar from '../components/node/SaveBar.svelte'
  import { navigate } from '../lib/router'
  import { pushToast } from '../lib/toast'
  import { session } from '../lib/stores'
  import { members, refreshCluster } from '../lib/clusterStore'
  import { groups, refreshGroups } from '../lib/groups'
  import {
    loadNode,
    setLoaded,
    mergeLoaded,
    loaded,
    draft,
    isDirty,
    isRenderEnabled,
    liveSyncErrorUs,
    offline,
    save,
    revert,
  } from '../lib/nodeStore'
  import { getNode, ApiError, type NodeDetailView } from '../lib/node'

  interface Props {
    id: string
  }
  let { id }: Props = $props()

  type Phase = 'loading' | 'ready' | 'error'
  let phase = $state<Phase>('loading')
  let loadErr = $state<{ code: string; message: string } | null>(null)
  let saveErr = $state<{ code: string; message: string } | null>(null)
  let saving = $state(false)

  // enrich joins the raw D.2 record with the live cluster/groups snapshot:
  // online from the gossiped members[], groupId/isMaster from the group view.
  function enrich(node: NodeDetailView): NodeDetailView {
    const m = $members.find((x) => x.id === node.id)
    const g = $groups.find((x) => x.memberNodeIds.includes(node.id))
    return {
      ...node,
      online: node.online ?? m?.online,
      fingerprint: node.fingerprint ?? m?.fingerprint,
      groupId: node.groupId ?? g?.id ?? m?.groupId,
      isMaster: node.isMaster ?? m?.isMaster,
    }
  }

  // disposed gates every async store write so a load/refresh resolving after the
  // screen unmounts (route change) never clobbers the next screen's shared store.
  let disposed = false

  async function load() {
    phase = 'loading'
    loadErr = null
    saveErr = null
    try {
      const { node } = await getNode(id)
      if (disposed) return
      setLoaded(enrich(node))
      phase = 'ready'
    } catch (e) {
      if (disposed) return
      loadErr =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this node.' }
      phase = 'error'
    }
  }

  // Use loadNode (which also re-seeds configVersion) for the canonical mount path.
  async function mountLoad() {
    phase = 'loading'
    loadErr = null
    try {
      await loadNode(id)
      if (disposed) return
      // Enrich the just-loaded record with live liveness/election.
      const cur = $loaded
      if (cur) setLoaded(enrich(cur))
      phase = 'ready'
    } catch (e) {
      if (disposed) return
      loadErr =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this node.' }
      phase = 'error'
    }
  }

  onMount(() => {
    disposed = false
    void mountLoad()
    // Warm the live cluster/group snapshots so enrich() can join liveness +
    // group/master state when the screen is reached by deep-link. Non-fatal: the
    // record renders from D.2 alone if these reads fail.
    void refreshCluster().then(reEnrich).catch(() => {})
    void refreshGroups().then(reEnrich).catch(() => {})
    return () => {
      disposed = true
    }
  })

  // reEnrich re-folds the freshly-loaded live snapshot into the loaded record.
  // It uses mergeLoaded (not setLoaded) so a pending draft is never clobbered by
  // a background liveness/group read landing mid-edit.
  function reEnrich() {
    if (disposed) return
    const cur = $loaded
    if (cur) mergeLoaded(enrich(cur))
  }

  async function onSave() {
    saving = true
    saveErr = null
    try {
      await save()
      const cur = $loaded
      if (cur) setLoaded(enrich(cur))
      pushToast('Node saved.', 'success')
    } catch (e) {
      if (e instanceof ApiError) {
        saveErr = { code: e.code, message: e.message }
        if (e.code === 'proxy_failed')
          pushToast('The target node must be reachable to apply this.', 'error')
      } else {
        saveErr = { code: 'unreachable', message: 'Cannot reach this node.' }
      }
    } finally {
      saving = false
    }
  }

  // reloadReapply (409): reload the doc fresh so the operator re-applies on top of
  // the concurrent edit — never a silent overwrite (08 §0.5).
  async function reloadReapply() {
    saveErr = null
    await load()
  }

  function onRevert() {
    revert()
  }

  const node = $derived($loaded)
  // Draft-or-loaded values threaded into the controls so edits + Revert reflect.
  const draftName = $derived($draft.name ?? node?.name ?? '')
  const channel = $derived($draft.channel ?? node?.channel ?? 'stereo')
  const gainDb = $derived($draft.gainDb ?? node?.gainDb ?? 0)
  const hwDelayUs = $derived($draft.hwDelayUs ?? node?.hwDelayUs ?? 0)
  const device = $derived($draft.device ?? node?.device ?? '')
  // disabled = offline || saving (threaded into audio + capability components).
  const controlsDisabled = $derived($offline || saving)
  const servingNode = $derived($session?.nodeId ?? 'this node')
  const isProxied = $derived(node !== null && node.id !== $session?.nodeId)
</script>

{#if phase === 'loading'}
  <Card>
    <Skeleton rows={6} />
  </Card>
{:else if phase === 'error' && loadErr}
  <Card title="Node not found">
    <Banner
      code={loadErr.code}
      message={loadErr.message}
      onRetry={load}
    />
    <div class="back-row">
      <button class="link" type="button" onclick={() => navigate('/cluster')}>
        ← back to Cluster
      </button>
    </div>
  </Card>
{:else if node}
  <div class="node-detail">
    <NodeDetailHeader {node} />

    {#if $offline}
      <Banner
        code="offline"
        message="This node is offline — showing the last-known config from the replicated document. Audio, calibration, and capability edits are disabled until it returns; a rename will apply when the node reconnects."
        tone="offline"
      />
    {/if}

    {#if isProxied}
      <p class="proxy">
        <ProxyHint serving={servingNode} target={node.id} />
      </p>
    {/if}

    {#if saveErr}
      <Banner
        code={saveErr.code}
        message={saveErr.message}
        onReloadReapply={saveErr.code === 'version_conflict' ? reloadReapply : undefined}
      />
    {/if}

    <Card title="Identity">
      <IdentityPanel {node} {draftName} offline={$offline} />
    </Card>

    {#if $isRenderEnabled}
      <Card title="Audio output">
        <AudioOutputSection
          {node}
          renderEnabled={$isRenderEnabled}
          disabled={controlsDisabled}
          {channel}
          {gainDb}
          {hwDelayUs}
          {device}
          liveSyncErrorUs={$liveSyncErrorUs}
        />
      </Card>
    {:else}
      <Card>
        <AudioOutputSection
          {node}
          renderEnabled={$isRenderEnabled}
          disabled={controlsDisabled}
          {channel}
          {gainDb}
          {hwDelayUs}
          {device}
          liveSyncErrorUs={$liveSyncErrorUs}
        />
      </Card>
    {/if}

    <Card title="Capabilities & audio backends">
      <CapabilitiesPanel
        {node}
        draftMask={$draft.capabilities}
        renderEnabled={$isRenderEnabled}
        disabled={controlsDisabled}
      />
    </Card>

    <Card title="Network">
      <NetworkPanel addrs={node.addrs ?? []} fingerprint={node.fingerprint} />
    </Card>

    <SaveBar dirty={$isDirty} {saving} onSave={onSave} onRevert={onRevert} />
  </div>
{/if}

<style>
  .node-detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    max-width: 56rem;
  }
  .proxy {
    margin: 0;
  }
  .back-row {
    margin-top: var(--space-3);
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
