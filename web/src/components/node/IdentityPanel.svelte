<script lang="ts">
  // Identity panel (09 §6): editable display name (rename → setField('name',…)),
  // read-only id + current group, and a master marker that appends "(no local
  // audio)" when the node is its group's master AND sink-less (D17). Rename is the
  // one edit allowed while OFFLINE (config-only) — the screen labels it "applies
  // when node returns" via the `offline` prop.
  import Field from '../ui/Field.svelte'
  import Chip from '../ui/Chip.svelte'
  import { setField } from '../../lib/nodeStore'
  import type { NodeDetailView } from '../../lib/node'

  interface Props {
    node: NodeDetailView
    // draftName is the live edited value (from the store draft), so the input
    // reflects pending edits + Revert. Falls back to the loaded name.
    draftName: string
    offline: boolean
  }
  let { node, draftName, offline }: Props = $props()

  const masterNoAudio = $derived(node.isMaster && node.caps.render === false)

  function onName(e: Event) {
    setField('name', (e.currentTarget as HTMLInputElement).value)
  }
</script>

<div class="identity">
  <Field
    label="name"
    id="node-name"
    hint={offline ? 'applies when node returns' : undefined}
  >
    <input
      id="node-name"
      type="text"
      value={draftName}
      oninput={onName}
      autocomplete="off"
      placeholder={node.id}
    />
  </Field>

  <div class="meta">
    <div class="row">
      <span class="label">id</span>
      <code class="mono id">{node.id}</code>
    </div>
    <div class="row">
      <span class="label">group</span>
      {#if node.groupId}
        <span class="group">{node.groupId}</span>
      {:else}
        <span class="dash">— (no group)</span>
      {/if}
      {#if node.isMaster}
        {#if masterNoAudio}
          <Chip tone="warn">★ master (no local audio)</Chip>
        {:else}
          <Chip tone="accent">★ master</Chip>
        {/if}
      {/if}
    </div>
  </div>
</div>

<style>
  .identity {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  input {
    padding: 0.45rem 0.6rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--raised);
    color: var(--text);
    font-size: var(--text-sm);
  }
  .meta {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .label {
    width: 3rem;
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .mono {
    font-family: var(--font-mono);
  }
  .id {
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  .group {
    font-size: var(--text-sm);
    color: var(--text);
  }
  .dash {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
</style>
