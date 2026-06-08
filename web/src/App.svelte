<script>
  import { onMount } from "svelte";
  import { cluster, connect, disconnect } from "./lib/ws.svelte.js";
  import { getStatus, getCluster, setSelfId } from "./lib/api.js";
  import { deriveRole } from "./lib/derive.js";
  import Groups from "./sections/Groups.svelte";
  import Nodes from "./sections/Nodes.svelte";
  import Media from "./sections/Media.svelte";
  import Toast from "./components/Toast.svelte";

  // self {id, name, role}; id/name seeded once from GET /api/status. The role
  // is derived LIVE from the cluster snapshot on every ws frame (the boot-status
  // role goes stale — e.g. shows "solo" after this node joins a group).
  let self = $state({ id: "", name: "", role: "" });

  // live role from the snapshot (falls back to the boot-status role until self
  // is resolvable in the snapshot).
  let role = $derived(deriveRole(cluster.snapshot, self.id, self.role));

  // re-render trigger for the "stale connection" hint (no interval needed —
  // heartbeats drive re-render; this just reads receivedAt).
  let stale = $derived(
    cluster.receivedAt > 0 && Date.now() - cluster.receivedAt > 10000,
  );

  // Two pages via hash routing (bulletproof under the embedded SPA — no server
  // route needed): "" → overview (groups + media), "nodes" → the node list.
  let route = $state(parseHash());
  function parseHash() {
    return location.hash.replace(/^#\/?/, "");
  }
  function onHash() {
    route = parseHash();
  }

  // Clicking a group card selects it → the Media section below shows that
  // group's MASTER library. group.id === group.master (D42), so the selected
  // master id doubles as the selected-group highlight key. selectTick bumps on
  // every click so re-selecting the same group (after manually changing the
  // media picker) re-applies it.
  let selectedMaster = $state("");
  let selectTick = $state(0);
  function selectGroup(masterId) {
    selectedMaster = masterId;
    selectTick += 1;
  }

  onMount(() => {
    window.addEventListener("hashchange", onHash);
    (async () => {
      try {
        const s = await getStatus();
        if (s) {
          self = { id: s.id || "", name: s.name || "", role: s.role || "" };
          setSelfId(self.id);
        }
      } catch {
        // non-fatal: "this node" markers simply absent
      }
      try {
        const snap = await getCluster();
        if (snap) cluster.snapshot = snap;
      } catch {
        // non-fatal: ws will seed
      }
      connect();
    })();

    return () => {
      window.removeEventListener("hashchange", onHash);
      disconnect();
    };
  });
</script>

<header class="app-header">
  <span class="title">ensemble</span>
  <span class="self">
    {#if self.name}
      {self.name}
      {#if role}<span class="badge">{role}</span>{/if}
    {:else}
      …
    {/if}
  </span>
  <span class="spacer"></span>
  {#if stale}<span class="muted small">stale</span>{/if}
  <span class="dot {cluster.status}" title={cluster.status}></span>
  <span class="muted small">{cluster.status}</span>
  {#if route === "nodes"}
    <a class="iconlink" href="#/" title="Back to groups" aria-label="Back to groups">←</a>
  {:else}
    <a class="iconlink" href="#/nodes" title="Nodes" aria-label="Nodes">⚙</a>
  {/if}
</header>

<Toast />

{#if route === "nodes"}
  <Nodes snapshot={cluster.snapshot} {self} />
{:else}
  <Groups
    snapshot={cluster.snapshot}
    {self}
    {selectedMaster}
    onselect={selectGroup}
  />
  <Media snapshot={cluster.snapshot} {self} {selectedMaster} {selectTick} />
{/if}
