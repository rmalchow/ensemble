<script lang="ts">
  // data/ mp3 list (09 §7). Reuses the list-row layout + dark styling from
  // ../media MediaBin/BinItem ONLY (row, filename, secondary meta, hover/selected
  // state); the body is reimplemented for audio: file + length + per-row
  // Select&play / Stop + the loop toggle. No drag-to-timeline, no transcode, no
  // thumbnails. The selected+playing row shows ♪ playing and a Stop; every other
  // row shows Select & play. The loop toggle sits on the selected row (F.3 carries
  // it while playing; F.2 persists it while stopped — handled by the parent).
  import Button from '../ui/Button.svelte'
  import { mmss } from '../../lib/format'
  import type { MediaFile } from '../../lib/media'

  interface Props {
    files: MediaFile[]
    masterNodeName: string // "data/ on N1 (Kitchen)"
    selectedFile: string | null // group.media.file
    playing: boolean
    loop: boolean
    busyFile?: string | null // a file whose command is in flight → spinner
    disabled?: boolean // offline master → browse/play disabled
    // Directory browsing (F.1 path/dirs): the current data/-relative folder,
    // its subdirectories, and the navigation callback. Optional so existing
    // call sites (offline last-selection) render flat.
    path?: string
    dirs?: string[]
    onOpenDir?: (path: string) => void
    onPlay: (file: string) => void
    onStop: () => void
    onToggleLoop: (loop: boolean) => void
  }
  let {
    files,
    masterNodeName,
    selectedFile,
    playing,
    loop,
    busyFile = null,
    disabled = false,
    path = '',
    dirs = [],
    onOpenDir,
    onPlay,
    onStop,
    onToggleLoop,
  }: Props = $props()

  function lengthOf(f: MediaFile): string {
    return f.durationMs ? mmss(f.durationMs / 1000) : '—'
  }

  // Command guards: an offline master disables every row action even if the
  // (disabled) button still receives a synthetic click in a test harness.
  function play(file: string) {
    if (!disabled) onPlay(file)
  }
  function halt() {
    if (!disabled) onStop()
  }
  function toggle(next: boolean) {
    if (!disabled) onToggleLoop(next)
  }

  // Directory navigation: enter a subfolder / go one level up. Paths are
  // slash-separated and data/-relative ("" = root).
  function enter(dir: string) {
    if (!disabled && onOpenDir) onOpenDir(path ? `${path}/${dir}` : dir)
  }
  function up() {
    if (disabled || !onOpenDir) return
    const i = path.lastIndexOf('/')
    onOpenDir(i >= 0 ? path.slice(0, i) : '')
  }
</script>

<div class="filelist">
  <div class="head">
    <span class="src">{masterNodeName}{path ? `/${path}` : ''}</span>
  </div>

  <table>
    <thead>
      <tr>
        <th class="c-file">file</th>
        <th class="c-len">length</th>
        <th class="c-act"></th>
      </tr>
    </thead>
    <tbody>
      {#if path}
        <tr class="dir">
          <td class="c-file" colspan="3">
            <button type="button" class="dirlink" {disabled} onclick={up}>
              ↩ ..
            </button>
          </td>
        </tr>
      {/if}
      {#each dirs as d (d)}
        <tr class="dir">
          <td class="c-file" colspan="3">
            <button type="button" class="dirlink" {disabled} onclick={() => enter(d)}>
              📁 {d}/
            </button>
          </td>
        </tr>
      {/each}
      {#each files as f (f.file)}
        {@const isSel = f.file === selectedFile}
        {@const isPlaying = isSel && playing}
        <tr class:selected={isSel}>
          <td class="c-file">
            <span class="marker" aria-hidden="true">{isPlaying ? '●' : ''}</span>
            <span class="fname">{f.file}</span>
            {#if isPlaying}<span class="np">♪ playing</span>{/if}
            {#if f.title || f.artist}
              <span class="meta">{[f.artist, f.title].filter(Boolean).join(' — ')}</span>
            {/if}
          </td>
          <td class="c-len">{lengthOf(f)}</td>
          <td class="c-act">
            <div class="row-actions">
              {#if isPlaying}
                <Button
                  variant="ghost"
                  loading={busyFile === f.file}
                  {disabled}
                  onclick={halt}>◼ Stop</Button
                >
              {:else}
                <Button
                  variant="primary"
                  loading={busyFile === f.file}
                  {disabled}
                  onclick={() => play(f.file)}>▶ Select &amp; play</Button
                >
              {/if}
              <!-- The loop toggle sits on the SELECTED row in both states: while
                   playing it rides the next F.3, while stopped it persists via F.2
                   (09 §7). Other rows show nothing extra. -->
              {#if isSel}
                <button
                  type="button"
                  class="loop"
                  class:on={loop}
                  aria-pressed={loop}
                  {disabled}
                  onclick={() => toggle(!loop)}
                >
                  ↻ loop {loop ? 'ON' : 'off'}
                </button>
              {/if}
            </div>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

<style>
  .filelist {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .src {
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }
  th {
    text-align: left;
    font-weight: 500;
    color: var(--text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    padding: 0 var(--space-3) var(--space-2);
    border-bottom: 1px solid var(--border-subtle);
  }
  td {
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    vertical-align: middle;
  }
  tbody tr:last-child td {
    border-bottom: none;
  }
  tbody tr:hover {
    background: var(--surface-2);
  }
  tr.selected {
    background: var(--surface-3);
  }
  .c-file {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .marker {
    width: 0.8rem;
    color: var(--success-bright);
  }
  .dirlink {
    background: none;
    border: none;
    padding: 0;
    font: inherit;
    color: var(--text);
    cursor: pointer;
  }
  .dirlink:hover:not(:disabled) {
    text-decoration: underline;
  }
  .dirlink:disabled {
    color: var(--text-dim);
    cursor: default;
  }
  .fname {
    color: var(--text);
    font-family: var(--font-mono);
  }
  .np {
    color: var(--success-bright);
    font-size: var(--text-xs);
  }
  .meta {
    color: var(--text-muted);
    font-size: var(--text-xs);
  }
  .c-len {
    color: var(--text-dim);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .c-act {
    text-align: right;
    white-space: nowrap;
  }
  .row-actions {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
  }
  .loop {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    font: inherit;
    font-size: var(--text-xs);
    padding: 0.3rem 0.55rem;
    cursor: pointer;
  }
  .loop.on {
    color: var(--accent-bright);
    border-color: var(--accent);
    background: rgba(31, 111, 235, 0.12);
  }
  .loop:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
