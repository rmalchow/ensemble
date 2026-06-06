<script lang="ts">
  // Compact signed-ms sync-error pill (09 §3). Adapts the threshold-color idea
  // from ../media Gauge.svelte (value → good/warn color) into a flat pill rather
  // than an arc: ✔ under the warn edge, ⚠ at/above it (SYNC_WARN_US, A.12
  // HardErrSamp / A.13 P4). A master (or non-listener) is passed us=null and
  // renders the em-dash "—" with no tone.
  import Chip from '../ui/Chip.svelte'
  import { syncMs, syncLevel } from '../../lib/syncfmt'

  interface Props {
    us: number | null
    isMaster: boolean
  }
  let { us, isMaster }: Props = $props()

  const text = $derived(syncMs(isMaster ? null : us))
  const level = $derived(isMaster ? 'ok' : syncLevel(us))
  const isDash = $derived(text === '—')
</script>

{#if isDash}
  <span class="dash" aria-label={isMaster ? 'reference (master)' : 'no sync data'}>—</span>
{:else if level === 'warn'}
  <Chip tone="warn">⚠ {text}</Chip>
{:else}
  <Chip tone="success">✔ {text}</Chip>
{/if}

<style>
  .dash {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }
</style>
