<script lang="ts">
  // One discovered node (09 §4 discovered table): id, source addr, CSR
  // fingerprint (verified out-of-band), a PIN input defaulting to "0000" (D9 —
  // a real secret, pre-filled but editable), and an Adopt action. A foreign node
  // (state === "foreign") is shown with a hint that it needs Takeover, not Adopt.
  import Button from '../ui/Button.svelte'
  import Chip from '../ui/Chip.svelte'
  import { fmtFingerprint } from '../../lib/format'
  import type { DiscoveredNode, ApiError } from '../../lib/cluster'

  interface Props {
    node: DiscoveredNode
    busy: boolean
    error?: ApiError
    onAdopt: (pin: string) => void
  }
  let { node, busy, error, onAdopt }: Props = $props()

  // PIN defaults to "0000" (D9) — pre-filled but editable; sent verbatim.
  let pin = $state('0000')
  const isForeign = $derived(node.state === 'foreign')

  function adopt() {
    if (busy) return
    onAdopt(pin)
  }
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
    <div class="action-row">
      <label class="pin">
        <span class="pin-label">PIN</span>
        <input
          type="text"
          inputmode="numeric"
          autocomplete="off"
          aria-label="Adoption PIN for {node.nodeId}"
          bind:value={pin}
          disabled={busy}
        />
      </label>
      <Button onclick={adopt} disabled={busy || pin.length === 0} loading={busy}>
        Adopt
      </Button>
    </div>
    {#if isForeign}
      <p class="hint">Already in another cluster — use <strong>Takeover</strong> instead.</p>
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
    color: var(--text-dim);
    vertical-align: top;
  }
  .node {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .id {
    color: var(--text);
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
  .pin {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .pin-label {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .pin input {
    width: 5rem;
    font-family: var(--font-mono);
    letter-spacing: 0.2em;
    text-align: center;
    padding: 0.4rem 0.5rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--raised);
    color: var(--text);
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
