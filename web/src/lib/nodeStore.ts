// Per-screen editing model for Node detail (09 §6). Pattern lifted from
// clusterStore.ts / stores.ts (the ../media derived-store split: components read
// derived projections, never the raw snapshot) but adds the draft/dirty EDITING
// layer the read-only media stores never had.
//
// loaded  = server truth (last getNode / live snapshot).
// draft   = the pending partial edit (a NodePatch).
// isDirty = draft differs from loaded.
// Components read the derived stores and mutate ONLY through the action fns
// (setField / toggleCapability / setForceControlOnly / revert / save), so the
// A↔B render flip, the capability mask, and the If-Match save stay in one place.

import { derived, get, writable, type Readable } from 'svelte/store'
import { configVersion } from './stores'
import { groupStatus } from './groupStatus'
import { getNode, patchNode, type NodeDetailView, type NodePatch } from './node'
import { previewRender } from './caps'

// ---- Raw internal writables ------------------------------------------------

const loadedStore = writable<NodeDetailView | null>(null)
const draftStore = writable<NodePatch>({})

// ---- Public derived views --------------------------------------------------

export const loaded: Readable<NodeDetailView | null> = derived(
  loadedStore,
  ($l) => $l,
)

export const draft: Readable<NodePatch> = derived(draftStore, ($d) => $d)

// isDirty is true once the draft carries any field that differs from loaded. An
// empty draft (or a draft whose every field equals loaded) is clean.
export const isDirty: Readable<boolean> = derived(
  [loadedStore, draftStore],
  ([$l, $d]) => draftDiffers($l, $d),
)

// isRenderEnabled drives the A↔B audio-output flip LIVE, pre-save: it previews
// what the server will derive from the draft mask (render = no forced
// control-only AND ≥1 enabled sink, 06 §1.5) and otherwise falls back to the
// loaded effective render flag.
export const isRenderEnabled: Readable<boolean> = derived(
  [loadedStore, draftStore],
  ([$l, $d]) => {
    if (!$l) return false
    // Only let the mask drive the preview once the operator has touched caps;
    // an untouched draft simply reflects the loaded effective render flag.
    if ($d.capabilities === undefined) return $l.caps.render
    return previewRender($d.capabilities, $l.caps)
  },
)

// liveSyncErrorUs selects THIS node's members[].syncErrorUs from its current
// group's live status (P3.3 groupStatus). The master is the reference → null;
// an offline / no-group node → null; a node with no live sample yet → null.
export const liveSyncErrorUs: Readable<number | null> = derived(
  [loadedStore, groupStatus],
  ([$l, $status]) => {
    if (!$l || !$l.groupId || $l.online === false) return null
    const st = $status.get($l.groupId)
    if (!st) return null
    if (st.masterNodeId === $l.id) return null // the reference
    const m = st.members.find((x) => x.nodeId === $l.id)
    if (!m || !m.online) return null
    return m.syncErrorUs
  },
)

// offline mirrors the loaded record's liveness into the read-only gate (§5.5):
// HWDelayUs/channel/gain, the calibration helper, and the capability toggles are
// disabled when offline (they need the live node to re-probe). Rename is still
// allowed by the screen, labelled "applies when node returns".
export const offline: Readable<boolean> = derived(
  loadedStore,
  ($l) => $l?.online === false,
)

// ---- Loaders ---------------------------------------------------------------

// setLoaded seeds the server-truth record and clears the draft. The screen calls
// this on mount (from getNode or the live snapshot) and after each successful
// save / 409 reload so editing always starts from fresh truth.
export function setLoaded(node: NodeDetailView): void {
  loadedStore.set(node)
  draftStore.set({})
}

// mergeLoaded updates the server-truth record WITHOUT clearing the draft — used
// when a background liveness/group snapshot enriches the record while the
// operator may already be mid-edit (the draft is preserved on top).
export function mergeLoaded(node: NodeDetailView): void {
  loadedStore.set(node)
}

// loadNode fetches node {id} (D.2) into loaded + clears the draft, and refreshes
// the shared configVersion from the read's ETag so the next save's If-Match is
// current. Throws (ApiError) on not-found etc. — the screen renders its error.
export async function loadNode(id: string): Promise<void> {
  const { version, node } = await getNode(id)
  loadedStore.set(node)
  draftStore.set({})
  configVersion.set(version)
}

// ---- Edit actions ----------------------------------------------------------

