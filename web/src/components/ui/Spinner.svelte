<script lang="ts">
  interface Props {
    size?: number // px
    label?: string
  }
  let { size = 20, label = 'Loading' }: Props = $props()
</script>

<span
  class="spinner"
  role="status"
  aria-label={label}
  style="width:{size}px;height:{size}px"
></span>

<style>
  .spinner {
    display: inline-block;
    border: 2px solid var(--surface-3);
    border-top-color: var(--accent-bright);
    border-radius: 50%;
    animation: spin 0.7s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
