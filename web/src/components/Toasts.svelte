<script lang="ts">
  import { toasts, dismissToast } from '../lib/toast'
</script>

<div class="host" aria-live="polite">
  {#each $toasts as t (t.id)}
    <div class="toast {t.tone}" role="status">
      <span class="msg">{t.message}</span>
      <button
        type="button"
        class="close"
        aria-label="Dismiss"
        onclick={() => dismissToast(t.id)}>×</button
      >
    </div>
  {/each}
</div>

<style>
  .host {
    position: fixed;
    bottom: var(--space-4);
    right: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    z-index: var(--z-toast);
    max-width: min(24rem, calc(100vw - 2rem));
  }
  .toast {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-4);
    border-radius: var(--radius-md);
    border: 1px solid var(--border);
    background: var(--surface-2);
    box-shadow: var(--shadow-modal);
    color: var(--text);
    font-size: var(--text-sm);
  }
  .toast.error {
    border-color: var(--danger);
  }
  .toast.success {
    border-color: var(--success);
  }
  .msg {
    flex: 1;
  }
  .close {
    background: transparent;
    border: none;
    color: var(--text-muted);
    font-size: 1.1rem;
    line-height: 1;
    padding: 0;
  }
  .close:hover {
    color: var(--text);
  }
</style>
