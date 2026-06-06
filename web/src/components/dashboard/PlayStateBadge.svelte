<script lang="ts">
  // "▶ playing" / "◼ stopped" pill driven by the group's play state (09 §3).
  // Reads the live status's playing when present (authoritative mid-transition),
  // else the stored GroupRecord.playing bool.
  import Chip from '../ui/Chip.svelte'

  interface Props {
    playing: boolean
  }
  let { playing }: Props = $props()
</script>

{#if playing}
  <Chip tone="success">▶ playing</Chip>
{:else}
  <Chip tone="muted">◼ stopped</Chip>
{/if}
