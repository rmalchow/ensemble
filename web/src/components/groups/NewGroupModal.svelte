<script lang="ts">
  // New-group name prompt → POST /api/v1/groups (08 §E.2). Modal overlay +
  // focus/escape handling reuses the ConfirmModal pattern (P0.4); the content is
  // NEW. An empty/whitespace name is blocked client-side (the server also 400s).
  // The parent owns the actual createGroup call + If-Match; this only collects
  // the name and reports busy/error.
  import Button from '../ui/Button.svelte'
  import Field from '../ui/Field.svelte'

  interface Props {
    open: boolean
    busy?: boolean
    error?: string
    onCreate: (name: string) => void
    onCancel: () => void
  }
  let { open, busy = false, error, onCreate, onCancel }: Props = $props()

  let name = $state('')
  let input = $state<HTMLInputElement | null>(null)
  const trimmed = $derived(name.trim())
  const valid = $derived(trimmed.length > 0)

  // Focus + reset the field each time the modal opens.
  $effect(() => {
    if (open) {
      name = ''
      // Defer focus until the input is in the DOM.
      queueMicrotask(() => input?.focus())
    }
  })

  function submit(e: Event) {
    e.preventDefault()
    if (!valid || busy) return
    onCreate(trimmed)
  }
</script>

{#if open}
  <div
    class="backdrop"
    role="button"
    tabindex="-1"
    onclick={onCancel}
    onkeydown={(e) => e.key === 'Escape' && onCancel()}
  >
    <div
      class="dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="new-group-title"
      tabindex="0"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => {
        if (e.key === 'Escape') onCancel()
        e.stopPropagation()
      }}
    >
      <h3 id="new-group-title">New group</h3>
      <form onsubmit={submit}>
        <Field label="Group name" id="new-group-name" error={error}>
          <input
            id="new-group-name"
            type="text"
            bind:this={input}
            bind:value={name}
            placeholder="e.g. Kitchen + Bath"
            autocomplete="off"
            disabled={busy}
          />
        </Field>
        <div class="actions">
          <Button variant="ghost" onclick={onCancel}>Cancel</Button>
          <Button type="submit" loading={busy} disabled={!valid}>Create</Button>
        </div>
      </form>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(1, 4, 9, 0.7);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: var(--z-modal);
  }
  .dialog {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: 1.25rem;
    width: 24rem;
    max-width: calc(100vw - 2rem);
    box-shadow: var(--shadow-modal);
  }
  h3 {
    margin: 0 0 var(--space-4);
    font-size: 1rem;
  }
  input {
    width: 100%;
    padding: 0.5rem 0.65rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--surface-2);
    color: var(--text);
    font: inherit;
  }
  input:focus {
    outline: none;
    border-color: var(--accent);
    box-shadow: 0 0 0 3px var(--focus-ring);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-4);
  }
</style>
