// Derived, read-only stores for the Cluster screen (09 §4). Pattern lifted from
// ../media web/src/lib/stores.ts (derived/writable; components read derived
// views, never the raw snapshot) but the DeviceView model is replaced with the
// ConfigDoc-backed MemberNode/DiscoveredNode shapes (08 C.2 / D.1, README §6.5).
//
// The screen orchestrates writes and calls refreshCluster() on mount + after
// each successful write; components subscribe to the derived stores below and
// never poke the raw fetch.

import { derived, get, writable, type Readable } from 'svelte/store'
import {
  getClusterInfo,
  getDiscovery,
  getNodes,
  ApiError,
  type DiscoveredNode,
  type MemberNode,
} from './cluster'
import { configVersion } from './stores'

// Raw, internal snapshot writables. Components read the derived projections, not
// these — keeping the same discipline as the mpvsync raw-vs-derived split.
const infoStore = writable<{ name: string; caFingerprint: string } | null>(null)
const nodesStore = writable<MemberNode[]>([])
// liveness maps node id → online (from the C.2 discovery members[] / C.1 gossip)
// so the D.1 NodeRecord projection (which has no liveness of its own) can be
// joined into a single online flag per member.
const livenessStore = writable<Record<string, boolean>>({})
// liveAddrsStore maps node id → the C.2 members[] addrs (the LIVE control
// endpoints, host:port). Preferred over the bare D.1 record addrs in the
// members view so the table shows reachable addresses.
const liveAddrsStore = writable<Record<string, string[]>>({})
const discoveredStore = writable<DiscoveredNode[]>([])

// rowStateStore holds per-row transient UI state (busy spinner / inline error),
// keyed by node id, so an action on one row never bleeds into another.
export interface RowState {
  busy: boolean
  error?: ApiError
}
const rowStateStore = writable<Record<string, RowState>>({})

// ---- Public derived views --------------------------------------------------

export const caFingerprint: Readable<string> = derived(
  infoStore,
  ($i) => $i?.caFingerprint ?? '',
)

export const clusterName: Readable<string> = derived(infoStore, ($i) => $i?.name ?? '')

// configVersion is re-exported as the screen's If-Match seed; it lives in the
// shared stores module (updated by every read's ETag + every write's returned
// version), so the rest of the app stays in sync.
export { configVersion }

// members joins the D.1 NodeRecord projection with the C.2/C.1 liveness map.
// When liveness has no entry for a node (e.g. discovery hasn't reported it yet)
// the node defaults to offline rather than silently appearing online.
export const members: Readable<MemberNode[]> = derived(
  [nodesStore, livenessStore, liveAddrsStore],
  ([$nodes, $live, $addrs]) =>
    $nodes.map((n) => ({
      ...n,
      online: $live[n.id] ?? n.online ?? false,
      addrs: $addrs[n.id]?.length ? $addrs[n.id] : n.addrs,
    })),
)

export const discovered: Readable<DiscoveredNode[]> = derived(
  discoveredStore,
  ($d) => $d,
)

export const onlineCounts: Readable<{ online: number; total: number }> = derived(
  members,
  ($m) => ({ online: $m.filter((n) => n.online).length, total: $m.length }),
)

export const rowState: Readable<Record<string, RowState>> = rowStateStore

// ---- Sink-less / master-no-audio helpers (consumed by MemberRow) -----------

// isSinkless flags a control/media-only node (Caps.Render === false, D17) — a
// configuration state, NOT an error or offline treatment.
export function isSinkless(n: MemberNode): boolean {
  return n.caps?.render === false
}

// isMasterNoAudio flags an elected group master that has no local audio output
// (isMaster && !render) — surfaces the "master (no local audio)" badge.
export function isMasterNoAudio(n: MemberNode): boolean {
  return !!n.isMaster && isSinkless(n)
}

// ---- Row-state mutators (used by the screen's action handlers) -------------

export function setRowBusy(id: string, busy: boolean): void {
  rowStateStore.update((s) => ({ ...s, [id]: { busy, error: busy ? undefined : s[id]?.error } }))
}

export function setRowError(id: string, error: ApiError | undefined): void {
  rowStateStore.update((s) => ({ ...s, [id]: { busy: false, error } }))
}

export function clearRow(id: string): void {
  rowStateStore.update((s) => {
    const next = { ...s }
    delete next[id]
    return next
  })
}

// ---- Loader ----------------------------------------------------------------

// refreshCluster (re)loads info + nodes + discovery into the stores. It runs the
// three reads concurrently; a failure in any propagates to the caller (the
// screen turns the corresponding region into its error state). The freshest
// config version (max of the info/nodes ETags) seeds configVersion for the next
// If-Match.
export async function refreshCluster(): Promise<void> {
  const [info, nodes, disco] = await Promise.all([
    getClusterInfo(),
    getNodes(),
    getDiscovery(),
  ])
  infoStore.set({
    name: info.cluster?.name ?? '',
    caFingerprint: info.cluster?.caFingerprint ?? '',
  })
  nodesStore.set(nodes.nodes)
  const live: Record<string, boolean> = {}
  const liveAddrs: Record<string, string[]> = {}
  for (const m of disco.members) {
    live[m.id] = m.online
    if (m.addrs?.length) liveAddrs[m.id] = m.addrs
  }
  livenessStore.set(live)
  liveAddrsStore.set(liveAddrs)
  discoveredStore.set(disco.discovered)
  const v = Math.max(info.version ?? 0, nodes.version ?? 0, get(configVersion) ?? 0)
  configVersion.set(v)
}
