// Smooth UI playback position from coarse, occasionally-stale authoritative
// updates. The server reports positionSec only ~every 5 s (group heartbeat) and it
// arrives a little stale (gossip + WS debounce). But position is just realtime
// between discrete events, so we run a LOCAL clock anchored to the server's value:
// free-run at 1x, gently slew toward each heartbeat, and hard-snap only on a real
// discontinuity (track change, resume, or a large delta = seek/replay). This yields
// evenly-spaced 1 s ticks instead of the stall-then-catch-up sawtooth a naive
// "re-anchor every heartbeat" produces.
//
// Pure + deterministic: all timestamps are passed in (nowMs), so it unit-tests
// without timers. PlaybackBar feeds it performance.now().

export const SNAP_THRESHOLD = 3; // s — a server update beyond this is a seek/replay → snap
export const SLEW = 0.15; // fraction of the (behind-only) error corrected per update

// createPlayClock returns fresh clock state. `pos` is the last sampled display
// value; the anchor (pos@time) defines the free-running line position = anchorPos +
// (now - anchorAt).
export function createPlayClock() {
  return {
    anchorPos: 0, // s — authoritative position captured at the anchor
    anchorAt: 0, // ms — nowMs when anchored
    anchorUri: "", // track playing at the anchor
    wasPlaying: false, // playing at the last reconcile (detects resume)
    pos: 0, // s — last sampled display position
  };
}

// reconcile folds one authoritative update into the clock. While paused/idle it
// freezes at the reported value; while playing it snaps on a discontinuity, else
// slews forward toward the reported value (never stepping backward in steady state).
export function reconcile(c, { positionSec, uri, playing, nowMs }, opts = {}) {
  const snapThreshold = opts.snapThreshold ?? SNAP_THRESHOLD;
  const slew = opts.slew ?? SLEW;
  const sp = positionSec || 0;
  const u = uri || "";

  if (!playing) {
    // paused / idle: hold the authoritative value, no ticking.
    c.anchorPos = sp;
    c.anchorAt = nowMs;
    c.anchorUri = u;
    c.wasPlaying = false;
    c.pos = sp;
    return c;
  }

  const est = c.anchorPos + (nowMs - c.anchorAt) / 1000;
  const discontinuity =
    u !== c.anchorUri || !c.wasPlaying || Math.abs(sp - est) > snapThreshold;

  if (discontinuity) {
    // new track / resume / seek: the jump to sp is correct.
    c.anchorPos = sp;
    c.pos = sp;
  } else {
    // steady state: only catch up when behind (positive error); ignore being
    // slightly ahead so the displayed number never stalls or steps back.
    const err = sp - est;
    c.anchorPos = err > 0 ? est + err * slew : est;
  }
  c.anchorAt = nowMs;
  c.anchorUri = u;
  c.wasPlaying = true;
  return c;
}

// sample reads the display position at nowMs, clamped to durationSec (0 = unknown),
// and monotonic within a track (never steps back between snaps).
export function sample(c, nowMs, durationSec = 0) {
  if (!c.wasPlaying) return c.pos; // frozen (paused/idle)
  let p = c.anchorPos + (nowMs - c.anchorAt) / 1000;
  if (durationSec > 0 && p > durationSec) p = durationSec;
  if (p < c.pos) p = c.pos; // monotonic safety
  c.pos = p;
  return p;
}
