// REST helpers (J arch §3.2). One function per §9.1 action. Proxy-aware: a call
// targeting another node is issued against /api/<nodeId>/<rest> and the local
// node proxies it (§9.3).

import { pushToast } from "./toast.svelte.js";
import { ZERO_ID } from "./derive.js";
import { shortId } from "./fmt.js";

// ApiError carries the server's message for the Toast.
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// self id, set once by App after getStatus(); used so base() never self-proxies.
let _selfId = "";
export function setSelfId(id) {
  _selfId = id || "";
}

// base(nodeId?) → "/api" for local (empty / self id), "/api/<nodeId>" otherwise.
export function base(nodeId) {
  if (!nodeId || nodeId === ZERO_ID || nodeId === _selfId) return "/api";
  return "/api/" + nodeId;
}

// req issues a fetch and returns parsed JSON, or throws ApiError. On non-2xx it
// reads {error} or {message} from the body for the toast.
async function req(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(path, opts);
  } catch (e) {
    throw new ApiError(0, e && e.message ? e.message : "network error");
  }
  let data = null;
  const text = await resp.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!resp.ok) {
    const msg =
      (data && (data.error || data.message)) ||
      resp.statusText ||
      "request failed";
    throw new ApiError(resp.status, msg);
  }
  return data;
}

// toasted wraps an action so a failing call surfaces as a toast and rethrows
// (callers may also catch to revert local UI state).
async function toasted(p) {
  try {
    return await p;
  } catch (e) {
    pushToast(e && e.message ? e.message : "action failed", "error");
    throw e;
  }
}

// --- self / cluster (local only) ---
export async function getStatus() {
  return req("GET", "/api/status");
}
export async function getCluster() {
  return req("GET", "/api/cluster");
}

// --- node actions ---
export function renameNode(nodeId, name) {
  return toasted(req("PATCH", base(nodeId) + "/node", { name }));
}
export function setVolume(nodeId, volume) {
  return toasted(req("PATCH", base(nodeId) + "/node", { volume }));
}
export function setOutputDelay(nodeId, outputDelayMs) {
  return toasted(req("PATCH", base(nodeId) + "/node", { outputDelayMs }));
}
export function setOutputDevice(nodeId, outputDevice) {
  return toasted(req("PATCH", base(nodeId) + "/node", { outputDevice }));
}
export function testTone(nodeId) {
  return toasted(req("POST", base(nodeId) + "/tone"));
}
export function setDisabled(nodeId, disabled) {
  return toasted(req("PATCH", base(nodeId) + "/node", { disabled }));
}

// --- group membership (issued ON the acting node) ---
export function follow(nodeId, targetId) {
  return toasted(req("POST", base(nodeId) + "/follow", { target: targetId }));
}
export function unfollow(nodeId) {
  return toasted(req("POST", base(nodeId) + "/unfollow"));
}
export function makeMaster(memberId, newMasterId) {
  return toasted(
    req("POST", base(memberId) + "/group/master", { node: newMasterId }),
  );
}

// --- group naming (LWW from any node; issued locally) ---
export function renameGroup(groupId, name) {
  return toasted(req("POST", "/api/group/name", { group: groupId, name }));
}

// --- media + playback ---
export function getMedia(nodeId) {
  return toasted(req("GET", base(nodeId) + "/media"));
}
export function play(nodeId, uri) {
  return toasted(req("POST", base(nodeId) + "/play", { uri }));
}

// playOnNode plays `uri` on `nodeId`, doing a CLIENT-SIDE takeover first when
// `nodeId` is NOT its group's master. Sequence:
//   1. read the current snapshot; if nodeId already masters its group → play.
//   2. else makeMaster(nodeId) → poll getSnapshot() until that group's master
//      is nodeId (or until the timeout) → then play.
// Surfaces a "moving playback…" toast during the wait; on timeout, an error
// toast and no play call. `getSnapshot` returns the latest Snapshot (the live
// ws store in the app; getCluster in tests). Avoids the server's "not master"
// rejection that play() alone would hit on a follower.
export async function playOnNode(nodeId, uri, getSnapshot, opts = {}) {
  const timeoutMs = opts.timeoutMs ?? 5000;
  const pollMs = opts.pollMs ?? 250;
  const nameOfNode = opts.name || shortId(nodeId);

  // groupOf(snap) → the group `nodeId` is a member of, or undefined.
  const groupOf = (snap) =>
    snap && snap.groups
      ? snap.groups.find((g) => g.members && g.members.includes(nodeId))
      : undefined;

  let snap = getSnapshot();
  let group = groupOf(snap);

  // Already the master (or unknown topology) → straight play.
  if (!group || group.master === nodeId) {
    return play(nodeId, uri);
  }

  // Not the master → take over, then wait for the snapshot to confirm.
  pushToast("moving playback to " + nameOfNode + "…", "ok");
  await makeMaster(nodeId, nodeId);

  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, pollMs));
    let next;
    try {
      next = await getSnapshot();
    } catch {
      next = undefined;
    }
    const g = groupOf(next);
    if (g && g.master === nodeId) {
      return play(nodeId, uri);
    }
  }

  pushToast("timed out moving playback to " + nameOfNode, "error");
  throw new ApiError(0, "takeover timed out");
}
export function stop(masterId) {
  return toasted(req("POST", base(masterId) + "/stop"));
}
export function pause(masterId) {
  return toasted(req("POST", base(masterId) + "/pause"));
}
export function resume(masterId) {
  return toasted(req("POST", base(masterId) + "/resume"));
}

// --- group settings (master only for POST) ---
export function setGroupSettings(masterId, settings) {
  return toasted(req("POST", base(masterId) + "/group/settings", settings));
}

// --- acoustic calibration (issued ON the mic node) ---
export function startCalibrate(micNodeId, opts = {}) {
  return toasted(req("POST", base(micNodeId) + "/calibrate", opts));
}
export function getCalibrate(micNodeId) {
  return req("GET", base(micNodeId) + "/calibrate");
}
