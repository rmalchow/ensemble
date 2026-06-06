<script lang="ts">
  // Inline error / offline / 409 banner (P1.4 §2, 09 §0 error state). Renders the
  // envelope `code` + `message`, an optional Retry, and — only for a 409
  // version_conflict — a "Reload & reapply" affordance (08 §0.5: never silently
  // overwrite a concurrent edit). `tone="offline"` styles the offline variant.
  import Button from './ui/Button.svelte'

  interface Props {
    code: string
    message: string
    tone?: 'error' | 'offline'
    onRetry?: () => void
    onReloadReapply?: () => void
  }
  let { code, message, tone = 'error', onRetry, onReloadReapply }: Props = $props()

  const isConflict = $derived(code === 'version_conflict')
</script>

<div class="banner {tone}" role="alert">
  <div class="text">
    <span class="code mono">{code}</span>
    <span class="msg">{message}</span>
  </div>
  <div class="actions">
    {#if isConflict && onReloadReapply}
      <Button variant="primary" onclick={onReloadReapply}>Reload &amp; reapply</Button>
    {/if}
    {#if onRetry}
      <Button variant="ghost" onclick={onRetry}>Retry</Button>
    {/if}
  </div>
</div>

<style>
  .banner {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--danger);
    border-radius: var(--radius-md);
    background: rgba(218, 54, 51, 0.1);
  }
  .banner.offline {
    border-color: var(--warn);
    background: rgba(210, 153, 34, 0.1);
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .code {
    font-size: var(--text-xs);
    color: var(--danger-bright);
  }
  .offline .code {
    color: var(--warn-bright);
  }
  .msg {
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  .actions {
    display: flex;
    gap: var(--space-2);
    flex-shrink: 0;
  }
</style>
