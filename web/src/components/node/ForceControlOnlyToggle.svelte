<script lang="ts">
  // "Force control-only (render:false)" checkbox (09 §6). NEW. Checking it sets
  // draft.capabilities.render = false → the node becomes sink-less (variant B),
  // hiding the audio-output controls LIVE via isRenderEnabled/previewRender;
  // clearing it (with a probed sink enabled) restores rendering. Turns a normal
  // node into a control/media-only one (master/origin/clock/UI, but no listener).
  import { setForceControlOnly } from '../../lib/nodeStore'

  interface Props {
    on: boolean
    disabled: boolean
  }
  let { on, disabled }: Props = $props()

  function onChange(e: Event) {
    setForceControlOnly((e.currentTarget as HTMLInputElement).checked)
  }
</script>

<label class="force" class:disabled>
  <input type="checkbox" checked={on} {disabled} onchange={onChange} />
  <span class="text">
    <strong>force control-only (render:false)</strong>
    <span class="hint">
      Turns this into a sink-less node — hides the audio-output controls; the node
      stays usable as master / origin / media store / clock / UI, but is not a
      listener.
    </span>
  </span>
</label>

<style>
  .force {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
    cursor: pointer;
  }
  .force.disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  input {
    margin-top: 0.2rem;
    accent-color: var(--accent);
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  strong {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--text);
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
</style>
