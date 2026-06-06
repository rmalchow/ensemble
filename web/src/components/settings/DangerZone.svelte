<script lang="ts">
  // Settings → danger zone (09 §8). Leave / reset this cluster behind a TYPED
  // confirm (the operator must type the cluster name). Runs C.6 with
  // If-Match: configVersion — coordinated self-forget: add this node's cert
  // fingerprint to the grow-only RevokedSet + drop its NodeRecord (gossiped,
  // monotonic-union merge), then wipe local identity/config and reboot into the
  // Setup Wizard. `coordinated:false` ⇒ unreachable-cluster local-wipe fallback;
  // warn the operator. After completion App re-probes → /setup.
  import { leaveCluster, ApiError } from '../../lib/api'
  import { configVersion } from '../../lib/stores'
  import { session } from '../../lib/stores'
  import Button from '../ui/Button.svelte'
  import Banner from '../Banner.svelte'

  interface Props {
    // clusterName is the phrase the operator must type to confirm.
    clusterName?: string
    // onLeft is invoked after a successful leave so App re-probes → /setup.
    onLeft?: (coordinated: boolean) => void
  }
  let { clusterName, onLeft }: Props = $props()

  let open = $state(false)
  let typed = $state('')
  let busy = $state(false)
  let banner = $state<{ code: string; message: string } | null>(null)

  const phrase = $derived(clusterName ?? 'leave')
  const confirmed = $derived(typed.trim() === phrase)

  function start() {
    open = true
    typed = ''
    banner = null
  }
  function cancel() {
    open = false
  }

  async function doLeave() {
    if (!confirmed || busy) return
    busy = true
    banner = null
    const ver = $configVersion
    if (ver === undefined) {
      banner = { code: 'precondition_required', message: 'Config version unknown — reload.' }
      busy = false
      return
    }
    try {
      const res = await leaveCluster(ver)
      open = false
      session.set(null)
      onLeft?.(res.coordinated)
    } catch (e) {
      banner =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Cannot reach this player.' }
    } finally {
      busy = false
    }
  }
</script>

<div class="zone">
  <div class="head">
    <span class="title">⚠ Danger zone</span>
    <span class="desc">Removes this node's identity &amp; cluster state.</span>
  </div>
  <Button variant="danger" onclick={start}>Leave / reset this cluster</Button>

  {#if banner}
    <Banner code={banner.code} message={banner.message} />
  {/if}
</div>

{#if open}
  <div
    class="backdrop"
    role="button"
    tabindex="-1"
    onclick={cancel}
    onkeydown={(e) => e.key === 'Escape' && cancel()}
  >
    <div
      class="dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="leave-title"
      tabindex="0"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => {
        if (e.key === 'Escape') cancel()
        e.stopPropagation()
      }}
    >
      <h3 id="leave-title">Leave / reset this cluster</h3>
      <p class="warn">
        This wipes this node's certs, identity, and config and reboots it into the
        Setup Wizard. If peers are offline the local wipe still happens, but the
        cluster won't drop this node until you forget it from another node.
      </p>
      <label class="confirm-label" for="leave-confirm">
        Type <strong>{phrase}</strong> to confirm:
      </label>
      <input
        id="leave-confirm"
        type="text"
        bind:value={typed}
        autocomplete="off"
        placeholder={phrase}
      />

      {#if banner}
        <Banner code={banner.code} message={banner.message} />
      {/if}

      <div class="actions">
        <Button variant="ghost" onclick={cancel}>Cancel</Button>
        <Button variant="danger" disabled={!confirmed} loading={busy} onclick={doLeave}>
          Leave cluster
        </Button>
      </div>
    </div>
  </div>
{/if}

<style>
  .zone {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius-md);
    padding: var(--space-4);
    background: rgba(218, 54, 51, 0.06);
  }
  .head {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .title {
    color: var(--danger-bright);
    font-weight: 600;
    font-size: var(--text-sm);
  }
  .desc {
    color: var(--text-muted);
    font-size: var(--text-xs);
  }
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(1, 4, 9, 0.7);
    display: grid;
    place-items: center;
    z-index: var(--z-modal);
  }
  .dialog {
    background: var(--surface);
    border: 1px solid var(--danger);
    border-radius: var(--radius-md);
    padding: 1.25rem;
    width: 26rem;
    max-width: calc(100vw - 2rem);
    box-shadow: var(--shadow-modal);
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  h3 {
    margin: 0;
    font-size: 1rem;
  }
  .warn {
    margin: 0;
    font-size: var(--text-sm);
    color: var(--text-dim);
    line-height: 1.45;
  }
  .confirm-label {
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  input {
    background: var(--raised);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0.5rem 0.65rem;
    color: var(--text);
    font: inherit;
  }
  input:focus {
    outline: none;
    border-color: var(--danger-bright);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
</style>
