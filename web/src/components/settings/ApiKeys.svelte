<script lang="ts">
  // Settings → API keys (09 §8). List metadata only (B.5); create (B.6) returns
  // the plaintext secret EXACTLY ONCE — rendered in a CopyField with a "copy it
  // now" note and never re-shown on the next list fetch; revoke (B.7) behind a
  // confirm. All mutating calls carry If-Match: configVersion (08 §0.5).
  import { onMount } from 'svelte'
  import {
    listKeys,
    createKey,
    revokeKey,
    ApiError,
    type ApiKeyMeta,
    type NewApiKey,
  } from '../../lib/api'
  import { configVersion } from '../../lib/stores'
  import { confirmAction } from '../../lib/confirm'
  import { fmtDate } from '../../lib/format'
  import Button from '../ui/Button.svelte'
  import Field from '../Field.svelte'
  import CopyField from '../CopyField.svelte'
  import Banner from '../Banner.svelte'
  import StateMachine from '../state/StateMachine.svelte'

  type DataState = 'loading' | 'ready' | 'error'
  let dataState = $state<DataState>('loading')
  let keys = $state<ApiKeyMeta[]>([])
  let loadErr = $state<{ code: string; message: string } | null>(null)

  let newLabel = $state('')
  let creating = $state(false)
  let createBanner = $state<{ code: string; message: string } | null>(null)
  // freshSecret holds the once-shown plaintext; cleared on the next list refresh.
  let freshSecret = $state<NewApiKey | null>(null)

  async function load() {
    dataState = 'loading'
    loadErr = null
    try {
      const { data } = await listKeys()
      keys = data.keys ?? []
      configVersion.set(data.version)
      dataState = 'ready'
    } catch (e) {
      loadErr =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
      dataState = 'error'
    }
  }

  onMount(load)

  async function create(e: Event) {
    e.preventDefault()
    if (creating || !newLabel.trim()) return
    creating = true
    createBanner = null
    const ver = $configVersion
    if (ver === undefined) {
      createBanner = { code: 'precondition_required', message: 'Config version unknown — reload.' }
      creating = false
      return
    }
    try {
      const res = await createKey(newLabel.trim(), ver)
      configVersion.set(res.version)
      freshSecret = res.key
      newLabel = ''
      // Refresh the list metadata; the secret stays in freshSecret only.
      await load()
    } catch (e) {
      createBanner =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
    } finally {
      creating = false
    }
  }

  async function revoke(k: ApiKeyMeta) {
    const ok = await confirmAction({
      type: 'revoke-api-key',
      title: 'Revoke API key',
      message: `Revoke “${k.label}”? Any client using it will stop working immediately.`,
      confirmLabel: 'Revoke',
      danger: true,
    })
    if (!ok) return
    const ver = $configVersion
    if (ver === undefined) {
      createBanner = { code: 'precondition_required', message: 'Config version unknown — reload.' }
      return
    }
    try {
      const res = await revokeKey(k.id, ver)
      configVersion.set(res.version)
      if (freshSecret?.id === k.id) freshSecret = null
      await load()
    } catch (e) {
      createBanner =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
    }
  }
</script>

<div class="panel">
  <form onsubmit={create} class="create">
    <Field
      id="key-label"
      label="New key label"
      bind:value={newLabel}
      placeholder="e.g. home-assistant"
      disabled={creating}
    />
    <Button type="submit" disabled={creating || !newLabel.trim()} loading={creating}>
      + Create
    </Button>
  </form>

  {#if createBanner}
    <Banner code={createBanner.code} message={createBanner.message} onReloadReapply={load} />
  {/if}

  {#if freshSecret}
    <div class="secret" role="status">
      <p class="secret-note">
        Copy this secret now — it will <strong>not</strong> be shown again.
      </p>
      <CopyField label={`Secret for “${freshSecret.label}”`} value={freshSecret.secret} secret />
    </div>
  {/if}

  <StateMachine
    state={dataState === 'ready' && keys.length === 0 ? 'empty' : dataState}
    error={loadErr ?? undefined}
    onRetry={load}
  >
    {#snippet empty()}
      <p class="empty">No API keys — create one for programmatic access.</p>
    {/snippet}
    <table>
      <thead>
        <tr><th>Name</th><th>Created</th><th>Last used</th><th></th></tr>
      </thead>
      <tbody>
        {#each keys as k (k.id)}
          <tr>
            <td>{k.label}</td>
            <td class="mono">{fmtDate(k.createdAt)}</td>
            <td class="mono">{fmtDate(k.lastUsedAt)}</td>
            <td class="right">
              <Button variant="danger" onclick={() => revoke(k)}>Revoke</Button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </StateMachine>
</div>

<style>
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .create {
    display: flex;
    align-items: flex-end;
    gap: var(--space-3);
  }
  .create :global(.field) {
    flex: 1;
    max-width: 22rem;
  }
  .secret {
    border: 1px solid var(--warn);
    border-radius: var(--radius-md);
    background: rgba(210, 153, 34, 0.08);
    padding: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .secret-note {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--warn-bright);
  }
  .empty {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }
  th {
    text-align: left;
    color: var(--text-muted);
    font-weight: 500;
    font-size: var(--text-xs);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
  }
  td {
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    color: var(--text-dim);
  }
  td.right {
    text-align: right;
  }
</style>
