<script>
  // Media section (J arch §4): node picker → file list → Play here, plus URL
  // and Input play paths gated on the picked node's reported sources (§6.1).
  import { bytes, relTime } from "../lib/fmt.js";
  import { nodeById, activeGroup } from "../lib/derive.js";
  import { getMedia, playOnNode } from "../lib/api.js";
  import { cluster } from "../lib/ws.svelte.js";
  import { entriesFor, crumbs, parentDir, joinDir } from "../lib/tree.js";

  let { snapshot, self, selectedMaster, selectTick } = $props();

  let pickedNodeId = $state("");
  let files = $state([]);
  let url = $state("");
  let loading = $state(false);
  // current directory within the picked node's media tree ("" == root).
  let dir = $state("");

  // Clicking a group card (App selectTick bump) points the picker at that
  // group's MASTER. Tracked by tick so re-selecting the same group re-applies,
  // while a manual pick from the dropdown below still sticks in between.
  let appliedTick = -1;
  $effect(() => {
    if (selectTick !== appliedTick) {
      appliedTick = selectTick;
      if (selectedMaster) pickedNodeId = selectedMaster;
    }
  });

  // Before any explicit selection, default to the active group's master
  // (a playing group, else self's group, else the first).
  $effect(() => {
    if (!pickedNodeId) {
      const g = activeGroup(snapshot, self.id);
      pickedNodeId = (g && g.master) || self.id || "";
    }
  });

  // nodes that can decode local media (non-empty formats).
  let mediaNodes = $derived(
    (snapshot.nodes || []).filter(
      (n) => (n.capabilities?.formats ?? []).length > 0,
    ),
  );

  let picked = $derived(nodeById(snapshot, pickedNodeId));
  let sources = $derived(picked?.capabilities?.sources ?? []);
  let canHttp = $derived(sources.includes("http"));
  let canInput = $derived(sources.includes("input"));
  let urlValid = $derived(/^https?:\/\/\S+/.test(url.trim()));

  // directory view derived from the flat file list + current dir (pure, tree.js).
  let view = $derived(entriesFor(files, dir));
  let trail = $derived(crumbs(dir));

  // refetch files when the picked node changes; reset to the root directory.
  $effect(() => {
    const id = pickedNodeId;
    dir = "";
    if (!id) {
      files = [];
      return;
    }
    loading = true;
    getMedia(id)
      .then((list) => {
        // Spec §6: bare array; tolerate an older node's {files:[...]} envelope.
        if (pickedNodeId === id) files = Array.isArray(list) ? list : Array.isArray(list?.files) ? list.files : [];
      })
      .catch(() => {
        if (pickedNodeId === id) files = [];
      })
      .finally(() => {
        if (pickedNodeId === id) loading = false;
      });
  });

  // playHere takes over the picked node when it's a follower, then plays. The
  // live snapshot comes from the ws store (playOnNode polls it to confirm the
  // takeover landed before issuing /play). §5.2 / J §4.
  function playHere(uri) {
    const name = picked?.name;
    playOnNode(pickedNodeId, uri, () => cluster.snapshot, { name }).catch(
      () => {},
    );
  }
  function playFile(f) {
    playHere("file:" + f.path);
  }
  function playUrl() {
    if (urlValid) playHere(url.trim());
  }
  function playInput() {
    playHere("input:");
  }

  function enter(folder) {
    dir = joinDir(dir, folder.name);
  }
  function goUp() {
    dir = parentDir(dir);
  }
  function goTo(d) {
    dir = d;
  }
</script>

<section class="section">
  <h2>Media</h2>
  <div class="card">
    <div class="row wrap" style="margin-bottom: 10px;">
      <label class="row">
        Node
        <select bind:value={pickedNodeId}>
          {#each mediaNodes as n (n.id)}
            <option value={n.id}>{n.name}</option>
          {/each}
        </select>
      </label>
      {#if loading}<span class="muted small">loading…</span>{/if}
    </div>

    {#if canHttp}
      <div class="row wrap" style="margin-bottom: 8px;">
        <input
          type="text"
          placeholder="http(s)://stream-url"
          bind:value={url}
          style="flex: 1; min-width: 200px;"
        />
        <button class="btn btn-accent" disabled={!urlValid} onclick={playUrl}>
          Play URL
        </button>
      </div>
    {/if}

    {#if canInput}
      <div class="row" style="margin-bottom: 8px;">
        <button class="btn" onclick={playInput}>Input (line-in / mic)</button>
      </div>
    {/if}

    <div class="crumbs row wrap">
      {#each trail as c, i (c.dir)}
        {#if i > 0}<span class="crumb-sep">/</span>{/if}
        {#if i === trail.length - 1}
          <span class="crumb here">{c.name}</span>
        {:else}
          <button class="crumb link" onclick={() => goTo(c.dir)}>
            {c.name}
          </button>
        {/if}
      {/each}
    </div>

    {#if view.folders.length === 0 && view.files.length === 0 && dir === ""}
      <div class="empty">{loading ? "" : "No media files."}</div>
    {:else}
      {#if dir !== ""}
        <button class="media-file folder-row up" onclick={goUp}>
          <span class="glyph">▸</span>
          <span>..</span>
        </button>
      {/if}
      {#each view.folders as folder (folder.name)}
        <button class="media-file folder-row" onclick={() => enter(folder)}>
          <span class="glyph">📁</span>
          <span>{folder.name}</span>
          <span class="muted small">{folder.count} file{folder.count === 1 ? "" : "s"}</span>
        </button>
      {/each}
      {#each view.files as f (f.path)}
        <div class="media-file">
          <span title={f.path}>{f.name}</span>
          <span class="muted small">{bytes(f.sizeBytes)}</span>
          <span class="muted small">{relTime(f.modTime)}</span>
          <span class="spacer"></span>
          <button class="btn btn-accent" onclick={() => playFile(f)}>
            Play here
          </button>
        </div>
      {/each}
    {/if}
  </div>
</section>
