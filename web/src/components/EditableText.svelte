<script>
  // Click-to-rename inline editor (J arch §4). Reused by group + node names.
  // allowEmpty: permit committing an EMPTY value (group rename clears an override
  // back to the derived label, D42). muted: render the display span muted/italic
  // (used for a derived group label that has no explicit override).
  let {
    value,
    onsave,
    placeholder = "",
    allowEmpty = false,
    muted = false,
  } = $props();

  let editing = $state(false);
  let draft = $state("");
  let inputEl = $state(null);

  function start() {
    draft = value || "";
    editing = true;
  }

  $effect(() => {
    if (editing && inputEl) inputEl.focus();
  });

  async function commit() {
    if (!editing) return;
    const next = draft.trim();
    editing = false;
    // Empty + allowEmpty clears (only meaningful when there IS a current value to
    // clear); otherwise empty is a no-op cancel.
    const changed = next !== (value || "");
    if (changed && (next || (allowEmpty && value))) {
      try {
        await onsave(next);
      } catch {
        // toast already shown by api.js; snapshot reverts the display
      }
    }
  }

  function cancel() {
    editing = false;
  }

  function onkey(e) {
    if (e.key === "Enter") {
      e.preventDefault();
      commit();
    } else if (e.key === "Escape") {
      e.preventDefault();
      cancel();
    }
  }
</script>

{#if editing}
  <input
    class="editable-input"
    bind:this={inputEl}
    bind:value={draft}
    onkeydown={onkey}
    onblur={commit}
    {placeholder}
  />
{:else}
  <span
    class="editable"
    class:muted
    role="button"
    tabindex="0"
    onclick={start}
    onkeydown={(e) => e.key === "Enter" && start()}
    title="click to rename"
  >
    {value || placeholder || "—"}
  </span>
{/if}

<style>
  /* a server-derived label (no explicit override) reads muted + italic, D42 */
  .muted {
    font-style: italic;
    opacity: 0.7;
  }
</style>
