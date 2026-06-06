// REST wrappers for the group-mutating calls the Groups + Dashboard screens
// drive (08 §E.2/§E.4/§F.2/§F.3/§F.4). Every call routes through P0.4's
// apiFetch so the §0.4 error envelope and §0.5 If-Match/ETag handling stay
// uniform; components NEVER call fetch directly (the "screens never touch the
// socket" discipline from ../media ws.ts/stores.ts, applied to REST here).
//
// All mutating calls REQUIRE If-Match: <configVersion> (08 §0.5). Each resolves
// to the NEW ConfigDoc version (from the body's `version` or the ETag) so the
// caller can refresh the shared configVersion seed; on failure they throw
// ApiError{code,message,status} for ErrorBanner / toast handling.

import { apiFetch, ApiError } from './api'
import type { GroupRecord, Profile } from './types'

export { ApiError }

// GroupPatch is the partial body for PATCH /api/v1/groups/{id} (08 §E.4). All
// fields optional; `profile` is a partial profile override (08 §E.4 body).
export interface GroupPatch {
  name?: string
  memberNodeIds?: string[]
  profile?: Partial<Profile>
  playing?: boolean
}

// GroupWriteResult is the common {version, group} success shape (08 §E.2/§E.4/
// §F.2/§F.3/§F.4). `group` is absent for create-only callers that want the
// version; `version` is always resolved.
export interface GroupWriteResult {
  version: number
  group?: GroupRecord
}

interface VersionGroupBody {
  version?: number
  group?: GroupRecord
}

// resolve folds the body's `version` and the ETag into one number, preferring
// the body (08 returns both); falls back to the supplied If-Match seed so a
// 204/empty body never yields NaN.
function resolve(
  data: VersionGroupBody | undefined,
  etag: number | undefined,
  seed: number,
): GroupWriteResult {
  return { version: data?.version ?? etag ?? seed, group: data?.group }
}

// createGroup creates an empty (or pre-populated) group (08 §E.2). 201 on
// success; the caller seeds members via a follow-up moveNode/patch or passes
// them here. If-Match REQUIRED.
export async function createGroup(
  name: string,
  ifMatch: number,
  memberNodeIds: string[] = [],
): Promise<GroupWriteResult> {
  const { data, version } = await apiFetch<VersionGroupBody>('/api/v1/groups', {
    method: 'POST',
    body: { name, memberNodeIds, playing: false },
    ifMatch,
  })
  return resolve(data, version, ifMatch)
}

// patchGroup applies a partial edit: rename, membership, profile override, or
// the playing flag (08 §E.4). If-Match REQUIRED. A 422 (infeasible profile /
// member in another group) or 409 (version conflict) throws ApiError.
export async function patchGroup(
  id: string,
  patch: GroupPatch,
  ifMatch: number,
): Promise<GroupWriteResult> {
  const { data, version } = await apiFetch<VersionGroupBody>(
    `/api/v1/groups/${encodeURIComponent(id)}`,
    { method: 'PATCH', body: patch, ifMatch },
  )
  return resolve(data, version, ifMatch)
}

// deleteGroup removes a group; members fall back to solo groups (08 §E.5).
// If-Match REQUIRED.
export async function deleteGroup(
  id: string,
  ifMatch: number,
): Promise<{ version: number; freedNodeIds: string[] }> {
  const { data, version } = await apiFetch<{
    version?: number
    freedNodeIds?: string[]
  }>(`/api/v1/groups/${encodeURIComponent(id)}`, { method: 'DELETE', ifMatch })
  return {
    version: data?.version ?? version ?? ifMatch,
    freedNodeIds: data?.freedNodeIds ?? [],
  }
}

// selectMedia sets the group's media file + loop (08 §F.2). The file must exist
// on the master (a 404 names the node). If-Match REQUIRED.
export async function selectMedia(
  id: string,
  file: string,
  loop: boolean,
  ifMatch: number,
): Promise<GroupWriteResult> {
  const { data, version } = await apiFetch<VersionGroupBody>(
    `/api/v1/groups/${encodeURIComponent(id)}/media`,
    { method: 'POST', body: { file, loop }, ifMatch },
  )
  return resolve(data, version, ifMatch)
}

// playGroup starts/resumes playback, flipping GroupRecord.playing=true and
// fanning out to the master (08 §F.3). An optional media selection plays in one
// shot. A 409 conflict ("no media selected") or 502 (master unreachable) throws.
// If-Match REQUIRED (flips the replicated playing bool).
export async function playGroup(
  id: string,
  ifMatch: number,
  sel?: { file: string; loop: boolean },
): Promise<GroupWriteResult> {
  const { data, version } = await apiFetch<VersionGroupBody>(
    `/api/v1/groups/${encodeURIComponent(id)}/play`,
    { method: 'POST', body: sel, ifMatch },
  )
  return resolve(data, version, ifMatch)
}

// stopGroup halts playback, flipping GroupRecord.playing=false (08 §F.4).
// If-Match REQUIRED.
export async function stopGroup(
  id: string,
  ifMatch: number,
): Promise<GroupWriteResult> {
  const { data, version } = await apiFetch<VersionGroupBody>(
    `/api/v1/groups/${encodeURIComponent(id)}/stop`,
    { method: 'POST', ifMatch },
  )
  return resolve(data, version, ifMatch)
}

// moveNode transfers a node out of `fromGroupId` and into `toGroupId`. A node is
// in exactly one group (README §2), so a move rewrites memberNodeIds[] on BOTH
// groups. 08 has no atomic two-group endpoint, so this is two sequential PATCHes
// against the SAME config version chain: remove from source, then add to target.
// The first PATCH bumps the version; the second uses the returned version. On a
// mid-sequence conflict the caller reloads + reapplies (09 §0). Returns the final
// version. `fromMembers`/`toMembers` are the current member arrays (the screen
// already holds them from the snapshot).
export async function moveNode(
  nodeId: string,
  fromGroupId: string,
  toGroupId: string,
  fromMembers: string[],
  toMembers: string[],
  ifMatch: number,
): Promise<number> {
  const nextFrom = fromMembers.filter((id) => id !== nodeId)
  const nextTo = toMembers.includes(nodeId) ? toMembers : [...toMembers, nodeId]
  const a = await patchGroup(fromGroupId, { memberNodeIds: nextFrom }, ifMatch)
  const b = await patchGroup(toGroupId, { memberNodeIds: nextTo }, a.version)
  return b.version
}
