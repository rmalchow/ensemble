<script lang="ts">
  // Setup Wizard — first-run, uninitialized node (09 §1). Two and only two paths
  // (D9): (a) Create new cluster, (b) Wait to be adopted. The boot probe in App
  // already gated us here (initialized==false) and supplies this node's identity
  // (nodeId / CSR fingerprint) for the adopt panel. Submitting (a) runs B.1 (which
  // logs the operator in) and lands on Dashboard; (b) is read-only — it shows the
  // identity an operator pins + the adoption PIN, and polls status so the wizard
  // auto-advances to Login once the node is adopted from elsewhere.
  import { onDestroy } from 'svelte'
  import { setup, ApiError } from '../lib/api'
  import { probeStatus, type StatusProbe } from '../lib/status'
  import { navigate } from '../lib/router'
  import { clusterInfo } from '../lib/stores'
  import { fmtFingerprint } from '../lib/format'
  import { ADOPTION_PIN_DEFAULT } from '../lib/constants'
  import Card from '../components/ui/Card.svelte'
  import Button from '../components/ui/Button.svelte'
  import Field from '../components/Field.svelte'
  import CopyField from '../components/CopyField.svelte'
  import PasswordStrength from '../components/PasswordStrength.svelte'
  import Banner from '../components/Banner.svelte'

  interface Props {
    // probe is the boot status (nodeId/fingerprint for the adopt panel).
    probe: StatusProbe
    // onComplete re-probes + advances the app once this node flips to
    // initialized — after a successful create (genesis auto-logs-in, so the
    // re-probe lands on the Dashboard) or after being adopted (lands on
    // /login). App supplies the re-probe; navigating directly would run the
    // route guard against App's STALE pre-setup flags and bounce back here.
    onComplete?: () => void
  }
  let { probe, onComplete }: Props = $props()

  type Path = 'create' | 'adopt'
  let path = $state<Path>('create')

  // --- Create-path form state ---
  let clusterName = $state('')
  let adminPassword = $state('')
  let confirm = $state('')
  let busy = $state(false)
  let err = $state<{ code: string; message: string } | null>(null)

  const passwordsMatch = $derived(adminPassword.length > 0 && adminPassword === confirm)
  const canSubmit = $derived(
    !busy && clusterName.trim().length > 0 && adminPassword.length > 0 && passwordsMatch,
  )
  const confirmError = $derived(
    confirm.length > 0 && !passwordsMatch ? 'Passwords do not match.' : undefined,
  )

  async function submitCreate(e: Event) {
    e.preventDefault()
    if (!canSubmit) return
    busy = true
    err = null
    try {
      const res = await setup({
        clusterName: clusterName.trim(),
        adminPassword,
      })
      // B.1 set a session cookie (auto-login). Seed the header, then let App
      // re-probe (initialized:true + authenticated:true) so its route guard
      // lands on the Dashboard (09 §1 user flow 1) — a direct navigate would
      // hit the stale pre-setup guard and bounce back to /setup.
      clusterInfo.set({
        name: res.cluster.name,
        caFingerprint: res.cluster.caFingerprint,
      })
      if (onComplete) onComplete()
      else navigate('/')
    } catch (e) {
      err =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
      // Already initialized (raced setup elsewhere) → bounce to Login.
      if (e instanceof ApiError && (e.status === 409 || e.code === 'conflict')) {
        navigate('/login')
      }
    } finally {
      busy = false
    }
  }

  // --- Adopt-path polling: detect when this node has been adopted elsewhere. ---
  let pollTimer: ReturnType<typeof setInterval> | undefined
  $effect(() => {
    if (path === 'adopt') startAdoptPoll()
    else stopAdoptPoll()
  })
  function startAdoptPoll() {
    stopAdoptPoll()
    pollTimer = setInterval(async () => {
      try {
        const st = await probeStatus()
        if (st.initialized) {
          stopAdoptPoll()
          if (onComplete) onComplete()
          else navigate('/login')
        }
      } catch {
        // Transient — keep polling; App owns the hard error state.
      }
    }, 3000)
  }
  function stopAdoptPoll() {
    if (pollTimer) clearInterval(pollTimer)
    pollTimer = undefined
  }
  onDestroy(stopAdoptPoll)

  const fp = $derived(fmtFingerprint(probe.fingerprint))
