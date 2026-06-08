<script>
  // Groups section (J arch §4): one GroupCard per derived group.
  import GroupCard from "../components/GroupCard.svelte";

  let { snapshot, self, selectedMaster, onselect } = $props();

  // named first, then by id.
  let groups = $derived(
    [...(snapshot.groups || [])].sort((a, b) => {
      const an = a.name ? 0 : 1;
      const bn = b.name ? 0 : 1;
      if (an !== bn) return an - bn;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    }),
  );
</script>

<section class="section">
  <h2>Groups</h2>
  {#if groups.length === 0}
    <div class="empty">No groups yet.</div>
  {:else}
    {#each groups as group (group.id)}
      <GroupCard
        {group}
        {snapshot}
        {self}
        selected={group.id === selectedMaster}
        {onselect}
      />
    {/each}
  {/if}
</section>
