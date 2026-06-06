<script lang="ts">
  // Hardware-delay (HWDelayUs) manual-entry control (09 §6, 06 §5.3). NEW — no
  // mpvsync analogue. Two-way bound slider (0..20000 µs) ↔ numeric µs box, INTEGER
  // µs. The 0..20000 slider extent is the 09 §6 wireframe presentation range (a UI
  // affordance, NOT an A.12 tunable); a value typed BEYOND the slider in the
  // numeric box is preserved (§5.7) — the slider just pins to its end. Edits go
  // through nodeStore.setField('hwDelayUs', …).
  import { setField } from '../../lib/nodeStore'

  interface Props {
    value: number
    disabled: boolean
  }
  let { value, disabled }: Props = $props()

  const SLIDER_MAX = 20000
  const SLIDER_MIN = 0

  // commit rounds to integer µs (HWDelayUs is integer) and stages the edit. The
  // numeric box keeps out-of-slider values; only negatives are clamped to 0.
  function commit(raw: number) {
    if (!Number.isFinite(raw)) return
    const v = Math.max(0, Math.round(raw))
    setField('hwDelayUs', v)
  }

  function onSlider(e: Event) {
    commit(Number((e.currentTarget as HTMLInputElement).value))
  }
  function onNumber(e: Event) {
    commit(Number((e.currentTarget as HTMLInputElement).value))
  }

  // The slider position pins to its range even if the stored value exceeds it.
  const sliderValue = $derived(Math.min(SLIDER_MAX, Math.max(SLIDER_MIN, value)))
</script>

<div class="hwdelay">
  <div class="slider-row">
    <span class="bound">{SLIDER_MIN}</span>
    <input
      type="range"
      min={SLIDER_MIN}
      max={SLIDER_MAX}
      step="1"
      value={sliderValue}
      {disabled}
      oninput={onSlider}
      aria-label="Hardware delay slider in microseconds"
    />
    <span class="bound">{SLIDER_MAX} µs</span>
  </div>
  <label class="num">
    <input
      type="number"
      min={SLIDER_MIN}
      step="1"
      value={value}
      {disabled}
      oninput={onNumber}
      aria-label="Hardware delay in microseconds"
    />
    <span class="unit">µs</span>
  </label>
</div>

<style>
  .hwdelay {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .slider-row {
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
  .num {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }
  input[type='number'] {
    width: 7rem;
    padding: 0.4rem 0.6rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--raised);
    color: var(--text);
    font-family: var(--font-mono);
    font-size: var(--text-sm);
  }
  .unit {
    font-size: var(--text-sm);
    color: var(--text-muted);
  }
</style>
