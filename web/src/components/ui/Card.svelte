<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    title?: string
    children?: Snippet
    actions?: Snippet
  }
  let { title, children, actions }: Props = $props()
</script>

<section class="card">
  {#if title || actions}
    <header>
      {#if title}<h3>{title}</h3>{/if}
      {#if actions}<div class="actions">{@render actions()}</div>{/if}
    </header>
  {/if}
  <div class="body">{@render children?.()}</div>
</section>

<style>
  .card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    box-shadow: var(--shadow-card);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
  }
  h3 {
    font-size: var(--text-base);
  }
  .body {
    padding: var(--space-4);
  }
</style>
