<script lang="ts">
  // Media scope switcher (09 §7): choose the GROUP whose master's data/ folder
  // is browsed (media plays on the group's master — D5 master-side decode), or a
  // raw NODE scope. Reuses the header-with-scope-dropdown idiom from ../media
  // ClusterDetail (the select chrome only); the data binding is GroupRecord +
  // NodeRecord, not media's DeviceView. The chosen scope is written to the route
  // query by the parent (Media.svelte) so Dashboard/Groups can deep-link.
  import type { GroupRecord, NodeRecord } from '../../lib/types'

  type Scope =
    | { kind: 'group'; id: string }
    | { kind: 'node'; id: string }

  interface Props {
    groups: GroupRecord[]
    nodesById: Record<string, NodeRecord>
    scope: Scope
    onChange: (scope: Scope) => void
  }
  let { groups, nodesById, scope, onChange }: Props = $props()

  // The select encodes each option as "kind:id" so one control spans groups +
  // nodes. Groups are primary (media targets a group); nodes are the secondary
  // affordance for browsing a specific node's data/ directly.
  const value = $derived(`${scope.kind}:${scope.id}`)
  const allNodes = $derived(Object.values(nodesById))

  function nodeLabel(id: string): string {
    const n = nodesById[id]
    return n ? n.name || n.id : id
  }

  function onSelect(e: Event) {
    const raw = (e.currentTarget as HTMLSelectElement).value
    const i = raw.indexOf(':')
    const kind = raw.slice(0, i) as Scope['kind']
    const id = raw.slice(i + 1)
    onChange({ kind, id })
  }
</script>

<label class="scope">
  <span class="lbl">scope</span>
  <select {value} onchange={onSelect} aria-label="Media scope">
    {#if groups.length > 0}
      <optgroup label="Groups">
        {#each groups as g (g.id)}
          <option value={`group:${g.id}`}>Group “{g.name || g.id}”</option>
        {/each}
      </optgroup>
    {/if}
    {#if allNodes.length > 0}
      <optgroup label="Nodes">
        {#each allNodes as n (n.id)}
          <option value={`node:${n.id}`}>Node {nodeLabel(n.id)}</option>
        {/each}
      </optgroup>
    {/if}
  </select>
</label>

<style>
  .scope {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--text-muted);
  }
  .lbl {
    text-transform: uppercase;
    font-size: var(--text-xs);
    letter-spacing: 0.04em;
  }
  select {
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 0.35rem 0.6rem;
    font: inherit;
    font-size: var(--text-sm);
  }
  select:hover {
    border-color: var(--border-strong, var(--accent));
  }
</style>
