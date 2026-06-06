<script lang="ts">
  // Login (09 §2). Single-admin model — no username (D11). Exchanges the admin
  // password for a session cookie (B.2), then lands on the originally-requested
  // route (?next) or Dashboard. Bad password → generic "wrong password" (no user
  // enumeration); rate_limited → the throttle message (A.12 brute-force guard).
  import { login, ApiError } from '../lib/api'
  import { refreshSession } from '../lib/session'
  import { navigate } from '../lib/router'
  import { session } from '../lib/stores'
  import Card from '../components/ui/Card.svelte'
  import Button from '../components/ui/Button.svelte'
  import Field from '../components/Field.svelte'

  interface Props {
    // nodeId/clusterName drive the footer chips; supplied by App from the probe.
    nodeId?: string
    clusterName?: string
    // onAuthenticated lets App flip its live `authenticated` guard flag BEFORE
    // the navigate below — without it the route guard still holds the stale
    // pre-login value and bounces the navigation straight back to /login.
    onAuthenticated?: () => void
  }
  let { nodeId, clusterName, onAuthenticated }: Props = $props()

  let password = $state('')
  let keep = $state(false)
  let busy = $state(false)
  let errorMsg = $state('')

  // next is the route to return to after a successful login (set by the 401
  // redirect / guard). Defaults to Dashboard.
  function nextRoute(): string {
    const sp = new URLSearchParams(location.search)
    const n = sp.get('next')
    return n && n.startsWith('/') ? n : '/'
  }

  async function submit(e: Event) {
    e.preventDefault()
    if (busy || !password) return
    busy = true
    errorMsg = ''
    try {
      await login(password, keep)
      // Refresh identity + configVersion, flip App's guard flag, then return to
      // the intended route (?next) or the Dashboard.
      await refreshSession()
      onAuthenticated?.()
      navigate(nextRoute())
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.code === 'rate_limited' || e.status === 429) {
          errorMsg =
            'Too many attempts. Wait for the lockout to clear and try again.'
        } else if (e.code === 'not_ready' || e.status === 503) {
          // Node uninitialized → Setup Wizard (09 §2 uninitialized state).
          navigate('/setup')
          return
        } else if (e.status === 401) {
          // Generic — never reveal whether the account/credential exists.
          errorMsg = 'Wrong password.'
        } else {
          errorMsg = e.message || 'Sign in failed.'
        }
      } else {
        errorMsg = 'Cannot reach this player; try another node’s URL.'
      }
    } finally {
      busy = false
      // Never keep the plaintext around longer than the in-flight request.
      if (!$session) password = ''
    }
  }
</script>

<div class="page">
  <Card title={clusterName ? `Ensemble — ${clusterName}` : 'Ensemble — sign in'}>
    <form onsubmit={submit} class="form">
      <Field
        id="login-password"
        label="Admin password"
        type="password"
        bind:value={password}
        autocomplete="current-password"
        required
        disabled={busy}
        error={errorMsg || undefined}
      />

      <label class="keep">
        <input type="checkbox" bind:checked={keep} disabled={busy} />
        <span>Keep me signed in</span>
      </label>

      <div class="actions">
        <Button type="submit" disabled={busy || !password} loading={busy}>Sign in</Button>
      </div>

      <div class="footer">
        {#if nodeId}<span class="chip mono">Serving node: {nodeId}</span>{/if}
        <span class="chip">Served over TLS</span>
      </div>
    </form>
  </Card>
</div>

<style>
  .page {
    min-height: 100%;
    display: grid;
    place-items: center;
    padding: var(--space-8);
  }
  .page :global(.card) {
    width: 24rem;
    max-width: calc(100vw - 2rem);
  }
  .form {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .keep {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--text-dim);
    cursor: pointer;
  }
  .keep input {
    accent-color: var(--accent);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
  }
  .footer {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    padding-top: var(--space-3);
    border-top: 1px solid var(--border-subtle);
  }
  .chip {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
