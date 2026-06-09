// Pure view-selectors over a Snapshot (J arch §3.3). The SPA never derives
// groups itself — these only join/resolve already-derived server data.

import { shortId } from "./fmt.js";

// ZERO_ID is the all-zero id: "solo / no group / unfollowed" sentinel (§2).
export const ZERO_ID = "00000000000000000000000000000000";

// nodeById finds a NodeView by id, or undefined.
export function nodeById(snapshot, id) {
  if (!snapshot || !snapshot.nodes) return undefined;
  return snapshot.nodes.find((n) => n.id === id);
}

// nameOf returns a node's display name, falling back to its shortId when the
// node isn't (yet) in the snapshot (convergence skew).
export function nameOf(snapshot, id) {
  const n = nodeById(snapshot, id);
  return n && n.name ? n.name : shortId(id);
}

// isIdle reports whether a node's PLAYER is idle (plays nothing): its `following`
// points at the zero id (D49+). Every node always MASTERS its own group (1:1), so
// "master" is no longer a distinguishing node property — only the player's target
// (idle / own / another group) varies.
export function isIdle(node) {
  return !!node && node.following === ZERO_ID;
}

// joinTargets returns the groups this node could send its PLAYER to: every OTHER
// alive node is a zone (it masters its own group), minus self and the current
// target (a no-op). "Play own" and "idle" are separate actions.
export function joinTargets(snapshot, node) {
  if (!snapshot || !snapshot.nodes || !node) return [];
  return snapshot.nodes.filter(
    (n) => n.id !== node.id && n.alive && n.id !== node.following,
  );
}

// groupLabel returns the group's display label. The server always supplies a
// label in `group.name` (D42): either an explicit override or a derived label
// computed from member names. A short-id fallback only covers a transient empty.
export function groupLabel(group) {
  if (!group) return "";
  return group.name && group.name.length > 0
    ? group.name
    : "Group " + shortId(group.id);
}

// groupNameIsDerived reports whether the group's label is the server-DERIVED
// label (no explicit override) — the UI renders those muted/italic (D42).
export function groupNameIsDerived(group) {
  return !!group && group.nameDerived === true;
}

// selfNode returns this node's NodeView, or undefined.
export function selfNode(snapshot, selfId) {
  return nodeById(snapshot, selfId);
}

// deriveRole computes a LIVE status word for THIS node, reflecting where its
// PLAYER plays (D49+ crosswise) from its own `following`:
//   - following Zero            → "idle" (speakers play nothing)
//   - following self            → "solo" (plays own group), or "master" once other
//                                 players have joined that group
//   - following another node    → "follower" (speakers play that node's group)
// Falls back to `fallback` (or "idle") when self isn't resolvable in the snapshot.
export function deriveRole(snapshot, selfId, fallback) {
  const fb = fallback || "idle";
  const self = nodeById(snapshot, selfId);
  if (!self) return fb;
  const f = self.following;
  if (!f || f === ZERO_ID) return "idle";
  if (f === selfId) {
    const own = ((snapshot && snapshot.groups) || []).find(
      (g) => g.master === selfId,
    );
    return own && (own.members || []).length > 1 ? "master" : "solo";
  }
  return "follower";
}

// activeGroup picks the group the overview's Media section follows by default:
// a currently-playing group wins; else the group self belongs to; else the
// first group. Returns undefined only when there are no groups at all.
export function activeGroup(snapshot, selfId) {
  const groups = (snapshot && snapshot.groups) || [];
  if (groups.length === 0) return undefined;
  const playing = groups.find((g) => g.playback && g.playback.state === "playing");
  if (playing) return playing;
  if (selfId) {
    const mine = groups.find((g) => (g.members || []).includes(selfId));
    if (mine) return mine;
  }
  return groups[0];
}

// addTargets returns the nodes eligible to be added to a group as PLAYERS —
// candidates for the group card's "Add node…" control. A node qualifies when it is
// alive, can actually play (capabilities.playback — a `--role master` node has no
// player and is excluded), and is not already a member. Adding one points its player
// at this group (follow → following = group.master).
export function addTargets(snapshot, group) {
  if (!snapshot || !snapshot.nodes || !group) return [];
  const members = new Set(group.members || []);
  return snapshot.nodes.filter(
    (n) =>
      n.alive &&
      !members.has(n.id) &&
      !!(n.capabilities && n.capabilities.playback),
  );
}

// playerZone is a short label of where a candidate player CURRENTLY plays — the
// annotation under an assign-chip, so the user sees what a click will move. "idle"
// when its player targets nothing (or only its own empty zone — free to grab);
// otherwise the label of the group it currently belongs to (a shared/other room).
export function playerZone(snapshot, node) {
  if (!node || isIdle(node)) return "idle";
  const groups = (snapshot && snapshot.groups) || [];
  const g = groups.find((x) => x.master === node.following);
  if (!g) return "idle";
  // its OWN group with no other players is effectively idle/standalone.
  if (g.master === node.id && (g.members || []).length <= 1) return "idle";
  return groupLabel(g);
}
