<script lang="ts">
  // Member transfer (09 §5 flow 2): two panels — this group (left) and the
  // available pool (other groups + Unassigned, right). Baseline is multiselect +
  // move (always works, accessible); drag is a progressive enhancement layered
  // on later if a re-homed lib/dnd.ts appears (P0.4 did not ship one — NEW). The
  // component emits a single move intent per node {nodeId, toGroupId}; the parent
  // turns it into the transactional two-group PATCH (moveNode).
  import Button from '../ui/Button.svelte'
  import type { NodeRecord, GroupRecord } from '../../lib/types'

  interface Props {
    group: GroupRecord
    nodes: Map<string, NodeRecord>
    // pool entries: every node NOT in this group, tagged with its current group
    // (or null for unassigned), so the operator sees where a node would move from.
    pool: { node: NodeRecord; fromGroupId: string | null; fromGroupName: string }[]
    disabled?: boolean
    onMoveOut: (nodeId: string) => void
    onMoveIn: (nodeId: string, fromGroupId: string | null) => void
  }
  let { group, nodes, pool, disabled = false, onMoveOut, onMoveIn }: Props = $props()

  const members = $derived(
    group.memberNodeIds
      .map((id) => nodes.get(id))
      .filter((n): n is NodeRecord => n !== undefined),
  )

  let selectedHere = $state<Set<string>>(new Set())
  let selectedThere = $state<Set<string>>(new Set())

  function toggle(set: Set<string>, id: string): Set<string> {
    const next = new Set(set)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  }

  function moveOutSelected() {
    for (const id of selectedHere) onMoveOut(id)
    selectedHere = new Set()
  }
  function moveInSelected() {
    for (const id of selectedThere) {
      const entry = pool.find((p) => p.node.id === id)
      onMoveIn(id, entry?.fromGroupId ?? null)
    }
    selectedThere = new Set()
  }
</script>

<div class="transfer">
  <div class="panel">
    <h4>In this group</h4>
    <ul>
      {#each members as n (n.id)}
        <li>
          <label class:selected={selectedHere.has(n.id)}>
            <input
              type="checkbox"
              checked={selectedHere.has(n.id)}
              {disabled}
              onchange={() => (selectedHere = toggle(selectedHere, n.id))}
            />
            <span>{n.name || n.id}</span>
          </label>
        </li>
      {:else}
        <li class="empty">No members</li>
      {/each}
    </ul>
    <Button
      variant="ghost"
      disabled={disabled || selectedHere.size === 0}
      onclick={moveOutSelected}
    >
      Move out →
    </Button>
  </div>

  <div class="panel">
    <h4>Available</h4>
    <ul>
      {#each pool as p (p.node.id)}
        <li>
          <label class:selected={selectedThere.has(p.node.id)}>
            <input
              type="checkbox"
              checked={selectedThere.has(p.node.id)}
              {disabled}
              onchange={() => (selectedThere = toggle(selectedThere, p.node.id))}
            />
            <span>{p.node.name || p.node.id}</span>
            <span class="from">{p.fromGroupName}</span>
          </label>
        </li>
      {:else}
        <li class="empty">No other nodes</li>
      {/each}
    </ul>
    <Button
      variant="primary"
      disabled={disabled || selectedThere.size === 0}
      onclick={moveInSelected}
    >
      ← Move in
    </Button>
  </div>
</div>

<style>
  .transfer {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-3);
  }
  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-height: 8rem;
  }
  h4 {
    margin: 0;
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex: 1;
  }
  label {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    color: var(--text-dim);
    cursor: pointer;
  }
  label:hover {
    background: var(--surface-2);
  }
  label.selected {
    background: var(--surface-3);
    color: var(--text);
  }
  input {
    accent-color: var(--accent);
  }
  .from {
    margin-left: auto;
    font-size: var(--text-xs);
    color: var(--text-muted);
    font-style: italic;
  }
  .empty {
    font-size: var(--text-sm);
    color: var(--text-muted);
    padding: var(--space-1) var(--space-2);
  }
</style>
