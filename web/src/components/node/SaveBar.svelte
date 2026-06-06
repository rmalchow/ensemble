<script lang="ts">
  // Dirty-state Save / Revert bar (09 §6). Save threads If-Match on the PATCH
  // (handled by nodeStore.save → patchNode). Both buttons are inert when the form
  // is clean; Save spins while in flight. The screen owns the actual save/revert
  // handlers + 409 handling — this is just the affordance.
  import Button from '../ui/Button.svelte'

  interface Props {
    dirty: boolean
    saving: boolean
    onSave: () => void
    onRevert: () => void
  }
  let { dirty, saving, onSave, onRevert }: Props = $props()
</script>

<div class="savebar" class:active={dirty}>
  {#if dirty}
    <span class="hint">Unsaved changes</span>
  {/if}
  <Button variant="ghost" disabled={!dirty || saving} onclick={onRevert}>
    Revert
  </Button>
  <Button variant="primary" disabled={!dirty} loading={saving} onclick={onSave}>
    Save changes
  </Button>
</div>

<style>
  .savebar {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--space-3);
    padding-top: var(--space-3);
    border-top: 1px solid var(--border-subtle);
  }
  .hint {
    margin-right: auto;
    font-size: var(--text-sm);
    color: var(--warn-bright);
  }
</style>
