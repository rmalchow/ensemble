<script>
  // Spotify Connect endpoints editor (D57), shown on a node that runs go-librespot.
  // The DEFAULT device ("ensemble <node>") is implicit + read-only (legacy
  // behavior). Below it, the operator manages named PRESETS: each is a name + a
  // row of toggleable players (speakers). Playing to a preset's Connect device
  // regroups those players and plays. All edits persist via PATCH /node, which
  // reconciles the live bridges (start/stop/rename) on the target node.
  import EditableText from "./EditableText.svelte";
  import { setSpotifyEndpoints } from "../lib/api.js";

  let { node, snapshot } = $props();

  // Local editable copy — source of truth while the editor is open. Seeded ONCE
  // from the node record (guarded like MediaBrowser) so snapshot ticks never clobber
  // an in-progress edit; ids are client-generated + stable so the backend keeps them.
  let seeded = false;
  let endpoints = $state([]);
  $effect(() => {
    if (seeded) return;
    seeded = true;
    endpoints = (node.spotifyEndpoints ?? []).map((e) => ({
      id: e.id,
      name: e.name,
      players: [...(e.players ?? [])],
    }));
  });

  // Player candidates: alive, playback-capable nodes (the speakers).
  let players = $derived(
    (snapshot?.nodes ?? [])
      .filter((n) => n.alive && n.capabilities && n.capabilities.playback)
      .sort((a, b) => (a.name || "").localeCompare(b.name || "")),
  );

  let baseName = $derived("ensemble " + (node.name || "node"));

  function save() {
    setSpotifyEndpoints(node.id, endpoints).catch(() => {});
  }
  function newId() {
    // stable slug the backend keeps as-is (normalize lowercases + keeps [a-z0-9-]).
    const r =
      (globalThis.crypto && crypto.randomUUID && crypto.randomUUID()) ||
      Math.random().toString(16).slice(2);
    return "ep-" + r.replace(/-/g, "").slice(0, 8);
  }

  function addEndpoint() {
    endpoints = [...endpoints, { id: newId(), name: "New endpoint", players: [] }];
    save();
  }
  function removeEndpoint(i) {
    endpoints = endpoints.filter((_, j) => j !== i);
    save();
  }
  function renameEndpoint(i, name) {
    endpoints[i].name = name;
    save();
  }
  function togglePlayer(i, pid) {
    const ep = endpoints[i];
    ep.players = ep.players.includes(pid)
      ? ep.players.filter((x) => x !== pid)
      : [...ep.players, pid];
    save();
  }
</script>

<div class="spotify-endpoints">
  <div class="ep default">
    <span class="ep-name">{baseName}</span>
    <span class="muted small">default · plays this node's current group</span>
  </div>

  {#each endpoints as ep, i (ep.id)}
    <div class="ep">
      <div class="ep-head">
        <span class="muted small">{baseName}:</span>
        <EditableText value={ep.name} onsave={(v) => renameEndpoint(i, v)} placeholder="endpoint name" />
        <span class="spacer"></span>
        <button class="btn btn-danger ep-remove" title="remove endpoint" onclick={() => removeEndpoint(i)}>
          ✕
        </button>
      </div>
      {#if players.length === 0}
        <span class="muted small">No playback-capable nodes yet.</span>
      {:else}
        <div class="row wrap players">
          {#each players as p (p.id)}
            {@const on = ep.players.includes(p.id)}
            <button
              type="button"
              class="chip player"
              class:on
              aria-pressed={on}
              title={on ? `remove ${p.name}` : `add ${p.name}`}
              onclick={() => togglePlayer(i, p.id)}
            >
              <span class="glyph" aria-hidden="true">{on ? "●" : "○"}</span>{p.name}
            </button>
          {/each}
        </div>
      {/if}
    </div>
  {/each}

  <button class="btn add-ep" onclick={addEndpoint}>+ Add endpoint</button>
</div>

<style>
  .spotify-endpoints {
    display: flex;
    flex-direction: column;
  }
  .ep {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 8px 0;
    border-bottom: 1px solid var(--border);
  }
  .ep.default {
    flex-direction: row;
    align-items: baseline;
    gap: 8px;
  }
  .ep-name {
    font-size: 14px;
  }
  .ep-head {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .ep-head .spacer {
    flex: 1;
  }
  .ep-remove {
    padding: 2px 8px;
    line-height: 1;
  }
  .players {
    gap: 6px;
  }
  .chip.player {
    cursor: pointer;
    background: var(--panel-2);
    border: 1px solid var(--border);
  }
  .chip.player.on {
    background: #15803d;
    border-color: #15803d;
    color: #fff;
  }
  .chip.player.on:hover {
    background: #166534;
  }
  .chip.player .glyph {
    margin-right: 4px;
  }
  .add-ep {
    margin-top: 8px;
  }
</style>
