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

// isMaster reports whether a node is a master (solo or group master): its
// following points at the zero id.
export function isMaster(node) {
  return !!node && node.following === ZERO_ID;
}

// masterCandidates returns the member ids eligible for "make master" — all
// members (§5.2).
export function masterCandidates(group) {
  return group && group.members ? group.members : [];
}

// joinTargets returns other alive masters this node could follow: alive,
// master (following === ZERO_ID), not itself, and not its current master.
export function joinTargets(snapshot, node) {
  if (!snapshot || !snapshot.nodes || !node) return [];
  return snapshot.nodes.filter(
    (n) =>
      n.id !== node.id &&
      n.alive &&
      n.following === ZERO_ID &&
      n.id !== node.following,
  );
}

// groupLabel returns the group's name or a short fallback.
export function groupLabel(group) {
  if (!group) return "";
  return group.name && group.name.length > 0
    ? group.name
    : "Group " + shortId(group.id);
}

// selfNode returns this node's NodeView, or undefined.
export function selfNode(snapshot, selfId) {
  return nodeById(snapshot, selfId);
}

// addTargets returns alive nodes that are NOT members of the given group —
// candidates for the group card's "Add node…" control. Following one onto the
// group's master folds it into this group (§5.1).
export function addTargets(snapshot, group) {
  if (!snapshot || !snapshot.nodes || !group) return [];
  const members = new Set(group.members || []);
  return snapshot.nodes.filter((n) => n.alive && !members.has(n.id));
}
