<script lang="ts">
  // One discovered node (09 §4 discovered table): id, source addr (host:port),
  // CSR fingerprint (verified out-of-band), and a TWO-STAGE action:
  //   - uninitialized → "Adopt" reveals the PIN input (pre-filled "0000", D9 — a
  //     real secret, editable) + Confirm/Cancel;
  //   - foreign (member of another cluster) → "Take over" reveals a PASSWORD
  //     input (the target's CURRENT cluster admin password authorizes the
  //     release, 03 §4) + Confirm/Cancel.
  // The credential input only appears AFTER the action is clicked, keeping the
  // resting table free of input chrome.
  import Button from '../ui/Button.svelte'
  import Chip from '../ui/Chip.svelte'
  import { fmtFingerprint } from '../../lib/format'
  import type { DiscoveredNode, ApiError } from '../../lib/cluster'

  interface Props {
    node: DiscoveredNode
    busy: boolean
    error?: ApiError
    onAdopt: (pin: string) => void
    onTakeover?: (password: string) => void
  }
  let { node, busy, error, onAdopt, onTakeover }: Props = $props()

  const isForeign = $derived(node.state === 'foreign')

  // armed: the action button was clicked and the credential input is shown.
  let armed = $state(false)
  // PIN defaults to "0000" (D9) — pre-filled but editable; sent verbatim.
  let pin = $state('0000')
  let password = $state('')

  function arm() {
    if (busy) return
    armed = true
  }
  function cancel() {
    armed = false
    pin = '0000'
    password = ''
  }
  function confirm() {
    if (busy) return
    if (isForeign) {
      if (password.length === 0) return
      onTakeover?.(password)
    } else {
      if (pin.length === 0) return
      onAdopt(pin)
    }
  }
  const canConfirm = $derived(isForeign ? password.length > 0 : pin.length > 0)
</script>

<tr class:busy>
  <td class="node">
    <span class="id mono">{node.nodeId}</span>
    <span class="meta">
      {node.name}
      {#if isForeign}
        <Chip tone="warn">foreign</Chip>
      {:else}
        <Chip tone="muted">new</Chip>
      {/if}
    </span>
  </td>
  <td class="mono addr">{node.addrs[0] ?? '—'}</td>
  <td class="mono fp" title={fmtFingerprint(node.fingerprint)}>
    {fmtFingerprint(node.fingerprint)}
  </td>
  <td class="action">
    {#if !armed}
      <div class="action-row">
        <Button onclick={arm} disabled={busy} loading={busy}>
          {isForeign ? 'Take over' : 'Adopt'}
        </Button>
      </div>
    {:else}
      <div class="action-row">
        <label class="cred">
          <span class="cred-label">{isForeign ? 'Cluster password' : 'PIN'}</span>
          {#if isForeign}
            <input
              type="password"
              autocomplete="off"
              class="password"
              aria-label="Current cluster password for {node.nodeId}"
              bind:value={password}
              disabled={busy}
            />
          {:else}
            <input
              type="text"
              inputmode="numeric"
              autocomplete="off"
              class="pin"
              aria-label="Adoption PIN for {node.nodeId}"
              bind:value={pin}
              disabled={busy}
            />
          {/if}
        </label>
        <Button onclick={confirm} disabled={busy || !canConfirm} loading={busy}>
          {isForeign ? 'Take over' : 'Adopt'}
        </Button>
        <Button variant="ghost" onclick={cancel} disabled={busy}>Cancel</Button>
      </div>
      {#if isForeign}
        <p class="hint">
          Enter the password of the cluster this node currently belongs to.
        </p>
      {/if}
    {/if}
    {#if error}
      <p class="row-error" role="alert">
        <span class="code mono">{error.code}</span>
        <span>{error.message}</span>
      </p>
    {/if}
  </td>
</tr>

<style>
  tr.busy {
    opacity: 0.7;
  }
  td {
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: top;
  }
  .node {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .id {
    color: var(--text);
    font-size: var(--text-sm);
  }
  .meta {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .addr {
    white-space: nowrap;
  }
  .fp {
    max-width: 16rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--text-xs);
  }
  .action-row {
    display: flex;
    align-items: flex-end;
    gap: var(--space-2);
  }
  .cred {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .cred-label {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .cred input {
    padding: 0.4rem 0.5rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--raised);
    color: var(--text);
  }
  .cred input.pin {
    width: 5rem;
    font-family: var(--font-mono);
    letter-spacing: 0.2em;
    text-align: center;
  }
  .cred input.password {
    width: 12rem;
  }
  .hint {
    margin: var(--space-2) 0 0;
    font-size: var(--text-xs);
    color: var(--warn-bright);
  }
  .row-error {
    margin: var(--space-2) 0 0;
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
    font-size: var(--text-xs);
    color: var(--danger-bright);
  }
  .code {
    color: var(--danger-bright);
  }
  .mono {
    font-family: var(--font-mono);
  }
</style>