// setField stages one identity/audio field edit. Setting a value back to the
// loaded truth leaves the field staged but isDirty re-computes false via the
// value compare, so Revert/Save reflect the real diff.
export function setField<K extends keyof NodePatch>(k: K, v: NodePatch[K]): void {
  draftStore.update((d) => ({ ...d, [k]: v }))
}

// toggleCapability adds/removes a probed path from the draft's enabled set for an
// axis. The mask is seeded from the loaded effective set on first touch (so the
// server receives the FULL desired enabled set, not just the delta). Re-enabling
// a path restores it; disabling the last sink will flip render off via
// previewRender (isRenderEnabled).
export function toggleCapability(
  kind: 'sinks' | 'encode' | 'decode' | 'fec',
  name: string,
  enabled: boolean,
): void {
  const l = get(loadedStore)
  if (!l) return
  draftStore.update((d) => {
    const caps = { ...(d.capabilities ?? {}) }
    const current = caps[kind] ?? ((l.caps[kind] as string[] | undefined) ?? [])
    const set = new Set(current)
    if (enabled) set.add(name)
    else set.delete(name)
    caps[kind] = [...set]
    return { ...d, capabilities: caps }
  })
}

// setForceControlOnly sets draft.capabilities.render = !on. Checking it forces
// the node sink-less (render:false, variant B); clearing it lets previewRender
// restore rendering when a probed sink is enabled. Other mask fields are kept.
export function setForceControlOnly(on: boolean): void {
  draftStore.update((d) => ({
    ...d,
    capabilities: { ...(d.capabilities ?? {}), render: !on },
  }))
}

// revert drops every pending edit back to the loaded server truth.
export function revert(): void {
  draftStore.set({})
}

// save PATCHes the minimal dirty draft with the current configVersion as
// If-Match (D.3), folds the returned version into the shared store + re-seeds
// loaded from the server's updated record, and clears the draft. Returns the new
// version. Throws ApiError on 409 version_conflict (screen reloads + reapplies),
// 502 proxy_failed (owner offline), 422 unprocessable, etc.
export async function save(): Promise<number> {
  const l = get(loadedStore)
  if (!l) throw new Error('save() with no loaded node')
  const v = get(configVersion)
  if (v === undefined) throw new Error('save() with unknown configVersion')
  const patch = minimalPatch(l, get(draftStore))
  const { version, node } = await patchNode(l.id, patch, v)
  loadedStore.set(node)
  draftStore.set({})
  configVersion.set(version)
  return version
}

// ---- Pure diff helpers (exported for tests) --------------------------------

// minimalPatch returns only the draft fields that actually differ from loaded,
// so a PATCH never sends an unchanged field (D.3 partial update).
export function minimalPatch(
  loadedNode: NodeDetailView,
  d: NodePatch,
): NodePatch {
  const out: NodePatch = {}
  if (d.name !== undefined && d.name !== loadedNode.name) out.name = d.name
  if (d.channel !== undefined && d.channel !== loadedNode.channel)
    out.channel = d.channel
  if (d.gainDb !== undefined && d.gainDb !== loadedNode.gainDb)
    out.gainDb = d.gainDb
  if (d.hwDelayUs !== undefined && d.hwDelayUs !== loadedNode.hwDelayUs)
    out.hwDelayUs = d.hwDelayUs
  if (d.device !== undefined && d.device !== (loadedNode.device ?? ''))
    out.device = d.device
  if (d.capabilities !== undefined && capsMaskDiffers(loadedNode, d.capabilities))
    out.capabilities = d.capabilities
  return out
}

function draftDiffers(l: NodeDetailView | null, d: NodePatch): boolean {
  if (!l) return false
  return Object.keys(minimalPatch(l, d)).length > 0
}

// capsMaskDiffers tells whether a staged mask actually changes the loaded
// effective caps (so an untouched-but-seeded mask doesn't mark the form dirty).
function capsMaskDiffers(l: NodeDetailView, mask: NonNullable<NodePatch['capabilities']>): boolean {
  if (mask.render !== undefined && mask.render !== l.caps.render) return true
  const axes: ('sinks' | 'encode' | 'decode' | 'fec')[] = [
    'sinks',
    'encode',
    'decode',
    'fec',
  ]
  for (const k of axes) {
    const m = mask[k]
    if (m === undefined) continue
    if (!sameSet(m, (l.caps[k] as string[] | undefined) ?? [])) return true
  }
  return false
}

function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const sb = new Set(b)
  return a.every((x) => sb.has(x))
}

// __resetForTest clears both stores between unit tests (the stores are module
// singletons). Not used outside tests.
export function __resetForTest(): void {
  loadedStore.set(null)
  draftStore.set({})
}
