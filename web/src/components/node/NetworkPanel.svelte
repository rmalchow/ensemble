<script lang="ts">
  // Network block (09 §6): the node's known addrs (with the note that they drive
  // the allowlist, README §6.5) and its cert fingerprint (copyable). Read-only —
  // addrs/fingerprint are not edited here.
  import CopyField from '../CopyField.svelte'
  import { fmtFingerprint } from '../../lib/format'

  interface Props {
    addrs: string[]
    fingerprint?: string
  }
  let { addrs, fingerprint }: Props = $props()
</script>

<div class="net">
  <div class="addrs">
    <span class="label">addrs</span>
    {#if addrs.length > 0}
      <code class="mono">{addrs.join(', ')}</code>
    {:else}
      <span class="dash">—</span>
    {/if}
    <span class="note">(drive the allowlist)</span>
  </div>
  {#if fingerprint}
    <CopyField label="cert fingerprint" value={fmtFingerprint(fingerprint)} />
  {/if}
</div>

<style>
  .net {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .addrs {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .label {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--text);
  }
  .dash {
    color: var(--text-muted);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
