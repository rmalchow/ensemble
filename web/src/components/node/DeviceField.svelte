<script lang="ts">
  // Audio-output device override (09 §6, 06 §1): a free-form device string
  // persisted on the NodeRecord (gossiped; the owning node re-opens its sink on
  // change). Empty = auto (the backend default / the node's --device flag). The
  // node's probed sink backends (caps.sinks) are offered as datalist
  // suggestions when known. Edits go through nodeStore.setField('device', …).
  import { setField } from '../../lib/nodeStore'
  import type { NodeDetailView } from '../../lib/node'

  interface Props {
    value: string
    node: NodeDetailView
    disabled: boolean
  }
  let { value, node, disabled }: Props = $props()

  function onInput(e: Event) {
    setField('device', (e.currentTarget as HTMLInputElement).value)
  }

  // The node's self-probed playback devices (id + human label) are the primary
  // suggestions; the probed backend names are the (rarely useful) fallback.
  const suggestions = $derived(
    node.audioDevices?.length
      ? node.audioDevices
      : (node.caps?.sinks ?? []).map((s) => ({ id: s, label: undefined })),
  )
  const listId = $derived(`device-suggestions-${node.id}`)
</script>

<div class="device">
  <input
    type="text"
    id="audio-device"
    aria-label="Audio output device"
    placeholder="auto (backend default)"
    autocomplete="off"
    list={suggestions.length > 0 ? listId : undefined}
    {value}
    {disabled}
    oninput={onInput}
  />
  {#if suggestions.length > 0}
    <datalist id={listId}>
      {#each suggestions as s (s.id)}
        <option value={s.id} label={s.label}></option>
      {/each}
    </datalist>
  {/if}
</div>

<style>
  .device input {
    width: 100%;
    max-width: 24rem;
    padding: 0.45rem 0.6rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--raised);
    color: var(--text);
    font-family: var(--font-mono);
    font-size: var(--text-sm);
  }
  .device input::placeholder {
    color: var(--text-dim);
    font-family: inherit;
  }
</style>
