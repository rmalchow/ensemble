<script lang="ts">
  // The "control / media only" badge for a sink-less member (Caps.Render ===
  // false, D17). It is configuration, NOT an error — rendered normal-weight and
  // distinct from the dimmed offline treatment (09 §4). Clicking it deep-links to
  // the node's Capabilities panel on Node detail (screen 6) via the P0.4 router.
  import { navigate } from '../../lib/router'

  interface Props {
    nodeId: string
  }
  let { nodeId }: Props = $props()

  function open() {
    navigate(`/nodes/${encodeURIComponent(nodeId)}#capabilities`)
  }
</script>

<button
  type="button"
  class="tag"
  onclick={open}
  title="No local audio output — control / media only. View capabilities."
>
  control / media only
</button>

<style>
  .tag {
    display: inline-flex;
    align-items: center;
    padding: 0.1rem 0.5rem;
    border-radius: 999px;
    font-size: var(--text-xs);
    font-weight: 500;
    border: 1px solid var(--border);
    background: var(--surface-3);
    color: var(--text-dim);
    cursor: pointer;
  }
  .tag:hover {
    border-color: var(--accent);
    color: var(--accent-bright);
  }
</style>
