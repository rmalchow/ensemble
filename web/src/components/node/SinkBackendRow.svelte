<script lang="ts">
  // One sink row in the Capabilities panel: backend name + a precise/coarse tag
  // (09 §6, 06 §1.1 — alsa/pipewire precise, exec:* coarse via Backend.Precise /
  // Delay() ok=false). Pure presentation; the enable/disable checkbox is the
  // sibling CapabilityToggleRow, so this is just the labelled tag.
  import Chip from '../ui/Chip.svelte'
  import { sinkTier, type SinkTier } from '../../lib/caps'

  interface Props {
    name: string
    // tier may be supplied (e.g. from a per-sink precise flag) or derived by name.
    tier?: SinkTier
  }
  let { name, tier }: Props = $props()

  const t = $derived(tier ?? sinkTier(name))
  const detail = $derived(
    name === 'alsa'
      ? 'precise — snd_pcm_delay'
      : t === 'coarse'
        ? 'coarse — Delay() ok=false'
        : 'precise',
  )
</script>

<span class="sink">
  <code class="name">{name}</code>
  <Chip tone={t === 'precise' ? 'success' : 'muted'}>{detail}</Chip>
</span>

<style>
  .sink {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }
  .name {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--text);
  }
</style>
