<script lang="ts">
  // One Dashboard group card (09 §3): header (name, play-state pill, Play/Stop),
  // a negotiated profile/transport sub-line, the MembersTable, and a per-card
  // error/offline region. The card subscribes its OWN ~1 Hz /status poll
  // (dedup'd by groupStatus.ts) for the lifetime it is mounted. Play/Stop spins
  // until the live (or next-snapshot) playing flag flips. Play is disabled with
  // no media selected (the server also 409s "no media selected"). A whole-group
  // offline (master unreachable → no status) disables Play/Stop.
  import { onMount } from 'svelte'
  import { get } from 'svelte/store'
  import Card from '../ui/Card.svelte'
  import Button from '../ui/Button.svelte'
  import PlayStateBadge from './PlayStateBadge.svelte'
  import MembersTable from './MembersTable.svelte'
  import ErrorBanner from '../state/ErrorBanner.svelte'
  import ProxyHint from '../cluster/ProxyHint.svelte'
  import { pollGroupStatus, groupStatus } from '../../lib/groupStatus'
  import { nodeById, nodeLiveness, configVersion } from '../../lib/groups'
  import { playGroup, stopGroup, ApiError } from '../../lib/groupActions'
  import { navigate } from '../../lib/router'
  import { pushToast } from '../../lib/toast'
  import type { GroupRecord } from '../../lib/types'

  interface Props {
    group: GroupRecord
  }
  let { group }: Props = $props()

  const status = $derived($groupStatus.get(group.id))
  // Live playing wins over the stored bool mid-transition; fall back to stored.
  const playing = $derived(status?.playing ?? group.playing)
  const hasMedia = $derived(!!group.media?.file)
  // No status at all (master unreachable / not yet polled) → group offline.
  const groupOffline = $derived(status === undefined)
  const masterNode = $derived(
    status ? $nodeById.get(status.masterNodeId) : undefined,
  )

  let busy = $state(false)
  let actionErr = $state<{ code: string; message: string } | null>(null)

  onMount(() => pollGroupStatus(group.id))

  function ifMatch(): number | null {
    const v = get(configVersion)
    if (v === undefined) {
      pushToast('Config version unknown — reload the screen.', 'error')
      return null
    }
    return v
  }

  async function onPlay() {
    const v = ifMatch()
    if (v === null) return
    busy = true
    actionErr = null
    try {
      const r = await playGroup(group.id, v)
      configVersion.set(r.version)
    } catch (e) {
      handle(e)
    } finally {
      busy = false
    }
  }

  async function onStop() {
    const v = ifMatch()
    if (v === null) return
    busy = true
    actionErr = null
    try {
      const r = await stopGroup(group.id, v)
      configVersion.set(r.version)
    } catch (e) {
      handle(e)
    } finally {
      busy = false
    }
  }

  function handle(e: unknown) {
    if (e instanceof ApiError) {
      actionErr = { code: e.code, message: e.message }
      if (e.code === 'proxy_failed')
        pushToast('The group master must be reachable to play/stop.', 'error')
    } else {
      actionErr = { code: 'unreachable', message: 'Cannot reach this player.' }
    }
  }

  // Profile sub-line: prefer the live negotiated profile (status), fall back to
  // the stored override. Transport has no GroupRecord field (08 §0.7) — the MVP
  // surfaces UDP unicast as the operating default (D2); the override lives on the
  // Groups screen.
  const profile = $derived(status?.profile ?? group.profile)
</script>

<Card>
  {#snippet actions()}
    <div class="header-actions">
      <PlayStateBadge {playing} />
      {#if playing}
        <Button variant="ghost" loading={busy} disabled={groupOffline} onclick={onStop}>
          ◼ Stop
        </Button>
      {:else}
        <Button
          variant="primary"
          loading={busy}
          disabled={groupOffline || !hasMedia}
          onclick={onPlay}
        >
          ▶ Play
        </Button>
      {/if}
    </div>
  {/snippet}

  <div class="card-head">
    <h3 class="title">{group.name || group.id}</h3>
    <div class="subline">
      <span class="codec">{profile.codec.toUpperCase()}</span>
      <span class="sep">·</span>
      <span>FEC {profile.fec}</span>
      <span class="sep">·</span>
      <span>{(profile.rate / 1000).toFixed(0)} kHz</span>
      <span class="sep">·</span>
      <span>UDP unicast</span>
      {#if masterNode}
        <span class="sep">·</span>
        <ProxyHint serving={masterNode.name || masterNode.id} target="master" />
      {/if}
    </div>
  </div>

  {#if !hasMedia}
    <p class="hint">
      No media selected —
      <button class="link" type="button" onclick={() => navigate('/groups')}>
        select media →
      </button>
    </p>
  {/if}

  {#if actionErr}
    <ErrorBanner code={actionErr.code} message={actionErr.message} />
  {/if}

  <MembersTable {group} {status} nodes={$nodeById} liveness={$nodeLiveness} />
</Card>

<style>
  .header-actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .card-head {
    margin-bottom: var(--space-3);
  }
  .title {
    font-size: var(--text-base);
    margin: 0 0 var(--space-1);
  }
  .subline {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .codec {
    color: var(--accent-bright);
    font-weight: 600;
  }
  .sep {
    opacity: 0.5;
  }
  .hint {
    font-size: var(--text-sm);
    color: var(--text-muted);
    margin: 0 0 var(--space-3);
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
