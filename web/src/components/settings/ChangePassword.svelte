<script lang="ts">
  // Settings → change admin password (09 §8). current + new + confirm → B.3a with
  // If-Match: configVersion (08 §0.5). Wrong current → 401 inline field error;
  // stale version → 409 reload-&-reapply banner; weak new → 400 envelope. Updates
  // the argon2id hash in ConfigDoc.Auth server-side (D11).
  import { changePassword, ApiError } from '../../lib/api'
  import { configVersion } from '../../lib/stores'
  import Field from '../Field.svelte'
  import Button from '../ui/Button.svelte'
  import Banner from '../Banner.svelte'
  import PasswordStrength from '../PasswordStrength.svelte'

  interface Props {
    // onReloadReapply re-reads the cluster config to refresh configVersion after
    // a 409, then the operator resubmits (08 §0.5 — never silently overwrite).
    onReloadReapply?: () => void
  }
  let { onReloadReapply }: Props = $props()

  let current = $state('')
  let next = $state('')
  let confirm = $state('')
  let busy = $state(false)
  let currentError = $state('')
  let banner = $state<{ code: string; message: string } | null>(null)
  let done = $state(false)

  const matches = $derived(next.length > 0 && next === confirm)
  const confirmError = $derived(
    confirm.length > 0 && !matches ? 'Passwords do not match.' : undefined,
  )
  const canSubmit = $derived(!busy && current.length > 0 && next.length > 0 && matches)

  async function submit(e: Event) {
    e.preventDefault()
    if (!canSubmit) return
    busy = true
    currentError = ''
    banner = null
    done = false
    const ver = $configVersion
    if (ver === undefined) {
      banner = { code: 'precondition_required', message: 'Config version unknown — reload.' }
      busy = false
      return
    }
    try {
      const res = await changePassword(current, next, ver)
      configVersion.set(res.version)
      current = ''
      next = ''
      confirm = ''
      done = true
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.status === 401) {
          currentError = 'Current password is incorrect.'
        } else if (e.code === 'version_conflict' || e.status === 409) {
          banner = { code: 'version_conflict', message: e.message || 'Config changed elsewhere.' }
        } else {
          banner = { code: e.code, message: e.message }
        }
      } else {
        banner = { code: 'unreachable', message: 'Cannot reach this player.' }
      }
    } finally {
      busy = false
    }
  }
</script>

<form onsubmit={submit} class="panel">
  <Field
    id="cur-pw"
    label="Current password"
    type="password"
    bind:value={current}
    autocomplete="current-password"
    disabled={busy}
    error={currentError || undefined}
  />
  <div class="pw">
    <Field
      id="new-pw"
      label="New password"
      type="password"
      bind:value={next}
      autocomplete="new-password"
      disabled={busy}
    />
    <PasswordStrength password={next} />
  </div>
  <Field
    id="new-pw-confirm"
    label="Confirm new password"
    type="password"
    bind:value={confirm}
    autocomplete="new-password"
    disabled={busy}
    error={confirmError}
  />

  {#if banner}
    <Banner code={banner.code} message={banner.message} {onReloadReapply} />
  {/if}
  {#if done}
    <p class="ok" role="status">Password updated.</p>
  {/if}

  <div class="actions">
    <Button type="submit" disabled={!canSubmit} loading={busy}>Update password</Button>
  </div>
</form>

<style>
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    max-width: 28rem;
  }
  .pw {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .actions {
    display: flex;
    justify-content: flex-start;
  }
  .ok {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--success-bright);
  }
</style>
