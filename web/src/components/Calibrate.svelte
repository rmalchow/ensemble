<script>
  // Acoustic calibration trigger + live progress (docs/calibrate.md). Starts a
  // run on the chosen mic node and polls GET /api/<mic>/calibrate until it ends,
  // then shows the per-node result table. The mic node measures every group
  // member and writes their output delays.
  import { startCalibrate, getCalibrate } from "../lib/api.js";

  let { micNodeId, micDevice = "" } = $props();

  let status = $state(null);
  let busy = $state(false);

  let running = $derived(!!status?.running);

  async function start() {
    if (!micNodeId || busy) return;
    busy = true;
    try {
      status = await startCalibrate(micNodeId, { micDevice });
      await poll();
    } catch {
      // toast shown by api.js; leave any prior status visible
    } finally {
      busy = false;
    }
  }

  async function poll() {
    for (;;) {
      const s = await getCalibrate(micNodeId);
      status = s;
      if (!s || !s.running) return;
      await new Promise((r) => setTimeout(r, 500));
    }
  }

  function fmtMs(v) {
    const r = Math.round(v);
    return (r > 0 ? "+" : "") + r + " ms";
  }
</script>

<div class="calib">
  <button class="btn" disabled={!micNodeId || running || busy} onclick={start}>
    {running ? "Calibrating…" : "Calibrate…"}
  </button>

  {#if status}
    <div class="calib-status">
      {#if status.running}
        <span class="muted small">
          {status.phase === "measuring" && status.currentNode
            ? `measuring ${status.currentNode} (${status.index + 1}/${status.total})`
            : status.phase}
        </span>
      {:else if status.error}
        <span class="err small">⚠ {status.error}</span>
      {:else if status.done}
        <span class="ok small">
          aligned · {status.mode} · spread {Math.round(status.spreadMs)} ms
        </span>
        <table class="calib-table">
          <tbody>
            {#each status.nodes as n (n.id)}
              <tr class:dim={!n.used}>
                <td class="nm">{n.name}</td>
                <td class="num">{(n.confidence ?? 0).toFixed(2)}</td>
                <td class="num">{n.used ? fmtMs(n.outputDelayMs) : "skipped"}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </div>
  {/if}
</div>

<style>
  .calib {
    display: flex;
    flex-direction: column;
    gap: 6px;
    align-items: flex-start;
    flex: 1;
    min-width: 0;
  }
  .calib-status {
    width: 100%;
  }
  .err {
    color: var(--danger);
  }
  .ok {
    color: var(--ok);
  }
  .calib-table {
    width: 100%;
    border-collapse: collapse;
    margin-top: 4px;
  }
  .calib-table td {
    padding: 2px 6px;
    border-top: 1px solid var(--border);
  }
  .calib-table .nm {
    color: var(--fg);
  }
  .calib-table .num {
    text-align: right;
    color: var(--muted);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .calib-table tr.dim {
    opacity: 0.5;
  }
</style>
