// Derived, read-only group/node views for the Dashboard (09 §3) and Groups
// (09 §5) screens, plus the pure join + least-capable computations they share.
//
// Pattern lifted from clusterStore.ts (P0.4 / ../media derived-store split):
// raw snapshot writables fed by a loader; components subscribe to the derived
// projections, never the raw fetch. The static structure (groups, members,
// channel role, media, profile, the stored `playing` bool) comes from the 08
// §E.1/§D.1 reads (composed = the P0.2 ConfigView projection); the LIVE
// per-member sync error / negotiated profile comes from groupStatus.ts.

import { derived, get, writable, type Readable } from 'svelte/store'
import { listGroups, listNodes } from './api'
import { configVersion } from './stores'
import type { GroupRecord, NodeRecord, GroupStatus, MemberStatus } from './types'

// ---- Raw snapshot writables (internal) ------------------------------------

const groupsRaw = writable<GroupRecord[]>([])
const nodesRaw = writable<NodeRecord[]>([])
// livenessRaw maps node id → online, joined from the C.2 discovery/gossip feed
// so the D.1 NodeRecord projection (no liveness of its own) yields one offline
// flag per node when /status has no live sample yet.
const livenessRaw = writable<Record<string, boolean>>({})

// ---- Public derived views (09 §3 / §5 data sources) -----------------------

export const groups: Readable<GroupRecord[]> = derived(groupsRaw, ($g) => $g)

export const nodeById: Readable<Map<string, NodeRecord>> = derived(
  nodesRaw,
  ($n) => new Map($n.map((n) => [n.id, n])),
)

export const groupById: Readable<Map<string, GroupRecord>> = derived(
  groupsRaw,
  ($g) => new Map($g.map((g) => [g.id, g])),
)

// nodeLiveness exposes the online map so rows can dim offline members even
// before the per-group /status poll lands.
export const nodeLiveness: Readable<Record<string, boolean>> = derived(
  livenessRaw,
  ($l) => $l,
)

// unassignedNodeIds are member nodes that belong to no *explicit* group: a node
// adopted into its own solo group not yet placed (09 §5). A group is "solo" when
// it has exactly one member and the group is unnamed-or-named-after that node —
// but the canonical signal the engine exposes is simply membership, so the UI
// derives "unassigned" as: nodes present in `nodes` but in no group's
// memberNodeIds. (Per glossary a node is always in exactly one group; a solo
// adopted node may not yet appear in any listed group during the placement gap.)
export const unassignedNodeIds: Readable<string[]> = derived(
  [groupsRaw, nodesRaw],
  ([$groups, $nodes]) => {
    const assigned = new Set<string>()
    for (const g of $groups) for (const id of g.memberNodeIds) assigned.add(id)
    return $nodes.map((n) => n.id).filter((id) => !assigned.has(id))
  },
)

// configVersion is re-exported as the If-Match seed (lives in shared stores,
// refreshed by every read's ETag + every write's returned version).
export { configVersion }

// ---- Member-row join (09 §3) ----------------------------------------------

// MemberRow is the joined view one Dashboard/MembersTable row renders: the
// static NodeRecord facts (name, channel role, render capability) joined with
// the live MemberStatus (sync error, online). The variant captures the four
// 09 §3 cases so the table renders the right badge without re-deriving.
export type MemberRowKind =
  | 'master' //         elected master, renders audio (sync "—")
  | 'masterNoAudio' //  sink-less master: "⊘ master (no local audio)"
  | 'listener' //       normal rendering listener (signed sync error)
  | 'noSink' //         sink-less non-master: "⊘ no sink" (config, normal weight)

export interface MemberRow {
  node: NodeRecord
  kind: MemberRowKind
  isMaster: boolean
  // syncErrorUs is null for the master and for non-listeners (no sink); a
  // number for online listeners. Offline rows also carry null (last-known
  // values are not surfaced as a live error).
  syncErrorUs: number | null
  online: boolean
  // showRole is true only for rendering listeners + the rendering master;
  // sink-less rows have no channel role (04 §4.2.4).
  showRole: boolean
}

// memberRows joins a group's members with the live status, in the group's
// declared member order. `status` may be undefined (status not loaded / group
// offline) — then every non-master row is treated as online:false unless the
// liveness map says otherwise, and sync errors fall to null.
export function memberRows(
  group: GroupRecord,
  status: GroupStatus | undefined,
  nodes: Map<string, NodeRecord>,
  liveness: Record<string, boolean> = {},
): MemberRow[] {
  const masterId = status?.masterNodeId
  const statusById = new Map<string, MemberStatus>(
    (status?.members ?? []).map((m) => [m.nodeId, m]),
  )
  return group.memberNodeIds
    .map((id) => nodes.get(id))
    .filter((n): n is NodeRecord => n !== undefined)
    .map((node) => {
      const st = statusById.get(node.id)
      const online = st?.online ?? liveness[node.id] ?? false
      const isMaster = masterId !== undefined && node.id === masterId
      const renders = node.caps?.render !== false
      let kind: MemberRowKind
      if (isMaster) kind = renders ? 'master' : 'masterNoAudio'
      else kind = renders ? 'listener' : 'noSink'
      // Sync error: only a rendering, online, non-master listener has one.
      const syncErrorUs =
        kind === 'listener' && online && st ? st.syncErrorUs : null
      return {
        node,
        kind,
        isMaster,
        syncErrorUs,
        online,
        showRole: kind === 'master' || kind === 'listener',
      }
    })
}

