<script>
  import { onMount } from "svelte";
  import { cluster, connect, disconnect } from "./lib/ws.svelte.js";
  import { getStatus, getCluster, setSelfId } from "./lib/api.js";
  import { startStatsPolling, stopStatsPolling } from "./lib/stats.svelte.js";
  import Groups from "./sections/Groups.svelte";
  import Nodes from "./sections/Nodes.svelte";
  import Toast from "./components/Toast.svelte";
  import UnreachableBanner from "./components/UnreachableBanner.svelte";
  import NodeSwitcher from "./components/NodeSwitcher.svelte";
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

  // Connection health folded into one signal for the node-name pill: green when
  // open + fresh, red when the socket is closed, amber in between (connecting /
  // reconnecting / stale data).
  let statusLevel = $derived(
    cluster.status === "closed"
      ? "bad"
      : cluster.status === "open" && !stale
        ? "good"
        : "warn",
  );
  let statusTitle = $derived(stale ? cluster.status + " · stale" : cluster.status);

  // When the serving node stays unreachable past a short grace (so brief reconnect
  // blips don't flash), surface the fallback banner with links to reachable peers.
  let showFallback = $state(false);
  $effect(() => {
    if (cluster.status === "open") {
      showFallback = false;
      return;
    }
    const id = setTimeout(() => (showFallback = true), 5000);
    return () => clearTimeout(id);
  });

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
      startStatsPolling();
    })();

    return () => {
      window.removeEventListener("hashchange", onHash);
      disconnect();
      stopStatsPolling();
    };
  });
</script>

<header class="app-header">
  <span class="brand-mark">
    <img class="wordmark" src={wordmark} alt="ensemble" />
    <span class="brand-dot"></span>
  </span>
  <span class="spacer"></span>
  <div class="header-right">
    {#if route === "nodes"}
      <a class="iconlink" href="#/" title="Back to rooms" aria-label="Back to rooms">←</a>
    {:else}
      <a class="iconlink" href="#/nodes" title="Nodes" aria-label="Nodes">⚙</a>
    {/if}
    <NodeSwitcher snapshot={cluster.snapshot} {self} {statusLevel} {statusTitle} />
  </div>
</header>

{#if showFallback}
  <UnreachableBanner {self} />
{/if}

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
