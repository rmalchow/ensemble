<script lang="ts">
  // Left rail of the Groups screen (09 §5): all groups with member counts, the
  // synthetic "Unassigned" bucket (adopted nodes in no explicit group), and a
  // "New group" CTA. Selection is single; the parent renders the editor for the
  // chosen group. NEW (no ../media analogue — mpvsync has no group model).
  import type { GroupRecord } from '../../lib/types'

  interface Props {
    groups: GroupRecord[]
    unassignedCount: number
    selectedId: string | null
    onSelect: (id: string | null) => void
    onNew: () => void
  }
  let { groups, unassignedCount, selectedId, onSelect, onNew }: Props = $props()
</script>

<nav class="grouplist" aria-label="Groups">
  <ul>
    {#each groups as g (g.id)}
      <li>
        <button
          type="button"
          class:active={selectedId === g.id}
          aria-current={selectedId === g.id ? 'true' : undefined}
          onclick={() => onSelect(g.id)}
        >
          <span class="name">{g.name || g.id}</span>
          <span class="count">{g.memberNodeIds.length}</span>
        </button>
      </li>
    {/each}
    {#if unassignedCount > 0}
      <li>
        <button
          type="button"
          class="unassigned"
          class:active={selectedId === '__unassigned__'}
          onclick={() => onSelect('__unassigned__')}
        >
          <span class="name">Unassigned</span>
          <span class="count">{unassignedCount}</span>
        </button>
      </li>
    {/if}
  </ul>
  <button type="button" class="new" onclick={onNew}>+ New group</button>
</nav>

<style>
  .grouplist {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  li button {
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    border: 1px solid transparent;
    background: transparent;
    color: var(--text-dim);
    font: inherit;
    font-size: var(--text-sm);
    cursor: pointer;
    text-align: left;
  }
  li button:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  li button.active {
    background: var(--surface-3);
    color: var(--text);
    border-color: var(--border);
    font-weight: 600;
  }
  .unassigned .name {
    font-style: italic;
    color: var(--text-muted);
  }
  .count {
    font-size: var(--text-xs);
    color: var(--text-muted);
    background: var(--surface-2);
    border-radius: 999px;
    padding: 0.05rem 0.45rem;
  }
  .new {
    margin-top: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    border: 1px dashed var(--border);
    background: transparent;
    color: var(--accent-bright);
    font: inherit;
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .new:hover {
    background: var(--surface-2);
  }
</style>
