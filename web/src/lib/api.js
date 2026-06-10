// REST helpers (J arch §3.2). One function per §9.1 action. Proxy-aware: a call
// targeting another node is issued against /api/<nodeId>/<rest> and the local
// node proxies it (§9.3).

import { pushToast } from "./toast.svelte.js";
import { ZERO_ID } from "./derive.js";

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
// D57: replace the node's Spotify Connect presets (each {id,name,players[]}).
export function setSpotifyEndpoints(nodeId, spotifyEndpoints) {
  return toasted(req("PATCH", base(nodeId) + "/node", { spotifyEndpoints }));
}

// --- group membership (issued ON the acting node) ---
export function follow(nodeId, targetId) {
  return toasted(req("POST", base(nodeId) + "/follow", { target: targetId }));
}
export function unfollow(nodeId) {
  return toasted(req("POST", base(nodeId) + "/unfollow"));
}

// --- playback-node mutations (MASTER-LOCAL) ---
// A non-gossiping playback node has no HTTP API (D56), so its name/volume/delay/
// group are set on the LOCAL master (which owns the proxied record and drives the
// node over the control plane) — NEVER proxied to the node via base(nodeId).
export function patchPlaybackNode(fields) {
  return toasted(req("POST", "/api/playback/patch", fields));
}

// node-aware routers: a playback node goes through the master-local patch; a normal
// gossiping node uses the per-node / follow endpoints (proxy-aware).
export function nodeRename(node, name) {
  return node.playbackNode
    ? patchPlaybackNode({ node: node.id, name })
    : renameNode(node.id, name);
}
export function nodeSetVolume(node, volume) {
  return node.playbackNode
    ? patchPlaybackNode({ node: node.id, volume })
    : setVolume(node.id, volume);
}
export function nodeSetOutputDelay(node, outputDelayMs) {
  return node.playbackNode
    ? patchPlaybackNode({ node: node.id, outputDelayMs })
    : setOutputDelay(node.id, outputDelayMs);
}
export function assignToGroup(node, masterId) {
  return node.playbackNode
    ? patchPlaybackNode({ node: node.id, following: masterId })
    : follow(node.id, masterId);
}
export function leaveGroup(node) {
  return node.playbackNode
    ? patchPlaybackNode({ node: node.id, following: "" })
    : unfollow(node.id);
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

// playOnNode plays `uri` on the master of the group to play. Under the crosswise
// model every node ALWAYS masters its own group, so this is a direct play — no
// takeover dance. Extra args (a snapshot getter, opts) are accepted for call-site
// compatibility and ignored.
export async function playOnNode(nodeId, uri) {
  return play(nodeId, uri);
}
// enqueue appends one or more file URIs to the master's play queue. A fresh idle
// queue auto-plays; an active queue gets them appended to the end.
export function enqueue(masterId, uris) {
  return toasted(req("POST", base(masterId) + "/queue", { uris }));
}
// queueRemove drops the upcoming item at index (0 == next); uri guards the index.
export function queueRemove(masterId, index, uri) {
  return toasted(req("POST", base(masterId) + "/queue/remove", { index, uri }));
}
// next skips to the next queued track (gaplessly).
export function next(masterId) {
  return toasted(req("POST", base(masterId) + "/next"));
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
