<script lang="ts">
  // Adapted from mpvsync ConfirmModal: logic unchanged; hard-coded hex replaced
  // with design tokens (P0.4 §3).
  import { activeConfirm } from '../lib/confirm'

  const req = $derived($activeConfirm)

  // askEveryTime is the checkbox state; it resets to checked each time a new
  // request opens. confirmBtn is focused on open for keyboard access.
  let askEveryTime = $state(true)
  let confirmBtn = $state<HTMLButtonElement | null>(null)

  $effect(() => {
    if (req) {
      askEveryTime = true
      // Focus the confirm button once it is rendered.
      confirmBtn?.focus()
    }
  })

  function cancel() {
    // Cancel never suppresses, regardless of the checkbox.
    req?._resolve(false, true)
  }
  function confirm() {
    req?._resolve(true, askEveryTime)
  }
</script>

{#if req}
  <div
    class="backdrop"
    role="button"
    tabindex="-1"
    onclick={cancel}
    onkeydown={(e) => e.key === 'Escape' && cancel()}
  >
    <div
      class="dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="confirm-title"
      tabindex="0"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => {
        if (e.key === 'Escape') cancel()
        e.stopPropagation()
      }}
    >
      <h3 id="confirm-title">{req.title ?? 'Please confirm'}</h3>
      <p class="message">{req.message}</p>

      <label class="ask">
        <input type="checkbox" bind:checked={askEveryTime} />
        <span>Ask every time</span>
      </label>

      <div class="actions">
        <button type="button" class="ghost" onclick={cancel}>Cancel</button>
        <button
          type="button"
          class:danger={req.danger}
          bind:this={confirmBtn}
          onclick={confirm}
        >
          {req.confirmLabel ?? 'Confirm'}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(1, 4, 9, 0.7);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: var(--z-modal);
  }
  .dialog {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: 1.25rem;
    width: 22rem;
    max-width: calc(100vw - 2rem);
    box-shadow: var(--shadow-modal);
  }
  h3 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
  }
  .message {
    margin: 0 0 1rem;
    font-size: var(--text-sm);
    color: var(--text-dim);
    line-height: 1.4;
  }
  .ask {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    margin-bottom: 1rem;
    font-size: 0.8rem;
    color: var(--text-muted);
    cursor: pointer;
  }
  .ask input {
    accent-color: var(--accent);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  button {
    font: inherit;
    padding: 0.45rem 0.9rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--success);
    color: var(--text-inverse);
    cursor: pointer;
  }
  button.ghost {
    background: transparent;
    color: var(--text);
  }
  button.danger {
    background: var(--danger);
    border-color: var(--danger-bright);
  }
</style>
