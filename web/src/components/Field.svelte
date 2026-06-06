<script lang="ts">
  // Self-contained labelled text/password input for the auth + settings forms
  // (P1.4 §2). Dark-mode styled from tokens. The value is two-way bound; the
  // host owns validation and passes `error` to flip the field into its error
  // treatment with an inline message. (The ui/Field wrapper is for composing an
  // arbitrary control under a label; this one IS the control, the common case.)
  interface Props {
    label: string
    id: string
    value: string
    type?: 'text' | 'password' | 'email'
    placeholder?: string
    autocomplete?: string
    hint?: string
    error?: string
    disabled?: boolean
    required?: boolean
    oninput?: (v: string) => void
  }
  let {
    label,
    id,
    value = $bindable(''),
    type = 'text',
    placeholder = '',
    autocomplete = 'off',
    hint,
    error,
    disabled = false,
    required = false,
    oninput,
  }: Props = $props()

  function handle(e: Event) {
    value = (e.target as HTMLInputElement).value
    oninput?.(value)
  }
</script>

<div class="field" class:has-error={!!error}>
  <label for={id}>{label}{#if required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
  {#if type === 'password'}
    <input
      {id}
      type="password"
      {placeholder}
      autocomplete={autocomplete as AutoFill}
      {disabled}
      {required}
      {value}
      aria-invalid={error ? 'true' : undefined}
      oninput={handle}
    />
  {:else}
    <input
      {id}
      type="text"
      inputmode={type === 'email' ? 'email' : undefined}
      {placeholder}
      autocomplete={autocomplete as AutoFill}
      {disabled}
      {required}
      {value}
      aria-invalid={error ? 'true' : undefined}
      oninput={handle}
    />
  {/if}
  {#if error}
    <p class="error" role="alert">{error}</p>
  {:else if hint}
    <p class="hint">{hint}</p>
  {/if}
</div>

<style>
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  label {
    font-size: var(--text-sm);
    color: var(--text-dim);
    font-weight: 500;
  }
  .req {
    color: var(--accent-bright);
  }
  input {
    background: var(--raised);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0.5rem 0.65rem;
    color: var(--text);
    font: inherit;
  }
  input:focus {
    outline: none;
    border-color: var(--accent-bright);
    box-shadow: 0 0 0 3px var(--focus-ring);
  }
  input:disabled {
    opacity: 0.6;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
    margin: 0;
  }
  .error {
    font-size: var(--text-xs);
    color: var(--danger-bright);
    margin: 0;
  }
  .has-error input {
    border-color: var(--danger);
  }
</style>
