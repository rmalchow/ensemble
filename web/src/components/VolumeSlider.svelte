<script>
  // 0–100% range → debounced setVolume (J arch §4 / D35). The held pct tracks
  // the thumb while dragging; a fresh snapshot re-syncs once released. A
  // last-sent guard makes repeat sends of the same value impossible (a stray
  // re-fire or a snapshot echo must never re-PATCH — that flooded a node with
  // hundreds of identical volume requests).
  let { value, onchange } = $props();

  let dragging = $state(false);
  let pct = $state(0);
  let lastSent = null; // last 0–1 value actually sent

  // Re-sync to the server truth when a new snapshot arrives and not dragging.
  $effect(() => {
    const v = Math.round((value || 0) * 100);
    if (!dragging) pct = v;
  });

  let timer = null;
  function send() {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
    const v = pct / 100;
    if (v === lastSent) return; // already sent this exact value — do nothing
    lastSent = v;
    onchange(v).catch(() => {
      lastSent = null; // failed → allow a retry
    });
  }

  function oninput(e) {
    dragging = true;
    pct = Number(e.target.value);
    if (timer) clearTimeout(timer);
    timer = setTimeout(send, 150);
  }

  function oncommit() {
    // trailing call on pointerup/change so the final position always lands.
    send();
    dragging = false;
  }
</script>

<span class="vol">
  <input
    type="range"
    min="0"
    max="100"
    value={pct}
    {oninput}
    onchange={oncommit}
    onpointerup={oncommit}
    aria-label="volume"
  />
  <span class="pct small">{pct}%</span>
</span>
