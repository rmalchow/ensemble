<script lang="ts">
  import type { Snippet } from 'svelte'
  import Skeleton from './Skeleton.svelte'
  import ErrorBanner from './ErrorBanner.svelte'
  import OfflineChip from './OfflineChip.svelte'

  // The shared data-state machine every data-backed screen composes (09 §0).
  // It selects exactly one branch by `state`. The default slot is the `ready`
  // content; named slots override the built-in fallbacks for loading/empty/
  // error/offline when a screen wants a custom treatment.
  interface Props {
    state: 'loading' | 'empty' | 'error' | 'offline' | 'ready'
    error?: { code: string; message: string }
    onRetry?: () => void
    onReloadReapply?: () => void
    children?: Snippet // ready
    loading?: Snippet
    empty?: Snippet
    errorSlot?: Snippet
    offline?: Snippet
  }
  let {
    state,
    error,
    onRetry,
    onReloadReapply,
    children,
    loading,
    empty,
    errorSlot,
    offline,
  }: Props = $props()
</script>

{#if state === 'ready'}
  {@render children?.()}
{:else if state === 'loading'}
  {#if loading}{@render loading()}{:else}
    <div class="fallback"><Skeleton rows={4} /></div>
  {/if}
{:else if state === 'empty'}
  {#if empty}{@render empty()}{:else}
    <p class="fallback muted">Nothing here yet.</p>
  {/if}
{:else if state === 'error'}
  {#if errorSlot}{@render errorSlot()}{:else}
    <ErrorBanner
      code={error?.code ?? 'error'}
      message={error?.message ?? 'Something went wrong.'}
      {onRetry}
      {onReloadReapply}
    />
  {/if}
{:else if state === 'offline'}
  {#if offline}{@render offline()}{:else}
    <div class="fallback offline">
      <OfflineChip />
      <span class="muted">This region is offline; showing last-known values.</span>
    </div>
  {/if}
{/if}

<style>
  .fallback {
    padding: var(--space-4);
  }
  .fallback.offline {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .muted {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
</style>
