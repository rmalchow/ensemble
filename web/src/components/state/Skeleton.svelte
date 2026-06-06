<script lang="ts">
  interface Props {
    rows?: number
    width?: string
  }
  let { rows = 3, width = '100%' }: Props = $props()
</script>

<div class="skeleton" aria-hidden="true" style="width:{width}">
  {#each Array(rows) as _, i (i)}
    <div class="line" style="width:{90 - i * 12}%"></div>
  {/each}
</div>

<style>
  .skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .line {
    height: 0.9rem;
    border-radius: var(--radius-sm);
    background: linear-gradient(
      90deg,
      var(--surface-2) 25%,
      var(--surface-3) 50%,
      var(--surface-2) 75%
    );
    background-size: 200% 100%;
    animation: shimmer 1.3s ease-in-out infinite;
  }
  @keyframes shimmer {
    0% {
      background-position: 200% 0;
    }
    100% {
      background-position: -200% 0;
    }
  }
</style>
