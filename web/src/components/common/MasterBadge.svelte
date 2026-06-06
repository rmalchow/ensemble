<script lang="ts">
  // Master marker shown on the group card header + the master's member row
  // (09 §3). When the master is sink-less (caps.render === false) it renders the
  // "⊘ master (no local audio)" form — the node originates the stream + serves
  // the clock but plays nothing locally (D17, 04 §4.2). Shared with Cluster /
  // Node-detail per the 09 cross-ref (this piece exposes it).
  import Chip from '../ui/Chip.svelte'
  import type { NodeRecord } from '../../lib/types'

  interface Props {
    node?: NodeRecord
  }
  let { node }: Props = $props()

  // A node with no audio sink (render:false) is a control/media-only master.
  const noAudio = $derived(node !== undefined && node.caps?.render === false)
</script>

{#if noAudio}
  <Chip tone="warn">⊘ master (no local audio)</Chip>
{:else}
  <Chip tone="accent">★ master</Chip>
{/if}
