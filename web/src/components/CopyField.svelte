<script lang="ts">
  // Read-only value + copy button (P1.4 §2). Used for node id, CSR fingerprint,
  // CA fingerprint, and the once-shown API-key secret (09 §1 / §8). Shows a
  // transient "Copied" confirmation; falls back gracefully if the platform
  // refuses (insecure context).
  import { copy } from '../lib/clipboard'

  interface Props {
    label?: string
    value: string
    // mono renders the value in the monospace font (ids/fingerprints/secrets).
    mono?: boolean
    // secret masks the value visually but still copies the plaintext (the
    // once-shown API key). Toggleable reveal.
    secret?: boolean
  }
  let { label, value, mono = true, secret = false }: Props = $props()

  let copied = $state(false)
  let revealed = $state(false)
  let timer: ReturnType<typeof setTimeout> | undefined

  const shown = $derived(secret && !revealed ? '•'.repeat(Math.min(value.length, 24)) : value)

  async function doCopy() {
    const ok = await copy(value)
    if (!ok) return
    copied = true
    if (timer) clearTimeout(timer)
    timer = setTimeout(() => (copied = false), 1500)
  }
</script>

<div class="copyfield">
  {#if label}<span class="label">{label}</span>{/if}
  <div class="row">
    <code class:mono class="value" title={secret && !revealed ? 'hidden' : value}>{shown}</code>
    {#if secret}
      <button type="button" class="ghost" onclick={() => (revealed = !revealed)}>
        {revealed ? 'Hide' : 'Show'}
      </button>
    {/if}
    <button type="button" class="copy" onclick={doCopy} aria-label="Copy {label ?? 'value'}">
      {copied ? 'Copied' : 'Copy'}
    </button>
  </div>
</div>

<style>
  .copyfield {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .label {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .value {
    flex: 1;
    min-width: 0;
    overflow-x: auto;
    white-space: nowrap;
    background: var(--raised);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0.4rem 0.6rem;
    font-size: var(--text-sm);
    color: var(--text);
  }
  .value.mono {
    font-family: var(--font-mono);
  }
  button {
    flex-shrink: 0;
    font: inherit;
    font-size: var(--text-sm);
    padding: 0.4rem 0.7rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--surface-3);
    color: var(--text);
    cursor: pointer;
  }
  button:hover {
    background: var(--surface-2);
  }
  button.copy {
    border-color: var(--accent);
    color: var(--accent-bright);
  }
</style>
