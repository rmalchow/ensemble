import { describe, it, expect } from 'vitest'
import {
  sinkTier,
  enabledSet,
  probedSet,
  probedButDisabled,
  previewRender,
} from '../caps'
import type { Capabilities } from '../types'
import type { CapabilityMask } from '../node'

function caps(p: Partial<Capabilities> = {}): Capabilities {
  return {
    render: true,
    sinks: ['alsa', 'exec:aplay'],
    encode: ['pcm', 'opus'],
    decode: ['pcm', 'opus'],
    fec: ['none', 'xorParity', 'duplicate'],
    maxRate: 48000,
    ...p,
  }
}

describe('sinkTier (06 §1.1)', () => {
  const cases: [string, 'precise' | 'coarse'][] = [
    ['alsa', 'precise'],
    ['pipewire', 'precise'],
    ['exec:aplay', 'coarse'],
    ['exec:pw-play', 'coarse'],
    ['exec:anything', 'coarse'],
    ['unknown', 'coarse'],
  ]
  it.each(cases)('%s → %s', (name, tier) => {
    expect(sinkTier(name)).toBe(tier)
  })
})

describe('enabledSet', () => {
  it('falls back to the effective list with no mask', () => {
    expect(enabledSet(caps(), undefined, 'sinks')).toEqual(['alsa', 'exec:aplay'])
  })
  it('the mask list wins when present (desired enabled set)', () => {
    const mask: CapabilityMask = { sinks: ['alsa'] }
    expect(enabledSet(caps(), mask, 'sinks')).toEqual(['alsa'])
  })
  it('an empty mask list means nothing enabled', () => {
    expect(enabledSet(caps(), { sinks: [] }, 'sinks')).toEqual([])
  })
})

describe('probedSet / probedButDisabled (D12)', () => {
  it('probed = effective ∪ draft-masked (a just-disabled path stays offered)', () => {
    const mask: CapabilityMask = { sinks: ['alsa'] } // exec:aplay disabled
    const probed = probedSet(caps(), mask, 'sinks').sort()
    expect(probed).toEqual(['alsa', 'exec:aplay'])
  })
  it('a probed path absent from the enabled mask is reported disabled', () => {
    const mask: CapabilityMask = { sinks: ['alsa'] }
    expect(probedButDisabled(caps(), mask, 'sinks')).toEqual(['exec:aplay'])
  })
  it('a never-probed path is not reported (cannot be enabled)', () => {
    // pipewire was never probed (not in effective, not in mask) → not offered.
    const mask: CapabilityMask = { sinks: ['alsa'] }
    expect(probedButDisabled(caps(), mask, 'sinks')).not.toContain('pipewire')
  })
  it('explicit probed superset wins when supplied', () => {
    const explicit = { sinks: ['alsa', 'exec:aplay', 'pipewire'] }
    const dis = probedButDisabled(caps({ sinks: ['alsa'] }), undefined, 'sinks', explicit)
    expect(dis.sort()).toEqual(['exec:aplay', 'pipewire'])
  })
})

describe('previewRender (06 §1.5)', () => {
  it('≥1 enabled sink → render true', () => {
    expect(previewRender(undefined, caps())).toBe(true)
  })
  it('zero enabled sinks → render false', () => {
    expect(previewRender({ sinks: [] }, caps())).toBe(false)
  })
  it('explicit render:false forces false even with sinks', () => {
    expect(previewRender({ render: false, sinks: ['alsa'] }, caps())).toBe(false)
  })
  it('explicit render:true still needs a sink (cannot conjure one)', () => {
    expect(previewRender({ render: true, sinks: [] }, caps())).toBe(false)
  })
})
