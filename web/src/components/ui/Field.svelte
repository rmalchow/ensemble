<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    label: string
    id: string
    hint?: string
    error?: string
    children: Snippet
  }
  let { label, id, hint, error, children }: Props = $props()
</script>

<div class="field" class:has-error={!!error}>
  <label for={id}>{label}</label>
  {@render children()}
  {#if error}
    <p class="error" role="alert">{error}</p>
  {:else if hint}
    <p class="hint">{hint}</p>
  {/if}
</div>

<style>
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  label {
    font-size: var(--text-sm);
    color: var(--text-dim);
    font-weight: 500;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .error {
    font-size: var(--text-xs);
    color: var(--danger-bright);
  }
  .has-error :global(input),
  .has-error :global(select),
  .has-error :global(textarea) {
    border-color: var(--danger);
  }
</style>
