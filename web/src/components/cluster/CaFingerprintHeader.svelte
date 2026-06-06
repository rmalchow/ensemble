<script lang="ts">
  // Cluster screen header strip (09 §4): cluster name on the left, the CA
  // fingerprint trust anchor on the right with a copy button. The fingerprint is
  // the anchor an operator verifies out-of-band before adopting raw nodes (A.1).
  // Reuses the shared CopyField (copy + transient confirmation + insecure-context
  // fallback) and fmtFingerprint (colon-grouped SHA256 rendering).
  import CopyField from '../CopyField.svelte'
  import { fmtFingerprint } from '../../lib/format'

  interface Props {
    name: string
    fingerprint: string
  }
  let { name, fingerprint }: Props = $props()
</script>

<header class="ca-header">
  <div class="title">
    <span class="label">Cluster</span>
    <span class="name">{name || '—'}</span>
  </div>
  <div class="fp">
    <CopyField label="CA fingerprint" value={fmtFingerprint(fingerprint)} mono />
  </div>
</header>

<style>
  .ca-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5);
    flex-wrap: wrap;
    padding: var(--space-4);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--surface-2);
  }
  .title {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .label {
    font-size: var(--text-xs);
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .name {
    font-size: var(--text-lg, 1.1rem);
    color: var(--text);
    font-weight: 600;
  }
  .fp {
    flex: 1;
    min-width: 18rem;
    max-width: 34rem;
  }
</style>
