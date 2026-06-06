<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    variant?: 'primary' | 'ghost' | 'danger' | 'success'
    type?: 'button' | 'submit' | 'reset'
    disabled?: boolean
    loading?: boolean
    onclick?: (e: MouseEvent) => void
    children?: Snippet
  }
  let {
    variant = 'primary',
    type = 'button',
    disabled = false,
    loading = false,
    onclick,
    children,
  }: Props = $props()
</script>

<button
  {type}
  class={variant}
  class:loading
  disabled={disabled || loading}
  {onclick}
>
  {#if loading}<span class="dot" aria-hidden="true"></span>{/if}
  {@render children?.()}
</button>

<style>
  button {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0.5rem 0.95rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--accent);
    color: var(--text-inverse);
    font-size: var(--text-sm);
    font-weight: 500;
    transition: filter 0.12s ease, opacity 0.12s ease;
  }
  button:hover:not(:disabled) {
    filter: brightness(1.1);
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  button.ghost {
    background: transparent;
    color: var(--text);
    border-color: var(--border);
  }
  button.ghost:hover:not(:disabled) {
    background: var(--surface-3);
    filter: none;
  }
  button.danger {
    background: var(--danger);
    border-color: var(--danger-bright);
  }
  button.success {
    background: var(--success);
    border-color: var(--success-bright);
  }
  .dot {
    width: 0.7rem;
    height: 0.7rem;
    border: 2px solid currentColor;
    border-top-color: transparent;
    border-radius: 50%;
    animation: spin 0.7s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
