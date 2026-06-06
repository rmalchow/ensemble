<script lang="ts">
  interface Props {
    // variant distinguishes a fully offline region ("offline") from a dimmed
    // last-known value ("last known"), per 09 §3. Both differ from a sink-less
    // node's "⊘ no sink" badge (a screen-piece concern; normal weight).
    variant?: 'offline' | 'last-known'
  }
  let { variant = 'offline' }: Props = $props()
</script>

<span class="offline-chip {variant}">
  {variant === 'offline' ? 'offline' : 'last known'}
</span>

<style>
  .offline-chip {
    display: inline-flex;
    align-items: center;
    padding: 0.1rem 0.45rem;
    border-radius: 999px;
    font-size: var(--text-xs);
    border: 1px solid var(--border);
    color: var(--text-muted);
    background: var(--surface-2);
  }
  .offline-chip.offline {
    border-style: dashed;
  }
  .offline-chip.last-known {
    font-style: italic;
    opacity: 0.85;
  }
</style>
