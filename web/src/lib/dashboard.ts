// Dashboard static view model (09 §3): composes the three ConfigDoc reads —
// cluster info (C.1), nodes (D.1), groups (E.1) — into one DashboardModel, and
// derives the offline-node strip from `nodes` minus the C.2 discovery online
// set. The LIVE per-member fields (sync error / drift / underruns) come SEPARATELY
// from getGroupStatus (P0.4 live.ts / 08 G.2), polled per-visible-card; they are
// NOT part of this static model. The ConfigDoc `version` (ETag) returned here
// seeds If-Match for the quick Play/Stop card actions (08 §0.5).
//
// This is the canonical P4.10 §4.2 contract. The screen components (GroupCard
// etc., P3.3) drive the live poll + per-card actions directly through groups.ts /
// groupActions.ts; dashboard.ts is the composed read + quick-command facade
// exported for downstream reuse.

import { listNodes, listGroups } from './api'
import { getClusterInfo, getDiscovery } from './cluster'
import { selectAndPlay, stop } from './media'
import type { GroupRecord, NodeRecord } from './types'

export interface DashboardModel {
  clusterName: string
  nodesById: Record<string, NodeRecord>
  groups: GroupRecord[]
  onlineNodeIds: Set<string> // from C.2 discovery.members[].online
  offlineMembers: { node: NodeRecord; lastKnownGroupId: string | null }[]
}

// loadDashboard composes C.1 + D.1 + E.1 (+ C.2 for liveness) into the static
// model. The reads run concurrently; discovery is best-effort (a failure leaves
// every node "online" rather than tearing down the whole load, since the offline
// strip is informational). Returns the freshest ConfigDoc version for If-Match.
export async function loadDashboard(): Promise<{ model: DashboardModel; version: number }> {
  const [info, nodes, groups, discovery] = await Promise.all([
    getClusterInfo(),
    listNodes(),
    listGroups(),
    getDiscovery().catch(() => ({ members: [], discovered: [] })),
  ])

  const nodeList = nodes.data?.nodes ?? []
  const groupList = groups.data?.groups ?? []
  const nodesById: Record<string, NodeRecord> = {}
  for (const n of nodeList) nodesById[n.id] = n

  // Online set: C.2 is the only endpoint with a per-node liveness bool (D.1
  // NodeRecords are static — P4.10 §9 risk 5). When discovery is empty (no
  // members reported), treat every node as online to avoid a false offline strip.
  const reported = discovery.members ?? []
  const onlineNodeIds = new Set<string>(
    reported.length === 0
      ? nodeList.map((n) => n.id)
      : reported.filter((m) => m.online).map((m) => m.id),
  )

  // Last-known group per node, from the gossiped ConfigDoc membership.
  const groupByNode = new Map<string, string>()
  for (const g of groupList) for (const id of g.memberNodeIds) groupByNode.set(id, g.id)

  const offlineMembers = nodeList
    .filter((n) => !onlineNodeIds.has(n.id))
    .map((node) => ({ node, lastKnownGroupId: groupByNode.get(node.id) ?? null }))

  const version = Math.max(
    info.version ?? 0,
    nodes.version ?? 0,
    groups.version ?? 0,
  )

  return {
    model: {
      clusterName: info.cluster?.name ?? '',
      nodesById,
      groups: groupList,
      onlineNodeIds,
      offlineMembers,
    },
    version,
  }
}

// quickPlay starts a card's group (08 F.3, proxied to master). When the group
// already has a media selection it plays it; the body carries the stored
// {file, loop} so a fresh streamGen starts. Returns the new ConfigDoc version to
// re-seed If-Match. Throws ApiError on 409/502 for the caller's toast/banner.
export async function quickPlay(group: GroupRecord, ifMatch: number): Promise<number> {
  const sel = group.media?.file
  const r = sel
    ? await selectAndPlay(group.id, group.media.file, group.media.loop, ifMatch)
    : await selectAndPlay(group.id, '', false, ifMatch) // server 409s "no media selected"
  return r.data.version
}

// quickStop stops a card's group (08 F.4). Returns the new ConfigDoc version.
export async function quickStop(groupId: string, ifMatch: number): Promise<number> {
  const r = await stop(groupId, ifMatch)
  return r.data.version
}
