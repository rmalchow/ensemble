<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    // tone selects the semantic color. `neutral` is the default badge.
    tone?: 'neutral' | 'success' | 'danger' | 'warn' | 'accent' | 'muted'
    children?: Snippet
  }
  let { tone = 'neutral', children }: Props = $props()
</script>

<span class="chip {tone}">{@render children?.()}</span>

<style>
  .chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 0.12rem 0.5rem;
    border-radius: 999px;
    font-size: var(--text-xs);
    font-weight: 500;
    line-height: 1.5;
    border: 1px solid var(--border);
    background: var(--surface-3);
    color: var(--text-dim);
  }
  .chip.success {
    color: var(--success-bright);
    border-color: var(--success);
    background: rgba(46, 160, 67, 0.12);
  }
  .chip.danger {
    color: var(--danger-bright);
    border-color: var(--danger);
    background: rgba(248, 81, 73, 0.12);
  }
  .chip.warn {
    color: var(--warn-bright);
    border-color: var(--warn);
    background: rgba(227, 179, 65, 0.12);
  }
  .chip.accent {
    color: var(--accent-bright);
    border-color: var(--accent);
    background: rgba(31, 111, 235, 0.12);
  }
  .chip.muted {
    color: var(--text-muted);
  }
</style>
