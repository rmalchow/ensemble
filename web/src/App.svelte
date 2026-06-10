<script>
  import { onMount } from "svelte";
  import { cluster, connect, disconnect } from "./lib/ws.svelte.js";
  import { getStatus, getCluster, setSelfId } from "./lib/api.js";
  import Groups from "./sections/Groups.svelte";
  import Nodes from "./sections/Nodes.svelte";
  import Toast from "./components/Toast.svelte";
  // wordmark-small.png is rendered near display size (crisp); wordmark.png is the
  // full-size master, kept for future high-res use but not referenced here.
  import wordmark from "./assets/wordmark-small.png";

  // self {id, name}; id/name seeded once from GET /api/status.
  let self = $state({ id: "", name: "", role: "" });

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

  // Clicking a group card selects it → that card reveals its operational controls
  // (assign roster, inline media browser, settings). group.id === group.master
  // (D42), so the selected master id doubles as the selected-group highlight key.
  let selectedMaster = $state("");
  function selectGroup(masterId) {
    selectedMaster = masterId;
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
  <div class="brand">
    <span class="brand-mark">
      <img class="wordmark" src={wordmark} alt="ensemble" />
      <span class="brand-dot"></span>
    </span>
    <span class="self">{self.name || "…"}</span>
  </div>
  <span class="spacer"></span>
  {#if stale}<span class="muted small">stale</span>{/if}
  <!-- Rams: the dot already communicates connection state (title on hover); the
       adjacent text was redundant. -->
  <span class="dot {cluster.status}" title={cluster.status}></span>
  {#if route === "nodes"}
    <a class="iconlink" href="#/" title="Back to rooms" aria-label="Back to rooms">←</a>
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
{/if}
