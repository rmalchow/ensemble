<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    title: string
    description?: string
    // cta slot for the primary call-to-action (e.g. "Create group").
    cta?: Snippet
  }
  let { title, description, cta }: Props = $props()
</script>

<div class="empty">
  <h3>{title}</h3>
  {#if description}<p>{description}</p>{/if}
  {#if cta}<div class="cta">{@render cta()}</div>{/if}
</div>

<style>
  .empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-3);
    text-align: center;
    padding: var(--space-8) var(--space-4);
    color: var(--text-muted);
  }
  h3 {
    color: var(--text-dim);
    font-size: var(--text-lg);
  }
  p {
    max-width: 28rem;
    font-size: var(--text-sm);
  }
  .cta {
    margin-top: var(--space-2);
  }
</style>
