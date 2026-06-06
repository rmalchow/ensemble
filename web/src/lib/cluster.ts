// Typed wrappers + view types for the Cluster screen (09 §4) over the 08 §C/§D
// API. Pattern lifted from ../media web/src/lib/api.ts (thin fetch → throw on
// non-2xx → typed body) but every endpoint re-targeted to the canonical
// /api/v1/... paths and routed through P0.4's shared apiFetch so the §0.4 error
// envelope and §0.5 If-Match/ETag handling stay uniform. Components NEVER call
// fetch directly — they go through these wrappers (mirrors the mpvsync "screens
// never touch the socket" discipline in ws.ts/stores.ts).

import { apiFetch, ApiError } from './api'
import type { Capabilities } from './types'

export { ApiError }
export type { Capabilities }

// ---- View types (mirror 08 §0.7 / C.2 / README §6.5) ----------------------

// MemberNode is a current cluster member: the ConfigDoc Nodes[] projection
// (D.1) joined with gossip liveness (C.1/C.2 members[].online).
export interface MemberNode {
  id: string
  name: string
  addrs: string[]
  online: boolean
  caps: Capabilities
  fingerprint?: string // node cert fingerprint (member row footer)
  groupId?: string // current group, if any
  isMaster?: boolean // elected master of its group (master-no-audio note)
}

// DiscoveredNode is a node seen on the LAN but not yet in this cluster (C.2
// discovered[]). `fingerprint` is the node's self-signed CSR fingerprint, which
// the operator pins out-of-band before sending the PIN.
export interface DiscoveredNode {
  nodeId: string
  name: string
  addrs: string[]
  fingerprint: string
  state: 'uninitialized' | 'foreign'
  softwareVersion?: string
}

// DiscoverySnapshot is the raw C.2 shape: cluster members (lightweight, with
// liveness) plus the discovered (not-yet-adopted) advertisers.
export interface DiscoverySnapshot {
  members: { id: string; name: string; addrs: string[]; state: string; online: boolean }[]
  discovered: DiscoveredNode[]
}

export interface ClusterInfo {
  version: number
  cluster: { name: string; caFingerprint: string; created: string }
  counts: { nodes: number; groups: number }
}

// ---- Reads -----------------------------------------------------------------

// getClusterInfo reads cluster identity + CA fingerprint + counts (C.1). The
// ETag (ConfigDoc.Version) is folded into the returned `version` so callers can
// seed If-Match without a second read.
export async function getClusterInfo(): Promise<ClusterInfo> {
  const { data, version } = await apiFetch<ClusterInfo>('/api/v1/cluster/info')
  return { ...data, version: data?.version ?? version ?? 0 }
}

// getDiscovery reads the live discovery snapshot (C.2): members + discovered.
// Read-only, no If-Match.
export async function getDiscovery(): Promise<DiscoverySnapshot> {
  const { data } = await apiFetch<DiscoverySnapshot>('/api/v1/discovery')
  return { members: data?.members ?? [], discovered: data?.discovered ?? [] }
}

// getNodes lists member NodeRecords (D.1). `version` is the doc ETag, used to
// keep configVersion fresh for the next write.
export async function getNodes(): Promise<{ version: number; nodes: MemberNode[] }> {
  const { data, version } = await apiFetch<{ version: number; nodes: MemberNode[] }>(
    '/api/v1/nodes',
  )
  return { version: data?.version ?? version ?? 0, nodes: data?.nodes ?? [] }
}

// rescan re-issues the discovery sweep (C.2 is a fan-out read on the serving
// node). Same shape as getDiscovery; no config write, no If-Match.
export function rescan(): Promise<DiscoverySnapshot> {
  return getDiscovery()
}

// ---- Writes (all require If-Match: <version>) ------------------------------

export interface AdoptReq {
  nodeId: string
  addr: string
  fingerprint: string
  pin: string
  name?: string
  // password is the C.4 takeover release credential: the TARGET node's CURRENT
  // cluster admin password (its operator authorizes the move, 03 §4). Unused
  // for a plain adopt.
  password?: string
}

// adopt signs + records a discovered uninitialized node (C.3). The PIN is sent
// verbatim — including the default "0000", which is a real secret (D9), never
// stripped or normalised here.
export async function adopt(
  req: AdoptReq,
  ifMatch: number,
): Promise<{ version: number; node: MemberNode }> {
  const { data, version } = await apiFetch<{ version: number; node: MemberNode }>(
    '/api/v1/cluster/adopt',
    { method: 'POST', body: req, ifMatch },
  )
  return { version: data?.version ?? version ?? ifMatch, node: data.node }
}

// takeover forces a re-issue of a foreign/old-cluster node's identity (C.4):
// the C.3 body plus force:true. Still PIN-gated.
export async function takeover(
  req: AdoptReq & { force: true },
  ifMatch: number,
): Promise<{ version: number; node: MemberNode }> {
  const { data, version } = await apiFetch<{ version: number; node: MemberNode }>(
    '/api/v1/cluster/takeover',
    { method: 'POST', body: req, ifMatch },
  )
  return { version: data?.version ?? version ?? ifMatch, node: data.node }
}

// forget revokes + removes a member node (C.5). On success it reports the
// groups whose membership/master changed as a side effect.
export async function forget(
  nodeId: string,
  ifMatch: number,
): Promise<{ version: number; removedNodeId: string; affectedGroups: string[] }> {
  const { data, version } = await apiFetch<{
    version: number
    removedNodeId: string
    affectedGroups: string[]
  }>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/forget`, {
    method: 'POST',
    ifMatch,
  })
  return {
    version: data?.version ?? version ?? ifMatch,
    removedNodeId: data?.removedNodeId ?? nodeId,
    affectedGroups: data?.affectedGroups ?? [],
  }
}
