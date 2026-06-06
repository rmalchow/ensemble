// Typed API wrappers + view types for Node detail (screen 6, 09 §6) over the 08
// §D.2/§D.3/§F2.1 surface. Pattern lifted from ../media web/src/lib/api.ts (thin
// fetch → throw on non-2xx → typed body) but every mpvsync endpoint
// (/api/devices/…, send, upload, audio, timing) is dropped and re-targeted to
// the canonical /api/v1/nodes/{id} + /api/v1/calibrate/play paths, routed through
// the shared apiFetch so the §0.4 error envelope and §0.5 If-Match/ETag handling
// stay uniform. Components NEVER call fetch directly — they go through these.

import { apiFetch, ApiError } from './api'
import type { Capabilities, Channel } from './types'

export { ApiError }
export type { Capabilities }

// CapsListKind re-exported so component prop contracts can name an axis without
// importing caps.ts (which depends on this module).
export type CapsListKind = 'sinks' | 'encode' | 'decode' | 'fec'

// ---- View types (mirror 08 §0.7 NodeRecord + README §6.5 Capabilities) -----

// NodeDetailView is the full node record screen 6 renders. The structural facts
// (id/name/addrs/channel/gain/hwDelayUs/caps) are the ConfigDoc Nodes[]
// projection (D.2); online/groupId/isMaster/cert fields are joined from gossip
// liveness + the cluster/group views for the header + identity panel.
export interface NodeDetailView {
  id: string
  name: string
  addrs: string[] // drive the allowlist (README §6.5)
  hwDelayUs: number // integer µs; "Hardware delay (HWDelayUs)" (09 §6 naming note)
  channel: Channel // 'stereo' | 'left' | 'right' (D13)
  gainDb: number
  device?: string // persisted audio-output device override ('' = auto)
  // audioDevices is the node's self-probed playback device list (the selectable
  // choices for `device`), gossiped from the owning node.
  audioDevices?: { id: string; label?: string }[]
  caps: Capabilities // EFFECTIVE = detected(runtime) ∩ enabled(config) (D16)
  // probed (optional) is the pre-mask runtime-discovered superset, if the record
  // ever surfaces it directly; absent, caps.ts reconstructs it from effective ∪
  // draft-masked. Lets the toggle rows offer a probed-but-disabled path.
  probed?: Partial<Record<CapsListKind, string[]>>
  fingerprint?: string // node cert fingerprint (Network panel + header)
  certSignedByCa?: boolean // header cert status
  online?: boolean // gossip/health (drives offline read-only mode)
  groupId?: string // current group (Identity + live-sync selector)
  isMaster?: boolean // elected master of its group (master marker)
}

// NodePatch is the PATCH body for /api/v1/nodes/{id} (D.3). All fields optional
// (partial update — only changed fields are sent). `capabilities` is the per-node
// MASK that re-shapes effective caps (07; D16): the caller sends the desired
// ENABLED set and the node re-probes/re-masks and re-advertises.
export interface NodePatch {
  name?: string
  channel?: Channel
  gainDb?: number
  hwDelayUs?: number
  device?: string // '' clears back to auto
  capabilities?: CapabilityMask
}

// CapabilityMask is the per-node enable/disable mask (08 §D.3 "capabilities?";
// 07 per-node config). It mirrors the effective Capabilities so the server
// intersects probed ∩ this. `render:false` forces control-only (sink-less); the
// list fields are the desired enabled subsets of the probed paths.
export interface CapabilityMask {
  render?: boolean
  sinks?: string[]
  encode?: string[]
  decode?: string[]
  fec?: string[]
}

// ---- Reads -----------------------------------------------------------------

// getNode reads one node's full record (D.2). Read-only — no If-Match; the ETag
// (ConfigDoc.Version) is folded into `version` to seed a follow-up PATCH's
// If-Match without a second read.
export async function getNode(
  id: string,
): Promise<{ version: number; node: NodeDetailView }> {
  const { data, version } = await apiFetch<{ version: number; node: NodeDetailView }>(
    `/api/v1/nodes/${encodeURIComponent(id)}`,
  )
  return { version: data?.version ?? version ?? 0, node: data.node }
}

// ---- Writes (require If-Match: <version>) ----------------------------------

// patchNode applies a partial update to a node (D.3). Proxied to the owning node
// (channel/HWDelayUs/gain hit its live renderer; capability edits change what it
// probes/masks). If-Match REQUIRED (mutates ConfigDoc.Nodes); a stale version →
// 409 version_conflict (the screen reloads + reapplies, never silently
// overwrites). An unreachable owner → 502 proxy_failed.
export async function patchNode(
  id: string,
  patch: NodePatch,
  ifMatch: number,
): Promise<{ version: number; node: NodeDetailView }> {
  const { data, version } = await apiFetch<{ version: number; node: NodeDetailView }>(
    `/api/v1/nodes/${encodeURIComponent(id)}`,
    { method: 'PATCH', body: patch, ifMatch },
  )
  return { version: data?.version ?? version ?? ifMatch, node: data.node }
}

// ---- Calibration (transient; NO If-Match — does not write the ConfigDoc) ---

// CalibratePlayReq triggers the built-in click+tone signal (08 §F2.1, A.10b).
// Exactly one of groupId / nodeIds must be set (the server 400s neither/both).
export interface CalibratePlayReq {
  groupId?: string
  nodeIds?: string[]
  durationSec: number
}

export interface CalibratePlayResp {
  playedOn: string[]
  durationSec: number
  warnings: string[]
}

// calibratePlay plays the synchronous built-in calibration signal (F2.1). It is
// transient playback — NO If-Match. We mirror the server's 400 guard client-side
// (reject neither/both of groupId/nodeIds) so a malformed request never leaves
// the browser. The signal itself (1 s: ~1 ms click + ~200 ms 1 kHz tone +
// silence) is generated in-process server-side; the UI only triggers it.
export async function calibratePlay(
  req: CalibratePlayReq,
): Promise<CalibratePlayResp> {
  const hasGroup = req.groupId !== undefined && req.groupId !== ''
  const hasNodes = req.nodeIds !== undefined && req.nodeIds.length > 0
  if (hasGroup === hasNodes) {
    throw new ApiError(
      400,
      'invalid_request',
      'calibrate/play needs exactly one of groupId or nodeIds',
    )
  }
  const { data } = await apiFetch<CalibratePlayResp>('/api/v1/calibrate/play', {
    method: 'POST',
    body: req,
  })
  return {
    playedOn: data?.playedOn ?? [],
    durationSec: data?.durationSec ?? req.durationSec,
    warnings: data?.warnings ?? [],
  }
}

// CALIBRATE_DEFAULT_SEC is the 09 §6 wireframe default playback length (10 s). A
// UI default only — NOT an A.12 tunable.
export const CALIBRATE_DEFAULT_SEC = 10
