<script>
  // Shown when the node SERVING this page goes unreachable (the websocket stays
  // down). It offers links to peer nodes so the user can re-open the UI elsewhere.
  // Crucially, every candidate is REACHABILITY-TESTED from the browser before it's
  // shown — a node advertises several CIDRs (LAN, docker, link-local…) and most are
  // not reachable from here, so we probe and list only those that actually respond.
  import { knownNodes } from "../lib/ws.svelte.js";

  let { self } = $props();

  let candidates = $state([]); // [{ name, url }] confirmed reachable
  let probing = $state(true);

  // A no-cors GET can't read the response, but it resolves on a live connection and
  // rejects on a network/timeout failure — exactly the liveness signal we want, and
  // it needs no CORS headers on the peer.
  async function reachable(origin, timeoutMs = 2500) {
    try {
      await fetch(origin + "/api/status", {
        mode: "no-cors",
        cache: "no-store",
        signal: AbortSignal.timeout(timeoutMs),
      });
      return true;
    } catch {
      return false;
    }
  }

  async function probeNode(node) {
    for (const origin of node.origins) {
      if (await reachable(origin)) return { name: node.name || origin, url: origin };
    }
    return null;
  }

  async function probeAll() {
    const others = knownNodes().filter((n) => n.id && n.id !== self.id);
    if (!others.length) {
      candidates = [];
      probing = false;
      return;
    }
    probing = true;
    const found = (await Promise.all(others.map(probeNode))).filter(Boolean);
    candidates = found;
    probing = false;
  }

  $effect(() => {
    void self.id; // re-probe if identity resolves later
    probeAll();
    const id = setInterval(probeAll, 6000);
    return () => clearInterval(id);
  });
</script>

<div class="unreachable" role="alert">
  <div class="u-title">
    <strong>{self.name || "This node"}</strong> is unreachable.
  </div>
  {#if candidates.length}
    <div class="u-body">
      Try another node:
      <span class="u-links">
        {#each candidates as c (c.url)}
          <a class="u-link" href={c.url}>{c.name}</a>
        {/each}
      </span>
    </div>
  {:else if probing}
    <div class="u-body small">Looking for reachable nodes…</div>
  {:else}
    <div class="u-body small">No other nodes are reachable right now.</div>
  {/if}
</div>

<style>
  .unreachable {
    margin: 12px 0;
    padding: 12px 16px;
    border-radius: 10px;
    border: 1px solid color-mix(in srgb, var(--danger) 55%, transparent);
    background: color-mix(in srgb, var(--danger) 12%, transparent);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .u-title {
    font-size: 15px;
  }
  .u-body {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
    color: var(--muted);
  }
  .u-links {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 8px;
  }
  .u-link {
    display: inline-flex;
    align-items: center;
    padding: 3px 10px;
    border-radius: 6px;
    border: 1px solid var(--border);
    color: var(--fg);
    text-decoration: none;
  }
  .u-link:hover {
    border-color: color-mix(in srgb, var(--accent) 70%, transparent);
    box-shadow:
      0 0 0 3px color-mix(in srgb, var(--accent) 16%, transparent),
      0 0 22px -6px color-mix(in srgb, var(--accent) 45%, transparent);
  }
</style>
