<script>
  // Inline media browser for a room card (J arch §4): plays a source into THIS room
  // (its master sources it directly). Three clearly-separated source types — Local
  // files (tree), Stream URL, Line-in — each gated on the master's reported sources.
  // Rendered only inside the SELECTED card, so there is no node picker and no
  // "playing into" label: the card it lives in IS the target.
  import { bytes, relTime } from "../lib/fmt.js";
  import { nodeById } from "../lib/derive.js";
  import { getMedia, playOnNode } from "../lib/api.js";
  import { entriesFor, crumbs, parentDir, joinDir } from "../lib/tree.js";

  let { snapshot, nodeId } = $props();

  let files = $state([]);
  let url = $state("");
  let loading = $state(false);
  let dir = $state(""); // current directory within the media tree ("" == root)

  // Stream-URL + line-in blocks are hidden for now (kept for when they return /
  // Spotify lands alongside them). Flip to re-enable.
  const streamSourcesEnabled = false;

  let picked = $derived(nodeById(snapshot, nodeId));
  let sources = $derived(picked?.capabilities?.sources ?? []);
  let canHttp = $derived(sources.includes("http"));
  let canInput = $derived(sources.includes("input"));
  let canSpotify = $derived(sources.includes("spotify"));
  let urlValid = $derived(/^https?:\/\/\S+/.test(url.trim()));

  // capture devices on this node: pick which input to play. "" = system default.
  let inputDevices = $derived(picked?.inputDevices ?? []);
  let inputDeviceId = $state("");
  $effect(() => {
    const ids = inputDevices.map((d) => d.id);
    if (!ids.includes(inputDeviceId)) inputDeviceId = inputDevices.length ? inputDevices[0].id : "";
  });

  // directory view derived from the flat file list + current dir (pure, tree.js).
  let view = $derived(entriesFor(files, dir));
  let trail = $derived(crumbs(dir));

  // (re)fetch files + reset to root ONLY when the target node actually changes.
  // lastNode is intentionally non-reactive: it guards against spurious effect
  // re-runs (e.g. an unrelated signal settling a second after mount) that would
  // otherwise throw the user back to the root mid-navigation.
  let lastNode = null;
  $effect(() => {
    const id = nodeId;
    if (id === lastNode) return; // same node → keep the current folder + list
    lastNode = id;
    dir = "";
    files = [];
    if (!id) return;
    loading = true;
    getMedia(id)
      .then((list) => {
        // Spec §6: bare array; tolerate an older node's {files:[...]} envelope.
        if (nodeId === id) files = Array.isArray(list) ? list : Array.isArray(list?.files) ? list.files : [];
      })
      .catch(() => {
        if (nodeId === id) files = [];
      })
      .finally(() => {
        if (nodeId === id) loading = false;
      });
  });

  function playHere(uri) {
    if (nodeId) playOnNode(nodeId, uri).catch(() => {});
  }
  function playFile(f) {
    playHere("file:" + f.path);
  }
  function playUrl() {
    if (urlValid) playHere(url.trim());
  }
  function playInput() {
    playHere("input:" + inputDeviceId);
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

<div class="media">
  <div class="media-block">
    <div class="crumbs row wrap">
      {#each trail as c, i (c.dir)}
        {#if i > 0}<span class="crumb-sep">/</span>{/if}
        {#if i === trail.length - 1}
          <span class="crumb here">{c.name}</span>
        {:else}
          <button class="crumb link" onclick={() => goTo(c.dir)}>{c.name}</button>
        {/if}
      {/each}
    </div>

    {#if view.folders.length === 0 && view.files.length === 0 && dir === ""}
      <div class="empty">{loading ? "" : "No media files."}</div>
    {:else}
      <div class="file-list">
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
            <button class="btn btn-accent" onclick={() => playFile(f)}>Play here</button>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  {#if streamSourcesEnabled && canHttp}
    <div class="media-block">
      <span class="media-label">Stream URL</span>
      <div class="row wrap">
        <input
          type="text"
          placeholder="http(s)://stream-url"
          bind:value={url}
          style="flex: 1; min-width: 200px;"
        />
        <button class="btn btn-accent" disabled={!urlValid} onclick={playUrl}>Play</button>
      </div>
    </div>
  {/if}

  {#if streamSourcesEnabled && canInput}
    <div class="media-block">
      <span class="media-label">Line-in</span>
      <div class="row wrap">
        {#if inputDevices.length > 0}
          <select bind:value={inputDeviceId} aria-label="input device" title="capture device">
            {#each inputDevices as d (d.id)}
              <option value={d.id}>{d.desc}</option>
            {/each}
          </select>
        {/if}
        <button class="btn" onclick={playInput}>Play line-in</button>
      </div>
    </div>
  {/if}
</div>

<style>
  /* the media block is set off from the card's other rows by a rule above and
     below, with vertical breathing room. */
  .media {
    display: flex;
    flex-direction: column;
    gap: 10px;
    margin: 8px 0;
    padding: 12px 0;
    border-top: 1px solid var(--border);
    border-bottom: 1px solid var(--border);
  }
  /* each source type is a labeled block, separated by a thin rule */
  .media-block {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .media-block + .media-block {
    border-top: 1px solid var(--border);
    padding-top: 10px;
  }
  .media-label {
    color: var(--muted);
    font-size: 12px;
  }

  /* The file list scrolls internally so a large library never makes the card
     (and the whole page) grow unbounded. Crumbs stay fixed above it. */
  .file-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
    max-height: 320px;
    overflow-y: auto;
    /* room for the scrollbar so rows don't jump under it */
    padding-right: 4px;
  }
</style>
