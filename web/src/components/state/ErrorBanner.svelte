<script lang="ts">
  import Button from '../ui/Button.svelte'

  interface Props {
    code: string
    message: string
    onRetry?: () => void
    // onReloadReapply is shown only for a 409 version_conflict (08 §0.5):
    // re-GET, show what changed, ask to reapply — never silently overwrite.
    onReloadReapply?: () => void
  }
  let { code, message, onRetry, onReloadReapply }: Props = $props()

  const isConflict = $derived(code === 'version_conflict')
</script>

<div class="banner" role="alert">
  <div class="text">
    <span class="code mono">{code}</span>
    <span class="msg">{message}</span>
  </div>
  <div class="actions">
    {#if onRetry}
      <Button variant="ghost" onclick={onRetry}>Retry</Button>
    {/if}
    {#if isConflict && onReloadReapply}
      <Button variant="primary" onclick={onReloadReapply}>Reload &amp; reapply</Button>
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
  .text {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .code {
    font-size: var(--text-xs);
    color: var(--danger-bright);
    text-transform: none;
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
