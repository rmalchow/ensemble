// Polls the master's per-member STATUS telemetry (GET /api/playback/statuses) and
// exposes it as a reactive map keyed by node id. The master collects every member's
// stats from the STATUS control payload, so this surfaces sync health even for
// members with no reachable HTTP API (D56) or on another subnet — unlike a per-node
// status fetch, which the Pis 502.

import { getPlaybackStatuses } from "./api.js";

export const playbackStats = $state({ byId: {}, at: 0 });

let timer = null;

export function startStatsPolling(intervalMs = 1500) {
  if (timer) return;
  const tick = async () => {
    try {
      const arr = await getPlaybackStatuses();
      const m = {};
      for (const s of arr || []) m[s.nodeId] = s;
      playbackStats.byId = m;
      playbackStats.at = Date.now();
    } catch {
      // non-fatal: stale stats just grey out in the UI
    }
  };
  tick();
  timer = setInterval(tick, intervalMs);
}

export function stopStatsPolling() {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}
