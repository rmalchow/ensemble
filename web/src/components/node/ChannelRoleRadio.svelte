<script lang="ts">
  // Channel-role radio (09 §6, D13 / 06 §5.1): stereo / left / right. NEW — no
  // mpvsync analogue (mpvsync had no per-node channel model). Two-way: edits go
  // through nodeStore.setField('channel', …). The radios are disabled while the
  // node is offline / a save is in flight (the `disabled` prop).
  import { setField } from '../../lib/nodeStore'
  import type { Channel } from '../../lib/types'

  interface Props {
    value: Channel
    disabled: boolean
  }
  let { value, disabled }: Props = $props()

  const ROLES: { v: Channel; label: string }[] = [
    { v: 'stereo', label: 'stereo' },
    { v: 'left', label: 'left' },
    { v: 'right', label: 'right' },
  ]

  function pick(v: Channel) {
    if (disabled) return
    setField('channel', v)
  }
</script>

<div class="roles" role="radiogroup" aria-label="Channel role">
  {#each ROLES as r (r.v)}
    <label class="role" class:active={value === r.v} class:disabled>
      <input
        type="radio"
        name="channel-role"
        value={r.v}
        checked={value === r.v}
        {disabled}
        onchange={() => pick(r.v)}
      />
      <span>{r.label}</span>
    </label>
  {/each}
</div>

<style>
  .roles {
    display: inline-flex;
    gap: var(--space-2);
  }
  .role {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0.35rem 0.7rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--surface-2);
    color: var(--text-dim);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .role.active {
    border-color: var(--accent);
    color: var(--accent-bright);
    background: rgba(31, 111, 235, 0.12);
  }
  .role.disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  input {
    accent-color: var(--accent);
  }
</style>
