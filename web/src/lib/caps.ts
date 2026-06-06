// Effective-caps math for Node detail (09 §6, README §6.5, D16/D12). Pure, no
// I/O, table-tested. The node advertises an EFFECTIVE Capabilities object
// (detected(runtime) ∩ enabled(config)); these helpers reason about what a
// per-node mask edit (CapabilityMask) does to it, mirroring what the server will
// derive after a re-probe/re-mask so the form preview matches the saved result.
//
// NEW (no ../media analogue — mpvsync had no per-node capability model).

import type { Capabilities } from './types'
import type { CapabilityMask } from './node'

export type SinkTier = 'precise' | 'coarse'

// CapsListKind names the four maskable list axes of Capabilities. (`render` is a
// boolean, handled by previewRender / setForceControlOnly, not a list.)
export type CapsListKind = 'sinks' | 'encode' | 'decode' | 'fec'

// sinkTier classifies an output backend precise vs coarse by name (06 §1.1):
//   - `alsa` (direct snd_pcm_delay ioctl) and `pipewire` → precise.
//   - `exec:aplay` / `exec:pw-play` (any `exec:*`) → coarse (Delay() ok=false).
// 09 §6's wireframe tags `pipewire` precise even though 06 §1.1's compiled
// registry lists only alsa(precise)+exec:*(coarse) — we honour the wireframe and
// treat a literal "pipewire" as precise (see P6.3 §9 risk 5). If the caps JSON
// ever carries a per-sink precise flag, prefer it over this name classification.
export function sinkTier(name: string): SinkTier {
  if (name.startsWith('exec:')) return 'coarse'
  if (name === 'alsa' || name === 'pipewire') return 'precise'
  // Unknown backend: be conservative and call it coarse (no delay guarantee).
  return 'coarse'
}

// enabledSet returns the effective-enabled set for one axis after applying the
// draft mask: when the draft carries a mask list for the axis it WINS (it is the
// desired enabled set); otherwise the loaded effective list stands.
export function enabledSet(
  effective: Capabilities,
  draftMask: CapabilityMask | undefined,
  kind: CapsListKind,
): string[] {
  const masked = draftMask?.[kind]
  if (masked !== undefined) return masked
  return (effective[kind] as string[] | undefined) ?? []
}

// probedSet returns every path the runtime DISCOVERED on this node for one axis
// — the superset the toggle rows are drawn from. The advertised effective caps
// only carry the currently-enabled set, so the probed superset is reconstructed
// as (effective ∪ anything the draft has masked off). A path the operator just
// unchecked therefore stays OFFERED (re-checkable) rather than vanishing; a path
// that was never probed is never in this set and so is never offered (D12).
//
// `probed` may also be supplied explicitly on the record (caps.probed) if the
// scaffold ever surfaces the pre-mask superset directly; when present it wins.
export function probedSet(
  effective: Capabilities,
  draftMask: CapabilityMask | undefined,
  kind: CapsListKind,
  explicitProbed?: Partial<Record<CapsListKind, string[]>>,
): string[] {
  const explicit = explicitProbed?.[kind]
  if (explicit !== undefined) return explicit
  const set = new Set<string>((effective[kind] as string[] | undefined) ?? [])
  for (const n of draftMask?.[kind] ?? []) set.add(n)
  return [...set]
}

// probedButDisabled lists paths that were probed but are NOT in the (draft)
// enabled set — the "probed but disabled ↳ re-enable to gain output" rows of the
// 09 §6 wireframe. A never-probed path is, by construction, never reported.
export function probedButDisabled(
  effective: Capabilities,
  draftMask: CapabilityMask | undefined,
  kind: CapsListKind,
  explicitProbed?: Partial<Record<CapsListKind, string[]>>,
): string[] {
  const enabled = new Set(enabledSet(effective, draftMask, kind))
  return probedSet(effective, draftMask, kind, explicitProbed).filter(
    (n) => !enabled.has(n),
  )
}

// previewRender derives the render flag the server WILL advertise for the draft:
// a node renders iff it has at least one enabled sink (06 §1.5). An explicit
// `render:false` in the mask (force control-only) overrides to false regardless
// of sinks; an explicit `render:true` cannot conjure a sink — it still requires
// one enabled sink. So: render = mask.render !== false AND len(enabled sinks)>0.
export function previewRender(
  draftMask: CapabilityMask | undefined,
  effective: Capabilities,
): boolean {
  if (draftMask?.render === false) return false
  return enabledSet(effective, draftMask, 'sinks').length > 0
}