// ---- Least-capable listener (mirrors 04 §4.3.2) ---------------------------

// LeastCapable names the limiting listener for one capability axis (codec/fec)
// and the resulting floored value. `nodeId` is undefined when no rendering
// listener constrains the axis (e.g. all listeners support the richest option,
// or the group has no rendering listeners → only the master renders).
export interface LeastCapable {
  nodeId?: string
  value: string
}

// CODEC_RANK / FEC_RANK order the options richest-first; the least-capable rule
// picks the poorest option any *rendering listener* can decode (sink-less and
// master nodes never constrain it — 04 §4.2.4 / §4.3.2). The limiting listener
// is the one that floors the choice (lowest-ranked supported option).
const CODEC_RANK: readonly string[] = ['opus', 'pcm'] // opus richest, pcm baseline
const FEC_RANK: readonly string[] = ['duplicate', 'xorParity', 'none']

function leastCapable(
  group: GroupRecord,
  status: GroupStatus | undefined,
  nodes: Map<string, NodeRecord>,
  axis: 'decode' | 'fec',
  rank: readonly string[],
): LeastCapable {
  const masterId = status?.masterNodeId
  // Rendering listeners only: caps.render === true AND not the master.
  const listeners = group.memberNodeIds
    .map((id) => nodes.get(id))
    .filter((n): n is NodeRecord => n !== undefined)
    .filter((n) => n.caps?.render !== false && n.id !== masterId)

  // Walk options richest→poorest. The floor is the richest option ALL listeners
  // support. The limiting listener is the one that forces us BELOW a richer
  // option — i.e. the first listener (in member order) that lacks a richer
  // option than the floor. When even the richest option is universal, no
  // listener constrains the choice (limiterId undefined).
  let limiterId: string | undefined
  let floor = rank[rank.length - 1] // poorest as the safe default
  for (const opt of rank) {
    const lacking = listeners.find((n) => {
      const supported: string[] = n.caps?.[axis] ?? []
      return !supported.includes(opt)
    })
    if (lacking === undefined) {
      // Every listener supports this (richest-so-far) option → it is the floor.
      floor = opt
      break
    }
    // This richer option is NOT universal: the first listener lacking it is the
    // limiter that forces us down. Record the first such (richest) limiter.
    if (limiterId === undefined) limiterId = lacking.id
  }
  // If no option was universal, the floor stays the poorest; the limiter is the
  // listener that lacked the second-poorest option (already recorded).
  return { nodeId: limiterId, value: floor }
}

// leastCapableCodec / leastCapableFec name the limiting listener for the codec
// and FEC axes, mirroring the 04 §4.3.2 least-common-capable rule client-side
// for the ProfileBlock "auto (limited by <node>)" caption.
export function leastCapableCodec(
  group: GroupRecord,
  status: GroupStatus | undefined,
  nodes: Map<string, NodeRecord>,
): LeastCapable {
  return leastCapable(group, status, nodes, 'decode', CODEC_RANK)
}

export function leastCapableFec(
  group: GroupRecord,
  status: GroupStatus | undefined,
  nodes: Map<string, NodeRecord>,
): LeastCapable {
  return leastCapable(group, status, nodes, 'fec', FEC_RANK)
}

// ---- Loader ----------------------------------------------------------------

// refreshGroups (re)loads the group + node structure into the raw stores. The
// two reads run concurrently; a failure propagates to the caller (the screen
// turns to its error state). The freshest ETag seeds configVersion.
export async function refreshGroups(): Promise<void> {
  const [g, n] = await Promise.all([listGroups(), listNodes()])
  groupsRaw.set(g.data?.groups ?? [])
  nodesRaw.set(n.data?.nodes ?? [])
  const v = Math.max(g.version ?? 0, n.version ?? 0, get(configVersion) ?? 0)
  configVersion.set(v)
}

// setLiveness folds the online map (from the per-group /status feeds) into the
// shared liveness store so rows dim consistently across cards.
export function setLiveness(map: Record<string, boolean>): void {
  livenessRaw.update((l) => ({ ...l, ...map }))
}

// __setSnapshotForTest seeds the raw stores directly so the derived-view + join
// tests need not stub fetch. Not used outside tests.
export function __setSnapshotForTest(
  g: GroupRecord[],
  n: NodeRecord[],
  liveness: Record<string, boolean> = {},
): void {
  groupsRaw.set(g)
  nodesRaw.set(n)
  livenessRaw.set(liveness)
}
