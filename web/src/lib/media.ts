// Typed wrappers for the MEDIA surface (08 §F.1–F.4) the Media screen (09 §7)
// drives. Every call routes through P0.4's apiFetch so the §0.4 error envelope
// and §0.5 If-Match/ETag handling stay uniform — screens never call fetch.
//
// The mutating calls (F.2/F.3/F.4) all flip part of ConfigDoc.Groups
// (media selection and/or the Playing bool), so they REQUIRE If-Match and
// resolve to the new ConfigDoc version (from the body's `version` or the ETag)
// for the caller to re-seed the shared configVersion. The serving node proxies
// the file existence check / playback fan-out to the group MASTER (08 §F-table).

import { apiFetch, type Read } from './api'
import type { GroupRecord } from './types'

// MediaFile mirrors one 08 §F.1 file entry. `durationMs` feeds mmss() for the
// row "length" column; the optional metadata is shown when the master's scan
// resolves it (id3 tags / probe). All optional fields degrade to "—".
export interface MediaFile {
  file: string // "song.mp3"
  title?: string
  artist?: string
  durationMs?: number // → mmss() for the row "length" column
  sizeBytes?: number
  sampleRate?: number
}

// MediaListing is the 08 §F.1 response: the resolved node id whose data/ was
// listed (the scoped master), the data/-relative folder listed ("" = root),
// its subdirectories (browse targets) and its mp3 entries (data/-relative
// paths, so a nested file plays unchanged).
export interface MediaListing {
  nodeId: string
  path: string
  dirs: string[]
  files: MediaFile[]
}

// GroupWriteBody is the common F.2/F.3/F.4 success shape: the new ConfigDoc
// version + the mutated GroupRecord (media set and/or playing flipped).
interface GroupWriteBody {
  version?: number
  group?: GroupRecord
}

function buildQuery(nodeId?: string, path?: string): string {
  const sp = new URLSearchParams()
  if (nodeId) sp.set('node', nodeId)
  if (path) sp.set('path', path)
  const q = sp.toString()
  return q ? `?${q}` : ''
}

// listMedia lists one folder of a node's data/ media tree (08 §F.1). `nodeId`
// defaults to the receiving node server-side; the Media screen passes the
// scoped master's id so the listing is proxied to the master whose disk holds
// the files. `path` selects a subfolder ("" = root). Read-only (data/ is not
// part of ConfigDoc) → no If-Match. A 502 means the target node is unreachable
// (offline master → the screen shows the offline state).
export function listMedia(nodeId?: string, path?: string): Promise<Read<MediaListing>> {
  return apiFetch<MediaListing>(`/api/v1/media${buildQuery(nodeId, path)}`)
}

// selectAndPlay selects a file and starts playback in one shot (08 §F.3, body
// {file, loop}); writes Groups[].media and flips Playing=true. If-Match REQUIRED
// (mutates ConfigDoc.Groups). A 409 version_conflict → stale version (reload &
// reapply); a 502 proxy_failed → the master is unreachable.
export async function selectAndPlay(
  groupId: string,
  file: string,
  loop: boolean,
  ifMatch: number,
  // sourceNodeId is the node whose data/ holds the file: the server writes it
  // as the group's MasterHint, so the SOURCE node is elected master and decodes
  // its own file locally (master-follows-source).
  sourceNodeId?: string,
): Promise<Read<{ group?: GroupRecord; version: number }>> {
  const { data, version } = await apiFetch<GroupWriteBody>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/play`,
    { method: 'POST', body: { file, loop, nodeId: sourceNodeId }, ifMatch },
  )
  return { data: { group: data?.group, version: data?.version ?? version ?? ifMatch }, version }
}

// setMedia sets the media selection (+loop) WITHOUT starting playback (08 §F.2).
// Used by the inline loop toggle when the group is stopped (and by the Groups
// screen). If-Match REQUIRED. A 404 names the master when the file is absent.
export async function setMedia(
  groupId: string,
  file: string,
  loop: boolean,
  ifMatch: number,
  // sourceNodeId: see selectAndPlay (master-follows-source).
  sourceNodeId?: string,
): Promise<Read<{ group?: GroupRecord; version: number }>> {
  const { data, version } = await apiFetch<GroupWriteBody>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/media`,
    { method: 'POST', body: { file, loop, nodeId: sourceNodeId }, ifMatch },
  )
  return { data: { group: data?.group, version: data?.version ?? version ?? ifMatch }, version }
}

// stop halts playback, flipping Playing=false (08 §F.4). If-Match REQUIRED.
export async function stop(
  groupId: string,
  ifMatch: number,
): Promise<Read<{ group?: GroupRecord; version: number }>> {
  const { data, version } = await apiFetch<GroupWriteBody>(
    `/api/v1/groups/${encodeURIComponent(groupId)}/stop`,
    { method: 'POST', ifMatch },
  )
  return { data: { group: data?.group, version: data?.version ?? version ?? ifMatch }, version }
}
