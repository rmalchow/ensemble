<script lang="ts">
  // Calibration helper card (09 §6 flow 2, 06 §5.3, A.10b). NEW content; reuses
  // the busy-button idiom from ConfirmModal for "Play test signal". Calibration is
  // MVP-MANUAL (D21): play the built-in synchronous click+tone signal, judge the
  // offset by ear / phone-mic, type the trim into HWDelayUs above, Save, and watch
  // the live sync error converge. Automated cross-correlation /calibrate/measure
  // is a documented LATER enhancement (08 §F2.2) — explicitly NOT in the MVP.
  import Button from '../ui/Button.svelte'
  import Chip from '../ui/Chip.svelte'
  import { calibratePlay, CALIBRATE_DEFAULT_SEC, ApiError } from '../../lib/node'
  import { syncMs, syncLevel } from '../../lib/syncfmt'
  import { pollGroupStatus } from '../../lib/groupStatus'
  import { onMount } from 'svelte'
  import type { NodeDetailView } from '../../lib/node'

  interface Props {
    node: NodeDetailView
    liveSyncErrorUs: number | null
    disabled: boolean
  }
  let { node, liveSyncErrorUs, disabled }: Props = $props()

  let busy = $state(false)
  let warnings = $state<string[]>([])
  let playErr = $state<{ code: string; message: string } | null>(null)

  // Keep the node's current-group /status poll warm while the helper is visible
  // so liveSyncErrorUs (selected upstream from groupStatus) actually ticks.
  onMount(() => {
    if (node.groupId) return pollGroupStatus(node.groupId)
    return () => {}
  })

  // The helper targets the node's current group (plays on all members
  // synchronously so the operator hears the array against a reference); a solo /
  // ungrouped node falls back to just this node (§9 risk 7).
  async function play() {
    busy = true
    playErr = null
    warnings = []
    try {
      const req = node.groupId
        ? { groupId: node.groupId, durationSec: CALIBRATE_DEFAULT_SEC }
        : { nodeIds: [node.id], durationSec: CALIBRATE_DEFAULT_SEC }
      const resp = await calibratePlay(req)
      warnings = resp.warnings
    } catch (e) {
      playErr =
        e instanceof ApiError
          ? { code: e.code, message: e.message }
          : { code: 'unreachable', message: 'Could not reach this node.' }
    } finally {
      busy = false
    }
  }

  const syncText = $derived(syncMs(liveSyncErrorUs))
  const converged = $derived(
    liveSyncErrorUs !== null && syncLevel(liveSyncErrorUs) === 'ok',
  )
</script>

<div class="helper">
  <h4>Calibration helper</h4>
  <ol>
    <li>Add this node to a group with a reference node.</li>
    <li>
      <Button variant="ghost" loading={busy} {disabled} onclick={play}>
        ▶ Play test signal
      </Button>
      <span class="signal-note">
        built-in click + tone, played synchronously (1 s period: ~1 ms full-scale
        click + ~200 ms 1 kHz tone + silence; identical across nodes — 06 §5.3)
      </span>
    </li>
    <li>Judge the offset by ear / phone-mic.</li>
    <li>
      Type the trim into <strong>Hardware delay (HWDelayUs)</strong> above and
      <strong>Save</strong>.
    </li>
  </ol>

  <div class="live">
    <span class="label">live sync error (from group status)</span>
    {#if liveSyncErrorUs === null}
      <span class="dash">— (master / offline / no group)</span>
    {:else if converged}
      <Chip tone="success">✔ {syncText}</Chip>
    {:else}
      <Chip tone="warn">⚠ {syncText}</Chip>
    {/if}
  </div>

  {#if warnings.length > 0}
    <ul class="warnings" role="alert">
      {#each warnings as w (w)}
        <li>{w}</li>
      {/each}
    </ul>
  {/if}
  {#if playErr}
    <p class="err" role="alert">
      <span class="code mono">{playErr.code}</span>
      {playErr.message}
    </p>
  {/if}

  <p class="future">
    Automated cross-correlation measurement is a future enhancement — the MVP
    measurement is manual.
  </p>
</div>

<style>
  .helper {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--surface-2);
    padding: var(--space-4);
  }
  h4 {
    margin: 0 0 var(--space-3);
    font-size: var(--text-base);
    color: var(--text);
  }
  ol {
    margin: 0 0 var(--space-3);
    padding-left: 1.2rem;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--text-dim);
  }
  .signal-note {
    display: block;
    margin-top: var(--space-1);
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .live {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding-top: var(--space-2);
    border-top: 1px solid var(--border-subtle);
  }
  .label {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  .dash {
    color: var(--text-muted);
    font-size: var(--text-sm);
  }
  .warnings {
    margin: var(--space-2) 0 0;
    padding-left: 1.2rem;
    font-size: var(--text-xs);
    color: var(--warn-bright);
  }
  .err {
    margin: var(--space-2) 0 0;
    font-size: var(--text-xs);
    color: var(--danger-bright);
  }
  .code {
    color: var(--danger-bright);
  }
  .mono {
    font-family: var(--font-mono);
  }
  .future {
    margin: var(--space-3) 0 0;
    font-size: var(--text-xs);
    color: var(--text-muted);
    font-style: italic;
  }
</style>