</script>

<div class="page">
  <Card title="Ensemble — set up this player">
    <p class="lead">This node is not part of any cluster yet.</p>

    <div class="paths" role="radiogroup" aria-label="Setup path">
      <button
        type="button"
        role="radio"
        aria-checked={path === 'create'}
        class:active={path === 'create'}
        onclick={() => (path = 'create')}
      >
        Create a new cluster
      </button>
      <button
        type="button"
        role="radio"
        aria-checked={path === 'adopt'}
        class:active={path === 'adopt'}
        onclick={() => (path = 'adopt')}
      >
        Wait to be adopted
      </button>
    </div>

    {#if path === 'create'}
      <form onsubmit={submitCreate} class="create">
        <Field
          id="cluster-name"
          label="Cluster name"
          bind:value={clusterName}
          placeholder="e.g. Living Room Cluster"
          required
          disabled={busy}
        />
        <div class="pw">
          <Field
            id="admin-pw"
            label="Admin password"
            type="password"
            bind:value={adminPassword}
            autocomplete="new-password"
            required
            disabled={busy}
          />
          <PasswordStrength password={adminPassword} />
        </div>
        <Field
          id="admin-pw-confirm"
          label="Confirm password"
          type="password"
          bind:value={confirm}
          autocomplete="new-password"
          required
          disabled={busy}
          error={confirmError}
        />

        <p class="note">
          This node becomes the first member and issues the cluster CA. You can
          adopt more nodes afterward.
        </p>

        {#if err}
          <Banner code={err.code} message={err.message} />
        {/if}

        <div class="actions">
          <Button type="submit" disabled={!canSubmit} loading={busy}>
            Create cluster →
          </Button>
        </div>
      </form>
    {:else}
      <div class="adopt">
        <CopyField label="Node ID" value={probe.nodeId} />
        <CopyField label="Fingerprint (this node's CSR key)" value={fp} />

        <div class="pin">
          <span class="pin-label">Adoption PIN</span>
          <span class="pin-value mono" aria-label="adoption pin">
            {ADOPTION_PIN_DEFAULT.split('').join(' ')}
          </span>
          <span class="pin-warn">
            ⚠ placeholder — treated as a real secret
          </span>
        </div>

        <p class="note">
          On another node's UI → <strong>Cluster → Adopt</strong>, enter the Node ID
          and PIN above to bring this player in. This screen will continue to Login
          automatically once adoption completes.
        </p>
      </div>
    {/if}
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
    width: 34rem;
    max-width: calc(100vw - 2rem);
  }
  .lead {
    margin: 0 0 var(--space-4);
    color: var(--text-dim);
    font-size: var(--text-sm);
  }
  .paths {
    display: flex;
    gap: var(--space-2);
    margin-bottom: var(--space-5);
    padding-bottom: var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
  }
  .paths button {
    flex: 1;
    font: inherit;
    font-size: var(--text-sm);
    padding: 0.6rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--surface-2);
    color: var(--text-dim);
    cursor: pointer;
  }
  .paths button.active {
    border-color: var(--accent);
    background: rgba(31, 111, 235, 0.12);
    color: var(--accent-bright);
    font-weight: 600;
  }
  .create,
  .adopt {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .pw {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .note {
    margin: 0;
    font-size: var(--text-xs);
    color: var(--text-muted);
    line-height: 1.5;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
  }
  .pin {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .pin-label {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .pin-value {
    font-size: var(--text-xl);
    letter-spacing: 0.3em;
    color: var(--text);
  }
  .pin-warn {
    font-size: var(--text-xs);
    color: var(--warn-bright);
  }
</style>
