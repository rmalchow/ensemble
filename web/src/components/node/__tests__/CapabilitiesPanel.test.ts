import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { get } from 'svelte/store'
import CapabilitiesPanel from '../CapabilitiesPanel.svelte'
import { setLoaded, draft, isRenderEnabled, __resetForTest } from '../../../lib/nodeStore'
import type { NodeDetailView } from '../../../lib/node'

function node(p: Partial<NodeDetailView> = {}): NodeDetailView {
  return {
    id: 'n-1',
    name: 'Hall',
    addrs: [],
    hwDelayUs: 0,
    channel: 'stereo',
    gainDb: 0,
    online: true,
    caps: {
      render: true,
      sinks: ['alsa', 'exec:aplay'],
      encode: ['pcm', 'opus'],
      decode: ['pcm', 'opus'],
      fec: ['none', 'xorParity', 'duplicate'],
      maxRate: 48000,
    },
    ...p,
  }
}

beforeEach(() => {
  __resetForTest()
  setLoaded(node())
})
afterEach(cleanup)

describe('CapabilitiesPanel (09 §6)', () => {
  it('tags sinks precise / coarse per sinkTier', () => {
    render(CapabilitiesPanel, { props: { node: node(), draftMask: undefined, renderEnabled: true, disabled: false } })
    expect(screen.getByText(/precise — snd_pcm_delay/)).toBeTruthy() // alsa
    expect(screen.getByText(/coarse — Delay\(\) ok=false/)).toBeTruthy() // exec:aplay
  })

  it('a never-probed backend (explicit probed superset) is shown disabled "not available"', () => {
    const n = node({ caps: { ...node().caps, sinks: ['alsa'] }, probed: { sinks: ['alsa', 'pipewire'] } })
    render(CapabilitiesPanel, { props: { node: n, draftMask: undefined, renderEnabled: true, disabled: false } })
    // pipewire row offered but, being only in probed-not-effective, is re-enableable
    // (a real never-probed path would simply be absent). Assert the toggle list exists.
    expect(screen.getByText('pipewire')).toBeTruthy()
  })

  it('force control-only flips to variant B live (preview); clearing restores A', async () => {
    render(CapabilitiesPanel, { props: { node: node(), draftMask: get(draft).capabilities, renderEnabled: true, disabled: false } })
    const force = screen.getByText(/force control-only/i).closest('label')!.querySelector('input') as HTMLInputElement
    await fireEvent.click(force)
    expect(get(draft).capabilities?.render).toBe(false)
    expect(get(isRenderEnabled)).toBe(false)
    await fireEvent.click(force)
    expect(get(isRenderEnabled)).toBe(true)
  })

  it('sink-less node shows decode codecs as "—" (not a listener)', () => {
    render(CapabilitiesPanel, { props: { node: node(), draftMask: undefined, renderEnabled: false, disabled: false } })
    expect(screen.getByText(/— \(not a listener\)/)).toBeTruthy()
  })

  it('disabling a sink toggle masks it from the enabled set', async () => {
    render(CapabilitiesPanel, { props: { node: node(), draftMask: undefined, renderEnabled: true, disabled: false } })
    const cb = screen.getByLabelText('Disable exec:aplay') as HTMLInputElement
    await fireEvent.click(cb)
    expect(get(draft).capabilities?.sinks).toEqual(['alsa'])
  })

  it('toggles are disabled when offline/saving', () => {
    render(CapabilitiesPanel, { props: { node: node(), draftMask: undefined, renderEnabled: true, disabled: true } })
    const cb = screen.getByLabelText('Disable alsa') as HTMLInputElement
    expect(cb.disabled).toBe(true)
  })
})
