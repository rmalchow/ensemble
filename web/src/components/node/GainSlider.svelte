<script lang="ts">
  // Gain (dB) trim slider (09 §6, 06 §5.2 GainDB). NEW — no mpvsync analogue. The
  // −6..+6 dB extent is the 09 §6 wireframe PRESENTATION range (a UI affordance,
  // NOT an A.12 tunable); the stored value is an unbounded float the server
  // 422-guards. Edits go through nodeStore.setField('gainDb', …).
  import { setField } from '../../lib/nodeStore'

  interface Props {
    value: number
    disabled: boolean
  }
  let { value, disabled }: Props = $props()

  const MIN = -6
  const MAX = 6

  function onInput(e: Event) {
    const v = Number((e.currentTarget as HTMLInputElement).value)
    if (Number.isFinite(v)) setField('gainDb', v)
  }

  // Format with a sign so −1.5 / +0.0 / +3.0 read clearly.
  const readout = $derived(
    `${value > 0 ? '+' : value < 0 ? '' : '+'}${value.toFixed(1)} dB`,
  )
</script>

<div class="gain">
  <span class="bound">{MIN}</span>
  <input
    type="range"
    min={MIN}
    max={MAX}
    step="0.5"
    value={value}
    {disabled}
    oninput={onInput}
    aria-label="Gain in decibels"
  />
  <span class="bound">+{MAX}</span>
  <span class="readout mono">{readout}</span>
</div>

<style>
  .gain {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  input[type='range'] {
    flex: 1;
    accent-color: var(--accent);
  }
  .bound {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .readout {
    min-width: 4.5rem;
    text-align: right;
    color: var(--text);
    font-size: var(--text-sm);
  }
  .mono {
    font-family: var(--font-mono);
    font-variant-numeric: tabular-nums;
  }
</style>
