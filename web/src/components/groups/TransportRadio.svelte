<script lang="ts">
  // Transport radio (09 §5): UDP unicast (default) / TCP fallback (D2). 08 §0.7
  // GroupRecord has no transport field, so the MVP surfaces the choice as a
  // local UI control whose change is wired through the parent (it may PATCH the
  // profile once the engine exposes a transport knob). NEW.
  import type { Transport } from '../../lib/types'

  interface Props {
    value: Transport
    disabled?: boolean
    onChange: (t: Transport) => void
  }
  let { value, disabled = false, onChange }: Props = $props()

  const options: { v: Transport; label: string; hint: string }[] = [
    { v: 'udp', label: 'UDP unicast', hint: 'Default — lowest latency' },
    { v: 'tcp', label: 'TCP fallback', hint: 'Lossy networks' },
  ]
</script>

<fieldset class="transport" {disabled}>
  <legend>Transport</legend>
  {#each options as o (o.v)}
    <label class:selected={value === o.v}>
      <input
        type="radio"
        name="transport"
        value={o.v}
        checked={value === o.v}
        {disabled}
        onchange={() => onChange(o.v)}
      />
      <span class="label">{o.label}</span>
      <span class="hint">{o.hint}</span>
    </label>
  {/each}
</fieldset>

<style>
  .transport {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  legend {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
    padding: 0 var(--space-2);
  }
  label {
    display: grid;
    grid-template-columns: auto 1fr;
    grid-template-areas: 'radio label' 'radio hint';
    align-items: center;
    gap: 0 var(--space-2);
    padding: var(--space-2);
    border-radius: var(--radius-sm);
    cursor: pointer;
  }
  label:hover {
    background: var(--surface-2);
  }
  label.selected {
    background: var(--surface-2);
  }
  input {
    grid-area: radio;
    accent-color: var(--accent);
  }
  .label {
    grid-area: label;
    font-size: var(--text-sm);
    color: var(--text);
  }
  .hint {
    grid-area: hint;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  fieldset:disabled {
    opacity: 0.6;
  }
</style>
